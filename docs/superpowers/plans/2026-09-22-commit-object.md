# Commit Object Type Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add CAS object type 5, `Commit`: the analogue of a git commit, encoded as deterministic CBOR, followed by every object-graph walk, verified by packstore, usable from the CLI, and documented.

**Architecture:** A new leaf package `commit` owns the record, its validation and its strict canonical codec, mirroring the `reference` package. `fstree.ChildKeys` gains one case returning the tree then the parents; reachability, completeness and the gc mark all dispatch through it and need no other change. The CLI peels a commit root to its tree and gains `commit create` / `commit show`.

**Tech Stack:** Go 1.26, `github.com/fxamacker/cbor/v2` (already a dependency), `github.com/urfave/cli/v2`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-22-commit-object-design.md`

## Global Constraints

- The type number is **5**; types 6–15 stay reserved.
- Encoding is RFC 8949 §4.2 core-deterministic CBOR, integer map keys, exactly the key numbers in the spec's record tables. Decoding is strict: input must be byte-for-byte what the encoder produces.
- The commit key's length field is the commit's own serialized byte length.
- Bounds: at most 256 parents, no duplicates; identity name 1–1024 bytes, email 0–1024 bytes, no control characters; tz offset −1439..1439 minutes; message at most 1 MiB of valid UTF-8; signature at most 64 KiB; public key at most 16 KiB.
- The tree must be a `DirLeaf` or `DirNode` key; every parent must be a `Commit` key.
- Package `commit` imports only `key`, the standard library and fxamacker/cbor. It must never import `fstree` (which imports it).
- Work on branch `commit-object`. Commit locally per task; do not push.
- Commit message style is `pkg: summary` (see `git log`). End every commit message with:

  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01EMBcgqyJYKqN2hB6jfjbjW
  ```

## File Structure

| File | Responsibility |
| --- | --- |
| `key/type.go`, `key/key.go`, `key/errors.go` | type 5 defined, valid, named; comments updated |
| `commit/commit.go` (new) | `Identity`, `Commit`, bounds, validation, `Encode`, `Object`, `SignaturePayload`, `Decode` |
| `commit/commit_test.go` (new) | round trip, hand-assembled wire bytes, golden vector, validation table, strict-decoding rejections |
| `fstree/children.go` | the `key.Commit` case of `ChildKeys` |
| `fstree/commit_test.go` (new) | `ChildKeys`, `ReachableKeys`, `CheckComplete` across a history with a merge |
| `packstore/verify.go` | `Commit` joins the length-checked types |
| `gc/commit_test.go` (new) | history stays live through a cycle; dropping the branch reclaims it |
| `cmd/amber-store/spec.go` | `descend` peels a commit root to its tree |
| `cmd/amber-store/commit.go` (new) | `commit create`, `commit show`, `parseIdentity`, `renderCommit` |
| `cmd/amber-store/commit_test.go` (new) | `parseIdentity`, `renderCommit` unit tests |
| `cmd/amber-store/e2e_test.go` | `TestE2E_Commit` |
| `architecture/commits.md` (new), `architecture/types.md`, `architecture/references.md`, `README.md`, `fstree/object.go` | docs |

---

### Task 1: `key.Commit` type

**Files:**
- Modify: `key/type.go`, `key/key.go:59-61`, `key/errors.go:12`
- Test: `key/type_test.go`, `key/key_test.go:114-121` and `:216-224`

**Interfaces:**
- Produces: `key.Commit` (`key.Type`, value 5); `key.Type(5).IsValid() == true`; `key.Commit.String() == "Commit"`.

- [x] **Step 1: Update the tests to the new contract**

In `key/type_test.go` replace both test functions:

```go
func TestType_IsValid(t *testing.T) {
	for ty := Type(0); ty <= 5; ty++ {
		if !ty.IsValid() {
			t.Errorf("Type(%d) should be valid", uint8(ty))
		}
	}
	for _, ty := range []Type{6, 7, 15, 16, 255} {
		if ty.IsValid() {
			t.Errorf("Type(%d) should be invalid", uint8(ty))
		}
	}
}

func TestType_String(t *testing.T) {
	cases := map[Type]string{
		Blob:     "Blob",
		FileNode: "FileNode",
		DirLeaf:  "DirLeaf",
		DirNode:  "DirNode",
		XattrSet: "XattrSet",
		Commit:   "Commit",
		Type(7):  "Type(7)",
	}
	for ty, want := range cases {
		if got := ty.String(); got != want {
			t.Errorf("Type(%d).String() = %q, want %q", uint8(ty), got, want)
		}
	}
}
```

In `key/key_test.go`, `TestNewFromHash_ReservedType`: change the list `[]Type{5, 15, 16, 255}` to `[]Type{6, 15, 16, 255}`. In `TestValidate_ReservedType`: change `k[0] = 5 << 4 // type 5, lengthSize 1` to `k[0] = 6 << 4 // type 6, lengthSize 1`. Append:

```go
func TestNewFromHash_Commit(t *testing.T) {
	var full [32]byte
	k, err := NewFromHash(Commit, 100, full)
	if err != nil {
		t.Fatal(err)
	}
	if k[0] != 0x50 {
		t.Errorf("header byte = %#x, want 0x50 (type 5, one length byte)", k[0])
	}
	if k.Type() != Commit || k.Length() != 100 {
		t.Errorf("Type() = %v, Length() = %d; want Commit, 100", k.Type(), k.Length())
	}
	if err := k.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./key`
Expected: build failure, `undefined: Commit`.

- [x] **Step 3: Implement**

`key/type.go`: add to the const block after `XattrSet`:

```go
	Commit   Type = 5 // snapshot record: tree, parent commits, author, committer, message
```

Replace `IsValid` and its comment:

```go
// IsValid reports whether t is a defined CAS object type (0..5). Types 6..15 are
// reserved and must not be emitted; values above 15 do not fit the 4-bit field.
func (t Type) IsValid() bool {
	return t <= Commit
}
```

Add to `String` before `default`:

```go
	case Commit:
		return "Commit"
```

`key/key.go`: in the `Validate` comment change `is defined (0..4)` to `is defined (0..5)`. In the `NewFromHash` comment change `for Blob/XattrSet it is the serialized byte length` to `for Blob/XattrSet/Commit it is the serialized byte length`.
`key/errors.go`: change `reserved (5..15)` to `reserved (6..15)`.

- [x] **Step 4: Run to verify pass**

Run: `go test ./key`
Expected: `ok`.

- [x] **Step 5: Commit**

```bash
git add key
git commit -m "key: define Commit as object type 5"
```

---

### Task 2: `commit` package

**Files:**
- Create: `commit/commit.go`, `commit/commit_test.go`

**Interfaces:**
- Consumes: `key.Commit`, `key.DirLeaf`, `key.DirNode`, `key.Key.Validate`, `key.Parse`, `key.New`.
- Produces:

```go
const MaxParents, MaxIdentityLen, MaxMessageLen, MaxSignatureLen, MaxPublicKeyLen, MaxTZOffset
type Identity struct { Name, Email string; When int64; TZOffset int }
type Commit struct {
	Tree key.Key; Parents []key.Key; Author, Committer Identity
	Message string; Signature, PublicKey []byte
}
func (c Commit) Encode() ([]byte, error)
func (c Commit) Object() (key.Key, []byte, error)
func (c Commit) SignaturePayload() ([]byte, error)
func Decode(b []byte) (Commit, error)
```

- [x] **Step 1: Write the failing tests**

Create `commit/commit_test.go`:

```go
package commit_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/key"
)

const (
	annWhen = int64(1_700_000_000_000_000_000)
	bobWhen = annWhen + 1
)

var (
	ann = commit.Identity{Name: "Ann", Email: "ann@example.com", When: annWhen, TZOffset: 120}
	bob = commit.Identity{Name: "Bob", Email: "", When: bobWhen, TZOffset: -300}
)

// emptyDir is the key of the empty directory: a DirLeaf whose body is the
// empty CBOR array 0x80, length field 1.
func emptyDir(t *testing.T) key.Key {
	t.Helper()
	k, err := key.New(key.DirLeaf, 1, []byte{0x80})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// root returns a parentless commit of the empty directory and its key.
func root(t *testing.T, msg string) (key.Key, commit.Commit) {
	t.Helper()
	c := commit.Commit{Tree: emptyDir(t), Author: ann, Committer: ann, Message: msg}
	k, _, err := c.Object()
	if err != nil {
		t.Fatal(err)
	}
	return k, c
}

// merge is the fixed vector commit: parents root("a") and root("b").
func merge(t *testing.T) commit.Commit {
	t.Helper()
	ka, _ := root(t, "a")
	kb, _ := root(t, "b")
	return commit.Commit{
		Tree:      emptyDir(t),
		Parents:   []key.Key{ka, kb},
		Author:    ann,
		Committer: bob,
		Message:   "merge\n",
	}
}

// Hand-rolled CBOR, independent of the library under test.
func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func bstr32(k key.Key) []byte   { return append([]byte{0x58, 0x20}, k[:]...) }
func u64(n int64) []byte        { return binary.BigEndian.AppendUint64([]byte{0x1b}, uint64(n)) }
func tstr(s string) []byte {
	if len(s) >= 24 {
		panic("tstr: short strings only")
	}
	return append([]byte{0x60 | byte(len(s))}, s...)
}

// handMerge assembles merge(t)'s canonical bytes from the spec's tables.
func handMerge(t *testing.T) []byte {
	t.Helper()
	ka, _ := root(t, "a")
	kb, _ := root(t, "b")
	return cat(
		[]byte{0xa5}, // map(5)
		[]byte{0x00}, bstr32(emptyDir(t)),
		[]byte{0x01, 0x82}, bstr32(ka), bstr32(kb), // array(2)
		[]byte{0x02, 0xa4, 0x00}, tstr("Ann"), []byte{0x01}, tstr("ann@example.com"),
		[]byte{0x02}, u64(annWhen), []byte{0x03, 0x18, 0x78}, // +120
		[]byte{0x03, 0xa4, 0x00}, tstr("Bob"), []byte{0x01}, tstr(""),
		[]byte{0x02}, u64(bobWhen), []byte{0x03, 0x39, 0x01, 0x2b}, // -300
		[]byte{0x04}, tstr("merge\n"),
	)
}

// replaceOnce swaps the single occurrence of old in b.
func replaceOnce(t *testing.T, b, old, new []byte) []byte {
	t.Helper()
	if n := bytes.Count(b, old); n != 1 {
		t.Fatalf("pattern % x occurs %d times, want 1", old, n)
	}
	return bytes.Replace(b, old, new, 1)
}

func TestEncodeMatchesHandAssembledBytes(t *testing.T) {
	got, err := merge(t).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if want := handMerge(t); !bytes.Equal(got, want) {
		t.Fatalf("encoding differs from the spec\n got: %x\nwant: %x", got, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	signed := merge(t)
	signed.Signature = []byte("sig")
	signed.PublicKey = []byte("pub")
	_, rootCommit := root(t, "")
	for name, c := range map[string]commit.Commit{"merge": merge(t), "signed": signed, "root, empty message": rootCommit} {
		b, err := c.Encode()
		if err != nil {
			t.Fatalf("%s: Encode: %v", name, err)
		}
		got, err := commit.Decode(b)
		if err != nil {
			t.Fatalf("%s: Decode: %v", name, err)
		}
		if !reflect.DeepEqual(got, c) {
			t.Errorf("%s: round trip\n got: %+v\nwant: %+v", name, got, c)
		}
	}
}

func TestObjectKey(t *testing.T) {
	k, b, err := merge(t).Object()
	if err != nil {
		t.Fatal(err)
	}
	if k.Type() != key.Commit {
		t.Errorf("key type = %v, want Commit", k.Type())
	}
	if k.Length() != uint64(len(b)) {
		t.Errorf("key length = %d, want own byte length %d", k.Length(), len(b))
	}
	want, err := key.New(key.Commit, uint64(len(b)), b)
	if err != nil {
		t.Fatal(err)
	}
	if k != want {
		t.Errorf("key = %s, want %s", k, want)
	}
}

func TestSignaturePayload(t *testing.T) {
	c := merge(t)
	c.PublicKey = []byte("pub")
	unsigned, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	c.Signature = []byte("sig")
	payload, err := c.SignaturePayload()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, unsigned) {
		t.Error("payload is not the encoding without the signature")
	}
	if full, _ := c.Encode(); bytes.Equal(full, payload) {
		t.Error("payload still carries the signature")
	}
	c.PublicKey = []byte("other")
	if other, _ := c.SignaturePayload(); bytes.Equal(other, payload) {
		t.Error("payload does not cover the public key")
	}
}

// commitKeys returns n distinct canonical Commit-type keys.
func commitKeys(t *testing.T, n int) []key.Key {
	t.Helper()
	out := make([]key.Key, n)
	for i := range out {
		var h [32]byte
		binary.BigEndian.PutUint32(h[:], uint32(i+1))
		k, err := key.NewFromHash(key.Commit, 1, h)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = k
	}
	return out
}

func TestEncodeAcceptsBounds(t *testing.T) {
	cases := map[string]func(*commit.Commit){
		"max parents":      func(c *commit.Commit) { c.Parents = commitKeys(t, commit.MaxParents) },
		"tz upper":         func(c *commit.Commit) { c.Author.TZOffset = commit.MaxTZOffset },
		"tz lower":         func(c *commit.Commit) { c.Author.TZOffset = -commit.MaxTZOffset },
		"max message":      func(c *commit.Commit) { c.Message = strings.Repeat("m", commit.MaxMessageLen) },
		"multiline":        func(c *commit.Commit) { c.Message = "subject\n\n\tbody\n" },
		"max name":         func(c *commit.Commit) { c.Author.Name = strings.Repeat("n", commit.MaxIdentityLen) },
		"negative time":    func(c *commit.Commit) { c.Author.When = -1 },
		"max signature":    func(c *commit.Commit) { c.Signature = make([]byte, commit.MaxSignatureLen) },
		"max public key":   func(c *commit.Commit) { c.PublicKey = make([]byte, commit.MaxPublicKeyLen) },
		"dir node as tree": func(c *commit.Commit) { c.Tree, _ = key.NewFromHash(key.DirNode, 9, [32]byte{1}) },
	}
	for name, mutate := range cases {
		c := merge(t)
		mutate(&c)
		b, err := c.Encode()
		if err != nil {
			t.Errorf("%s: Encode: %v", name, err)
			continue
		}
		if _, err := commit.Decode(b); err != nil {
			t.Errorf("%s: Decode: %v", name, err)
		}
	}
}

func TestEncodeRejectsInvalid(t *testing.T) {
	fileNode, _ := key.NewFromHash(key.FileNode, 9, [32]byte{1})
	cases := map[string]func(*commit.Commit){
		"zero tree":            func(c *commit.Commit) { c.Tree = key.Key{} },
		"file tree":            func(c *commit.Commit) { c.Tree = fileNode },
		"non-canonical tree":   func(c *commit.Commit) { c.Tree[0] |= 0x08 },
		"parent not a commit":  func(c *commit.Commit) { c.Parents[0] = c.Tree },
		"duplicate parents":    func(c *commit.Commit) { c.Parents[1] = c.Parents[0] },
		"too many parents":     func(c *commit.Commit) { c.Parents = commitKeys(t, commit.MaxParents+1) },
		"empty author name":    func(c *commit.Commit) { c.Author.Name = "" },
		"empty committer name": func(c *commit.Commit) { c.Committer.Name = "" },
		"control char in name": func(c *commit.Commit) { c.Author.Name = "An\nn" },
		"DEL in email":         func(c *commit.Commit) { c.Author.Email = "a\x7fb" },
		"invalid UTF-8 name":   func(c *commit.Commit) { c.Author.Name = "A\xffnn" },
		"name too long":        func(c *commit.Commit) { c.Author.Name = strings.Repeat("n", commit.MaxIdentityLen+1) },
		"email too long":       func(c *commit.Commit) { c.Committer.Email = strings.Repeat("e", commit.MaxIdentityLen+1) },
		"tz too far east":      func(c *commit.Commit) { c.Author.TZOffset = commit.MaxTZOffset + 1 },
		"tz too far west":      func(c *commit.Commit) { c.Committer.TZOffset = -commit.MaxTZOffset - 1 },
		"message too long":     func(c *commit.Commit) { c.Message = strings.Repeat("m", commit.MaxMessageLen+1) },
		"invalid UTF-8 message": func(c *commit.Commit) { c.Message = "bad \xff" },
		"signature too long":   func(c *commit.Commit) { c.Signature = make([]byte, commit.MaxSignatureLen+1) },
		"public key too long":  func(c *commit.Commit) { c.PublicKey = make([]byte, commit.MaxPublicKeyLen+1) },
	}
	for name, mutate := range cases {
		c := merge(t)
		mutate(&c)
		if _, err := c.Encode(); err == nil {
			t.Errorf("%s: Encode accepted an invalid commit", name)
		}
		if _, _, err := c.Object(); err == nil {
			t.Errorf("%s: Object accepted an invalid commit", name)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	good := handMerge(t)
	if _, err := commit.Decode(good); err != nil {
		t.Fatalf("baseline must decode: %v", err)
	}
	ka, _ := root(t, "a")
	kb, _ := root(t, "b")
	tree := emptyDir(t)
	message := cat([]byte{0x04}, tstr("merge\n"))

	withHeader := func(b []byte, h byte) []byte {
		out := bytes.Clone(b)
		out[0] = h
		return out
	}
	cases := map[string][]byte{
		"garbage":        []byte("not cbor at all"),
		"empty":          {},
		"trailing byte":  append(bytes.Clone(good), 0x00),
		"unknown key 7":  append(withHeader(good, 0xa6), 0x07, 0x00),
		"missing message": withHeader(bytes.TrimSuffix(good, message), 0xa4),
		"non-minimal tz": replaceOnce(t, good, []byte{0x03, 0x18, 0x78}, []byte{0x03, 0x19, 0x00, 0x78}),
		"tz out of range": replaceOnce(t, good, []byte{0x03, 0x18, 0x78}, []byte{0x03, 0x19, 0x05, 0xa0}), // 1440
		"identity missing email": replaceOnce(t, good,
			cat([]byte{0xa4, 0x00}, tstr("Bob"), []byte{0x01}, tstr("")),
			cat([]byte{0xa3, 0x00}, tstr("Bob"))),
		"keys out of order": replaceOnce(t, good,
			cat([]byte{0x00}, bstr32(tree), []byte{0x01, 0x82}, bstr32(ka), bstr32(kb)),
			cat([]byte{0x01, 0x82}, bstr32(ka), bstr32(kb), []byte{0x00}, bstr32(tree))),
		"parents null":       replaceOnce(t, good, cat([]byte{0x01, 0x82}, bstr32(ka), bstr32(kb)), []byte{0x01, 0xf6}),
		"indefinite parents": replaceOnce(t, good, cat([]byte{0x01, 0x82}, bstr32(ka), bstr32(kb)), cat([]byte{0x01, 0x9f}, bstr32(ka), bstr32(kb), []byte{0xff})),
		"parent is a tree":   replaceOnce(t, good, cat(bstr32(ka), bstr32(kb)), cat(bstr32(tree), bstr32(kb))),
		"duplicate parent":   replaceOnce(t, good, cat(bstr32(ka), bstr32(kb)), cat(bstr32(ka), bstr32(ka))),
		"short tree key":     replaceOnce(t, good, cat([]byte{0x00}, bstr32(tree)), cat([]byte{0x00, 0x58, 0x1f}, tree[:31])),
		"message as bytes":   replaceOnce(t, good, message, cat([]byte{0x04, 0x46}, []byte("merge\n"))),
	}
	for name, b := range cases {
		if _, err := commit.Decode(b); err == nil {
			t.Errorf("%s: Decode accepted % x", name, b)
		}
	}
}

// Golden vector: the fixed merge commit, pinned as literal bytes so a CBOR
// library upgrade cannot silently change the wire format, and so other
// implementations (core-rs) can adopt it. Derivation, all from this file:
// tree = key.New(DirLeaf, 1, {0x80}); parents = the keys of two root commits of
// that tree by Ann (committer Ann) with messages "a" and "b"; author Ann,
// committer Bob, message "merge\n".
const (
	goldenParentA = ""
	goldenParentB = ""
	goldenBytes   = ""
	goldenKey     = ""
)

func TestGoldenVector(t *testing.T) {
	ka, _ := root(t, "a")
	kb, _ := root(t, "b")
	k, b, err := merge(t).Object()
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"parent a": {ka.String(), goldenParentA},
		"parent b": {kb.String(), goldenParentB},
		"bytes":    {hex.EncodeToString(b), goldenBytes},
		"key":      {k.String(), goldenKey},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s changed!\n got: %s\nwant: %s", name, pair[0], pair[1])
		}
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./commit`
Expected: build failure, package `commit` has no non-test Go files.

- [x] **Step 3: Implement `commit/commit.go`**

```go
// Package commit defines the Commit object (CAS type 5): the analogue of a git
// commit — a directory root, ordered parent commits, author, committer and
// message, with an optional opaque signature. Encoding is RFC 8949 §4.2
// core-deterministic CBOR (canonical map, integer keys), the fstree and
// reference convention. See architecture/commits.md.
package commit

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/amber-store/core/key"
	"github.com/fxamacker/cbor/v2"
)

const (
	// MaxParents is the maximum number of parent commits.
	MaxParents = 256
	// MaxIdentityLen is the maximum byte length of an identity's name or email.
	MaxIdentityLen = 1024
	// MaxMessageLen is the maximum message length in bytes (1 MiB).
	MaxMessageLen = 1 << 20
	// MaxSignatureLen is the maximum Signature length in bytes (64 KiB).
	MaxSignatureLen = 64 << 10
	// MaxPublicKeyLen is the maximum PublicKey length in bytes (16 KiB).
	MaxPublicKeyLen = 16 << 10
	// MaxTZOffset bounds Identity.TZOffset to under a day either side of UTC.
	MaxTZOffset = 1439
)

// encMode is the shared deterministic encoder, mirroring fstree.encMode.
// NilContainerAsEmpty makes a root commit's nil Parents the empty array.
var encMode cbor.EncMode

func init() {
	opts := cbor.CoreDetEncOptions()
	opts.NilContainers = cbor.NilContainerAsEmpty
	m, err := opts.EncMode()
	if err != nil {
		panic(fmt.Sprintf("commit: building CBOR enc mode: %v", err))
	}
	encMode = m
}

// Identity is who acted and when: git's "Name <email> time tz".
type Identity struct {
	Name     string // 1..MaxIdentityLen bytes, no control characters
	Email    string // 0..MaxIdentityLen bytes, no control characters
	When     int64  // ns since the Unix epoch
	TZOffset int    // minutes east of UTC, -MaxTZOffset..MaxTZOffset
}

// Commit is a snapshot record. Parents are ordered — the first is the
// mainline — and empty for a root commit. Signature and PublicKey are carried
// opaquely; the core neither creates nor verifies signatures.
type Commit struct {
	Tree      key.Key   // DirLeaf or DirNode
	Parents   []key.Key // Commit keys, no duplicates
	Author    Identity
	Committer Identity
	Message   string // UTF-8, may be empty
	Signature []byte // raw SSHSIG blob, nil when unsigned
	PublicKey []byte // signer's key, SSH wire format, nil when absent
}

// wireIdentity and wireCommit are the encoded shapes: canonical CBOR maps with
// integer keys, fields declared in ascending key order. Every identity key and
// commit keys 0-4 are always present; 5 and 6 are omitted when absent.
type wireIdentity struct {
	Name     string `cbor:"0,keyasint"`
	Email    string `cbor:"1,keyasint"`
	When     int64  `cbor:"2,keyasint"`
	TZOffset int64  `cbor:"3,keyasint"`
}

type wireCommit struct {
	Tree      []byte       `cbor:"0,keyasint"`
	Parents   [][]byte     `cbor:"1,keyasint"`
	Author    wireIdentity `cbor:"2,keyasint"`
	Committer wireIdentity `cbor:"3,keyasint"`
	Message   string       `cbor:"4,keyasint"`
	Signature []byte       `cbor:"5,keyasint,omitempty"`
	PublicKey []byte       `cbor:"6,keyasint,omitempty"`
}

func (id Identity) wire() wireIdentity {
	return wireIdentity{Name: id.Name, Email: id.Email, When: id.When, TZOffset: int64(id.TZOffset)}
}

func (c Commit) wire() wireCommit {
	parents := make([][]byte, len(c.Parents))
	for i := range c.Parents {
		parents[i] = c.Parents[i][:]
	}
	return wireCommit{
		Tree:      c.Tree[:],
		Parents:   parents,
		Author:    c.Author.wire(),
		Committer: c.Committer.wire(),
		Message:   c.Message,
		Signature: c.Signature,
		PublicKey: c.PublicKey,
	}
}

func (w wireIdentity) identity() (Identity, error) {
	if w.TZOffset < -MaxTZOffset || w.TZOffset > MaxTZOffset {
		return Identity{}, fmt.Errorf("tz offset %d outside ±%d minutes", w.TZOffset, MaxTZOffset)
	}
	return Identity{Name: w.Name, Email: w.Email, When: w.When, TZOffset: int(w.TZOffset)}, nil
}

func (w wireCommit) commit() (Commit, error) {
	tree, err := key.Parse(w.Tree)
	if err != nil {
		return Commit{}, fmt.Errorf("tree: %w", err)
	}
	var parents []key.Key
	if len(w.Parents) > 0 {
		parents = make([]key.Key, len(w.Parents))
	}
	for i, raw := range w.Parents {
		if parents[i], err = key.Parse(raw); err != nil {
			return Commit{}, fmt.Errorf("parent %d: %w", i, err)
		}
	}
	author, err := w.Author.identity()
	if err != nil {
		return Commit{}, fmt.Errorf("author: %w", err)
	}
	committer, err := w.Committer.identity()
	if err != nil {
		return Commit{}, fmt.Errorf("committer: %w", err)
	}
	return Commit{
		Tree:      tree,
		Parents:   parents,
		Author:    author,
		Committer: committer,
		Message:   w.Message,
		Signature: w.Signature,
		PublicKey: w.PublicKey,
	}, nil
}

// validateText checks an identity string: at most MaxIdentityLen bytes of
// valid UTF-8 with no control characters.
func validateText(s string) error {
	if len(s) > MaxIdentityLen {
		return fmt.Errorf("exceeds %d bytes", MaxIdentityLen)
	}
	if !utf8.ValidString(s) {
		return errors.New("must be valid UTF-8")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

func (id Identity) validate() error {
	if id.Name == "" {
		return errors.New("name must not be empty")
	}
	if err := validateText(id.Name); err != nil {
		return fmt.Errorf("name %w", err)
	}
	if err := validateText(id.Email); err != nil {
		return fmt.Errorf("email %w", err)
	}
	if id.TZOffset < -MaxTZOffset || id.TZOffset > MaxTZOffset {
		return fmt.Errorf("tz offset %d outside ±%d minutes", id.TZOffset, MaxTZOffset)
	}
	return nil
}

// validate checks the whole record against the rules in architecture/commits.md.
func (c Commit) validate() error {
	if err := c.Tree.Validate(); err != nil {
		return fmt.Errorf("commit tree: %w", err)
	}
	if t := c.Tree.Type(); t != key.DirLeaf && t != key.DirNode {
		return fmt.Errorf("commit tree %s is not a directory key (type %v)", c.Tree, t)
	}
	if len(c.Parents) > MaxParents {
		return fmt.Errorf("commit has %d parents, more than %d", len(c.Parents), MaxParents)
	}
	seen := make(map[key.Key]struct{}, len(c.Parents))
	for i, p := range c.Parents {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("commit parent %d: %w", i, err)
		}
		if p.Type() != key.Commit {
			return fmt.Errorf("commit parent %d: %s is not a commit key (type %v)", i, p, p.Type())
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("commit parent %d: duplicate %s", i, p)
		}
		seen[p] = struct{}{}
	}
	if err := c.Author.validate(); err != nil {
		return fmt.Errorf("commit author: %w", err)
	}
	if err := c.Committer.validate(); err != nil {
		return fmt.Errorf("commit committer: %w", err)
	}
	if len(c.Message) > MaxMessageLen {
		return fmt.Errorf("commit message exceeds %d bytes", MaxMessageLen)
	}
	if !utf8.ValidString(c.Message) {
		return errors.New("commit message must be valid UTF-8")
	}
	if len(c.Signature) > MaxSignatureLen {
		return fmt.Errorf("commit signature exceeds %d bytes", MaxSignatureLen)
	}
	if len(c.PublicKey) > MaxPublicKeyLen {
		return fmt.Errorf("commit public key exceeds %d bytes", MaxPublicKeyLen)
	}
	return nil
}

// Encode returns the deterministic CBOR encoding of a validated commit.
func (c Commit) Encode() ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	return encMode.Marshal(c.wire())
}

// Object encodes c and derives its key: type Commit, length field = the
// encoding's own byte length.
func (c Commit) Object() (key.Key, []byte, error) {
	b, err := c.Encode()
	if err != nil {
		return key.Key{}, nil, err
	}
	k, err := key.New(key.Commit, uint64(len(b)), b)
	if err != nil {
		return key.Key{}, nil, err
	}
	return k, b, nil
}

// SignaturePayload returns the bytes a signature runs over: the deterministic
// encoding without the Signature field. PublicKey stays in, so the payload
// binds the signer's key; set it before computing the payload.
func (c Commit) SignaturePayload() ([]byte, error) {
	c.Signature = nil
	return c.Encode()
}

// Decode parses and validates a commit. It rejects non-canonical encodings:
// the input must be byte-for-byte what Encode produces for the same record
// (extra or missing map keys, reordered keys, indefinite-length items,
// non-minimal integers and trailing bytes are all rejected).
func Decode(b []byte) (Commit, error) {
	var w wireCommit
	if err := cbor.Unmarshal(b, &w); err != nil {
		return Commit{}, fmt.Errorf("decoding commit: %w", err)
	}
	c, err := w.commit()
	if err != nil {
		return Commit{}, fmt.Errorf("invalid commit: %w", err)
	}
	if err := c.validate(); err != nil {
		return Commit{}, fmt.Errorf("invalid commit: %w", err)
	}
	canonical, err := encMode.Marshal(c.wire())
	if err != nil {
		return Commit{}, fmt.Errorf("re-encoding commit: %w", err)
	}
	if !bytes.Equal(canonical, b) {
		return Commit{}, errors.New("commit encoding is not canonical")
	}
	return c, nil
}
```

- [x] **Step 4: Run; everything but the golden vector passes**

Run: `go test ./commit`
Expected: only `TestGoldenVector` fails, printing `got:` values against empty `want:`. Every other test passes — in particular `TestEncodeMatchesHandAssembledBytes`, which proves the printed bytes against the spec independently of the CBOR library.

- [x] **Step 5: Pin the golden vector**

Copy the four printed `got:` values into the `goldenParentA`, `goldenParentB`, `goldenBytes`, `goldenKey` constants. Check by eye that `goldenBytes` starts `a5005820` and that `goldenKey` starts `50` (type 5, one length byte) followed by the byte length of `goldenBytes` in hex.

Run: `go test ./commit && go vet ./commit`
Expected: `ok`, no vet output.

- [x] **Step 6: Commit**

```bash
git add commit
git commit -m "commit: the Commit record, its validation and canonical codec"
```

---

### Task 3: graph walks follow commits

**Files:**
- Modify: `fstree/children.go`, `fstree/object.go:1-4`
- Test: `fstree/commit_test.go` (new)

**Interfaces:**
- Consumes: `commit.Commit`, `commit.Identity`, `(commit.Commit).Object`, `commit.Decode`; existing test helpers `completeTree`, `mapGetter`, `mapHas` (package `fstree_test`).
- Produces: `fstree.ChildKeys(k, data)` for `k.Type() == key.Commit` returns `[tree, parents...]`.

- [x] **Step 1: Write the failing tests**

Create `fstree/commit_test.go`:

```go
package fstree_test

import (
	"errors"
	"testing"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

// commitObj builds a commit of tree with the given parents.
func commitObj(t *testing.T, msg string, tree key.Key, parents ...key.Key) fstree.Object {
	t.Helper()
	id := commit.Identity{Name: "Ann", Email: "ann@example.com", When: 1, TZOffset: 60}
	c := commit.Commit{Tree: tree, Parents: parents, Author: id, Committer: id, Message: msg}
	k, b, err := c.Object()
	if err != nil {
		t.Fatal(err)
	}
	return fstree.Object{Key: k, Bytes: b}
}

// history builds two trees and a diamond of commits over them:
//
//	first(treeA) <- left(treeB), right(treeA) <- tip(treeB), a merge.
//
// It returns every object, and the named ones.
func history(t *testing.T) (all []fstree.Object, first, tip fstree.Object) {
	t.Helper()
	treeA := completeTree(t)
	rootA := treeA[len(treeA)-1]
	blobC, err := fstree.EncodeBlob([]byte("gamma"))
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := fstree.EncodeDirLeaf([]fstree.Entry{{Name: []byte("c.txt"), Mode: 0o100644, ContentKey: blobC.Key[:]}})
	if err != nil {
		t.Fatal(err)
	}
	first = commitObj(t, "first", rootA.Key)
	left := commitObj(t, "left", rootB.Key, first.Key)
	right := commitObj(t, "right", rootA.Key, first.Key)
	tip = commitObj(t, "merge", rootB.Key, left.Key, right.Key)
	all = append(append([]fstree.Object{}, treeA...), blobC, rootB, first, left, right, tip)
	return all, first, tip
}

func TestChildKeysCommit(t *testing.T) {
	tree, err := fstree.EncodeDirLeaf(nil)
	if err != nil {
		t.Fatal(err)
	}
	p1 := commitObj(t, "p1", tree.Key)
	p2 := commitObj(t, "p2", tree.Key)
	c := commitObj(t, "merge", tree.Key, p2.Key, p1.Key)
	kids, err := fstree.ChildKeys(c.Key, c.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want := []key.Key{tree.Key, p2.Key, p1.Key} // the tree, then parents in recorded order
	if len(kids) != len(want) {
		t.Fatalf("children = %v, want %v", kids, want)
	}
	for i := range want {
		if kids[i] != want[i] {
			t.Errorf("child %d = %s, want %s", i, kids[i], want[i])
		}
	}
	if _, err := fstree.ChildKeys(c.Key, []byte("not a commit")); err == nil {
		t.Error("ChildKeys accepted garbage under a Commit key")
	}
}

func TestReachableKeysFollowsHistory(t *testing.T) {
	all, _, tip := history(t)
	keys, err := fstree.ReachableKeys(tip.Key, mapGetter(all...))
	if err != nil {
		t.Fatal(err)
	}
	if keys[0] != tip.Key {
		t.Errorf("keys[0] = %s, want the tip", keys[0])
	}
	got := map[key.Key]int{}
	for _, k := range keys {
		got[k]++
	}
	for _, o := range all {
		if got[o.Key] != 1 {
			t.Errorf("%s (%v) listed %d times, want once", o.Key, o.Key.Type(), got[o.Key])
		}
	}
	if len(keys) != len(all) {
		t.Errorf("%d keys reachable, want %d", len(keys), len(all))
	}
}

func TestCheckCompleteFollowsHistory(t *testing.T) {
	all, first, tip := history(t)
	visited, err := fstree.CheckComplete(tip.Key, mapGetter(all...), mapHas(all...), 4)
	if err != nil {
		t.Fatalf("CheckComplete: %v", err)
	}
	if len(visited) != len(all) {
		t.Errorf("visited %d keys, want %d", len(visited), len(all))
	}

	without := func(drop key.Key) []fstree.Object {
		var out []fstree.Object
		for _, o := range all {
			if o.Key != drop {
				out = append(out, o)
			}
		}
		return out
	}
	// An ancestor commit is gone: the history is incomplete.
	rest := without(first.Key)
	if _, err := fstree.CheckComplete(tip.Key, mapGetter(rest...), mapHas(rest...), 4); err == nil {
		t.Error("CheckComplete accepted a history with a missing ancestor")
	}
	// A blob only the first commit's tree holds is gone: also incomplete.
	blobB := all[1]
	rest = without(blobB.Key)
	_, err = fstree.CheckComplete(tip.Key, mapGetter(rest...), mapHas(rest...), 4)
	var missing *fstree.MissingObjectError
	if !errors.As(err, &missing) || missing.Key != blobB.Key {
		t.Errorf("err = %v, want MissingObjectError for %s", err, blobB.Key)
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./fstree -run 'Commit|History'`
Expected: FAIL with `fstree: unknown object type Commit`.

- [x] **Step 3: Implement**

`fstree/children.go`: add the import `"github.com/amber-store/core/commit"`, replace the doc comment's last sentence, and add the case before `default`:

```go
// ChildKeys returns the keys directly referenced by the object with key k
// and serialized bytes data, in encounter order. Blob and XattrSet objects
// are leaves and have no children. A Commit's children are its tree, then
// its parents in recorded order — so every walk built on ChildKeys follows
// history.
```

```go
	case key.Commit:
		c, err := commit.Decode(data)
		if err != nil {
			return nil, fmt.Errorf("fstree: decoding Commit %s: %w", k, err)
		}
		out := make([]key.Key, 0, 1+len(c.Parents))
		out = append(out, c.Tree)
		return append(out, c.Parents...), nil
```

`fstree/object.go`: extend the package comment after its last sentence:

```go
// The object-graph walks (ChildKeys, ReachableKeys, CheckComplete) also follow
// Commit objects (package commit), whose children are a tree and parent commits.
```

- [x] **Step 4: Run to verify pass**

Run: `go test ./fstree ./commit && go vet ./fstree`
Expected: `ok` twice.

- [x] **Step 5: Commit**

```bash
git add fstree
git commit -m "fstree: ChildKeys follows a Commit to its tree and parents"
```

---

### Task 4: packstore verifies a commit's length field

**Files:**
- Modify: `packstore/verify.go:93-112`
- Test: `packstore/verify_test.go` (append)

**Interfaces:**
- Consumes: `key.Commit`; internal `verifyObject(o Object) error`, `Object{Key, Data}`, `ErrVerify`.

- [x] **Step 1: Write the failing test**

Append to `packstore/verify_test.go` (package `packstore`; add `"github.com/amber-store/core/key"` and `"errors"` to its imports if absent):

```go
func TestVerifyObjectChecksCommitLength(t *testing.T) {
	// verifyObject does not parse payloads, so any bytes serve.
	data := []byte("stand-in for a commit's canonical CBOR")
	good, err := key.New(key.Commit, uint64(len(data)), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyObject(Object{Key: good, Data: data}); err != nil {
		t.Fatalf("honest commit key rejected: %v", err)
	}
	// Same payload hash, but a length field that lies about the byte length.
	bad, err := key.New(key.Commit, uint64(len(data))+1, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyObject(Object{Key: bad, Data: data}); !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v, want ErrVerify for a wrong length field", err)
	}
}
```

- [x] **Step 2: Run to verify failure**

Run: `go test ./packstore -run TestVerifyObjectChecksCommitLength`
Expected: FAIL, `err = <nil>, want ErrVerify`.

- [x] **Step 3: Implement**

In `packstore/verify.go` replace the `verifyObject` comment and the switch case:

```go
// verifyObject recomputes o.Key from o.Data and reports ErrVerify on mismatch.
// For Blob, XattrSet and Commit — whose key length is the serialized byte
// length — it also checks the length field. Aggregate types
// (FileNode/DirLeaf/DirNode) carry a logical length the store cannot recompute
// without parsing, so only their hash is checked.
```

```go
	case key.Blob, key.XattrSet, key.Commit:
```

- [x] **Step 4: Run to verify pass**

Run: `go test ./packstore`
Expected: `ok`.

- [x] **Step 5: Commit**

```bash
git add packstore
git commit -m "packstore: verify a Commit's length field"
```

---

### Task 5: gc keeps history live

No production change is expected: the mark, `PrepareRef` and `Why` all dispatch through `fstree.ChildKeys`. This task proves it and guards it.

**Files:**
- Test: `gc/commit_test.go` (new, package `gc`)

**Interfaces:**
- Consumes: test helpers `newTestStore`, `(*testStore).openCollector`, `storeTree`, `putTestRef`, `rmTestRef`, `backdatePacks`; `(*Collector).Run`, `PrepareRef`, `Why`; `commit.Commit.Object`.

- [x] **Step 1: Write the tests**

Create `gc/commit_test.go`:

```go
package gc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

// storeDirTree stores storeTree's file under a one-entry directory and
// returns the directory root (a commit's tree must be a directory) and
// every key.
func storeDirTree(t *testing.T, st *packstore.Store, seed string, n int) (key.Key, []key.Key) {
	t.Helper()
	file, all := storeTree(t, st, seed, n)
	leaf, err := fstree.EncodeDirLeaf([]fstree.Entry{{Name: []byte("f"), Mode: 0o100644, ContentKey: file[:]}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(leaf.Key, leaf.Bytes); err != nil {
		t.Fatal(err)
	}
	return leaf.Key, append(all, leaf.Key)
}

// newCommit builds a commit of tree without storing it.
func newCommit(t *testing.T, tree key.Key, parents ...key.Key) (key.Key, []byte) {
	t.Helper()
	id := commit.Identity{Name: "Ann", When: 1}
	k, b, err := commit.Commit{Tree: tree, Parents: parents, Author: id, Committer: id, Message: "m"}.Object()
	if err != nil {
		t.Fatal(err)
	}
	return k, b
}

func storeCommit(t *testing.T, st *packstore.Store, tree key.Key, parents ...key.Key) key.Key {
	t.Helper()
	k, b := newCommit(t, tree, parents...)
	if err := st.Put(k, b); err != nil {
		t.Fatal(err)
	}
	return k
}

func countGone(t *testing.T, st *packstore.Store, keys []key.Key) int {
	t.Helper()
	gone := 0
	for _, k := range keys {
		if _, err := st.Get(k); errors.Is(err, packstore.ErrNotFound) {
			gone++
		}
	}
	return gone
}

func TestCommitHistoryStaysLive(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	treeOld, keysOld := storeDirTree(t, ts.objects, "old", 40)
	treeNew, keysNew := storeDirTree(t, ts.objects, "new", 40)
	_, keysDead := storeDirTree(t, ts.objects, "dead", 40) // never referenced
	first := storeCommit(t, ts.objects, treeOld)
	tip := storeCommit(t, ts.objects, treeNew, first)
	putTestRef(t, c, ts.refs, "main", tip)

	// Why walks through the commits: the old tree is held by the branch.
	names, err := c.Why(keysOld[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"main"}) {
		t.Errorf("Why(old blob) = %v, want [main]", names)
	}

	backdatePacks(t, ts)
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) == 0 {
		t.Fatalf("nothing reaped: %+v", stats)
	}
	live := append(append(slices.Clone(keysOld), keysNew...), first, tip)
	for _, k := range live {
		if _, err := ts.objects.Get(k); err != nil {
			t.Fatalf("key %s reachable from the branch: %v", k, err)
		}
	}
	if countGone(t, ts.objects, keysDead) == 0 {
		t.Error("no unreferenced key was collected")
	}

	// Dropping the branch makes the whole history garbage.
	rmTestRef(t, c, ts.refs, "main", tip)
	backdatePacks(t, ts)
	if _, err := c.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if countGone(t, ts.objects, keysOld) == 0 {
		t.Error("no key of the old commit's tree was collected after the branch went")
	}
}

func TestPrepareRefMissingAncestorFails(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	tree, _ := storeDirTree(t, ts.objects, "tree", 4)
	absent, _ := newCommit(t, tree) // built, never stored
	tip := storeCommit(t, ts.objects, tree, absent)
	if _, _, err := c.PrepareRef(tip); err == nil {
		t.Fatal("PrepareRef accepted a commit whose parent is missing")
	}
}
```

- [x] **Step 2: Run**

Run: `go test ./gc -run 'Commit|Ancestor'`
Expected: PASS (Task 3 already taught the walks). To see the guard bite, temporarily delete the `key.Commit` case from `fstree/children.go`, re-run, observe `unknown object type Commit`, and restore it.

- [x] **Step 3: Run the package**

Run: `go test ./gc && go vet ./gc`
Expected: `ok`.

- [x] **Step 4: Commit**

```bash
git add gc
git commit -m "gc: test that a commit keeps its history live"
```

---

### Task 6: CLI — peel commits, `commit create`, `commit show`

**Files:**
- Modify: `cmd/amber-store/spec.go` (`descend`), `cmd/amber-store/main.go:40-47`
- Create: `cmd/amber-store/commit.go`, `cmd/amber-store/commit_test.go`
- Test: `cmd/amber-store/e2e_test.go` (append `TestE2E_Commit`)

**Interfaces:**
- Consumes: `resolveSpec`, `descend`, `openStore`, `closeStore`, `openCollector`, `putRef`, `runApp`, `writeFixture`; `commit.Commit`, `commit.Identity`, `commit.Decode`; `reference.Reference`, `reference.ValidateName`.
- Produces: `peelCommit(objects, k) (key.Key, error)`, `commitCommand() *cli.Command`, `parseIdentity(s string) (commit.Identity, error)`, `renderCommit(w io.Writer, k key.Key, c commit.Commit) error`.

- [x] **Step 1: Write the failing unit tests**

Create `cmd/amber-store/commit_test.go`:

```go
package main

import (
	"bytes"
	"testing"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/key"
)

func TestParseIdentity(t *testing.T) {
	cases := []struct {
		in, name, email string
		wantErr         bool
	}{
		{in: "Ann Example <ann@example.com>", name: "Ann Example", email: "ann@example.com"},
		{in: "  Ann  <ann@example.com>  ", name: "Ann", email: "ann@example.com"},
		{in: "Ann", name: "Ann"},
		{in: "Ann <>", name: "Ann"},
		{in: "A <b> C <c@d>", name: "A <b> C", email: "c@d"},
		{in: "<ann@example.com>", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseIdentity(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseIdentity(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got.Name != tc.name || got.Email != tc.email {
			t.Errorf("parseIdentity(%q) = %q, %q; want %q, %q", tc.in, got.Name, got.Email, tc.name, tc.email)
		}
	}
}

func TestRenderCommit(t *testing.T) {
	tree, _ := key.New(key.DirLeaf, 1, []byte{0x80})
	parent, _ := key.NewFromHash(key.Commit, 7, [32]byte{1})
	self, _ := key.NewFromHash(key.Commit, 9, [32]byte{2})
	c := commit.Commit{
		Tree:      tree,
		Parents:   []key.Key{parent},
		Author:    commit.Identity{Name: "Ann", Email: "ann@example.com", When: 1767323045_000000000, TZOffset: 60},
		Committer: commit.Identity{Name: "Bob", When: 1767323045_000000000, TZOffset: -300},
		Message:   "subject\n\nbody\n",
		Signature: []byte("sig"),
	}
	var buf bytes.Buffer
	if err := renderCommit(&buf, self, c); err != nil {
		t.Fatal(err)
	}
	want := "commit " + self.String() + "\n" +
		"tree " + tree.String() + "\n" +
		"parent " + parent.String() + "\n" +
		"author Ann <ann@example.com> 2026-01-02T04:04:05+01:00\n" +
		"committer Bob 2026-01-01T22:04:05-05:00\n" +
		"signature 3 bytes\n" +
		"\n" +
		"    subject\n" +
		"    \n" +
		"    body\n"
	if got := buf.String(); got != want {
		t.Errorf("renderCommit:\n got: %q\nwant: %q", got, want)
	}
}
```

(`1767323045` is 2026-01-02T03:04:05Z.)

- [x] **Step 2: Write the failing e2e test**

Append to `cmd/amber-store/e2e_test.go`:

```go
func TestE2E_Commit(t *testing.T) {
	src := t.TempDir()
	writeFixture(t, src)
	store := t.TempDir()
	seg := []string{"--store", store, "--segment-size", "4096"}
	run := func(args ...string) string {
		t.Helper()
		out, err := runApp(t, append(slices.Clone(seg), args...)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return strings.TrimSpace(out)
	}
	create := []string{"commit", "create", "--ref", "main",
		"--author", "Ann <ann@example.com>", "--date", "2026-01-02T03:04:05+01:00"}

	root1 := run("ingest", "--no-progress", src)
	c1 := run(append(slices.Clone(create), "-m", "first", root1)...)

	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha, revised"), 0o644); err != nil {
		t.Fatal(err)
	}
	root2 := run("ingest", "--no-progress", src)
	c2 := run(append(slices.Clone(create), "--parent", "ref:main", "-m", "second", root2)...)
	if c1 == c2 || c2 == root2 {
		t.Fatalf("commit keys: c1 %s, c2 %s, root2 %s", c1, c2, root2)
	}
	if got := run("ref", "get", "main"); got != c2 {
		t.Errorf("ref get main = %s, want the second commit %s", got, c2)
	}

	show := run("commit", "show", "ref:main")
	for _, want := range []string{
		"commit " + c2, "tree " + root2, "parent " + c1,
		"author Ann <ann@example.com> 2026-01-02T03:04:05+01:00", "    second",
	} {
		if !strings.Contains(show, want) {
			t.Errorf("commit show output %q is missing %q", show, want)
		}
	}

	// A commit stands in for its tree wherever a directory is expected.
	for spec, want := range map[string]string{"ref:main": "a.txt", "ref:main@sub": "b.txt", c1: "a.txt", c1 + "/sub": "b.txt"} {
		if out := run("ls", spec); !strings.Contains(out, want) {
			t.Errorf("ls %s output %q does not mention %s", spec, out, want)
		}
	}

	// History is reachable from the branch, so a forced gc keeps the first
	// commit's tree: it still restores, with the original content.
	time.Sleep(50 * time.Millisecond)
	run("gc", "run", "--grace", "1ms", "--garbage", "0")
	for commitKey, want := range map[string]string{c1: "alpha", c2: "alpha, revised"} {
		dest := filepath.Join(t.TempDir(), "restored")
		run("restore", commitKey, dest)
		got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("restored a.txt of %s = %q, want %q", commitKey, got, want)
		}
	}

	// Rejections: a tree as a parent, a file as the tree, a missing author.
	for name, args := range map[string][]string{
		"tree as parent": {"commit", "create", "--author", "Ann", "-m", "x", "--parent", root1, root2},
		"file as tree":   {"commit", "create", "--author", "Ann", "-m", "x", root2 + "/a.txt"},
		"no author":      {"commit", "create", "-m", "x", root2},
		"show a tree":    {"commit", "show", root2},
	} {
		if _, err := runApp(t, append(slices.Clone(seg), args...)...); err == nil {
			t.Errorf("%s: command succeeded", name)
		}
	}
}
```

Add `"slices"` to the file's imports.

- [x] **Step 3: Run to verify failure**

Run: `go test ./cmd/amber-store -run 'Commit|ParseIdentity'`
Expected: build failure, `undefined: parseIdentity`, `undefined: renderCommit`.

- [x] **Step 4: Implement the peel**

In `cmd/amber-store/spec.go` add the import `"github.com/amber-store/core/commit"`, add `peelCommit`, and make `descend` start from the peeled key:

```go
// peelCommit maps a Commit key to the directory root it records; any other
// key is returned unchanged. Only a spec's root can be a commit — directory
// entries never hold one.
func peelCommit(objects *packstore.Store, k key.Key) (key.Key, error) {
	if k.Type() != key.Commit {
		return k, nil
	}
	data, err := objects.Get(k)
	if err != nil {
		return key.Key{}, fmt.Errorf("reading commit %s: %w", k, err)
	}
	rec, err := commit.Decode(data)
	if err != nil {
		return key.Key{}, fmt.Errorf("commit %s: %w", k, err)
	}
	return rec.Tree, nil
}
```

In `descend`, extend the doc comment with `A Commit root stands for its tree.` and replace the first line `k := root` with:

```go
	k, err := peelCommit(objects, root)
	if err != nil {
		return key.Key{}, err
	}
```

- [x] **Step 5: Implement `cmd/amber-store/commit.go`**

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/gc"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
	"github.com/amber-store/core/reference"
	"github.com/amber-store/core/refstore"
	"github.com/urfave/cli/v2"
)

func commitCommand() *cli.Command {
	return &cli.Command{
		Name:  "commit",
		Usage: "create and inspect commits: a tree with its parent commits, author, committer and message",
		Subcommands: []*cli.Command{
			{
				Name:      "create",
				Usage:     "record the directory at TREE as a commit and print the commit key",
				ArgsUsage: "KEY[/PATH] | ref:NAME[@PATH]",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "message", Aliases: []string{"m"}, Usage: "commit message"},
					&cli.StringFlag{Name: "author", Usage: "author as 'Name <email>'", Required: true},
					&cli.StringFlag{Name: "committer", Usage: "committer as 'Name <email>' (default: the author)"},
					&cli.StringFlag{Name: "date", Usage: "author and committer time, RFC 3339 (default: now, local zone)"},
					&cli.StringSliceFlag{Name: "parent", Usage: "parent commit, KEY or ref:NAME; repeat for a merge, mainline first"},
					&cli.StringFlag{Name: "ref", Usage: "point reference NAME at the new commit"},
				},
				Action: runCommitCreate,
			},
			{
				Name:      "show",
				Usage:     "print the commit at KEY or ref:NAME",
				ArgsUsage: "KEY | ref:NAME",
				Action:    runCommitShow,
			},
		},
	}
}

// parseIdentity splits "Name <email>" into its parts. A string with no
// trailing "<...>" is all name; the name must not be empty.
func parseIdentity(s string) (commit.Identity, error) {
	s = strings.TrimSpace(s)
	name, email := s, ""
	if strings.HasSuffix(s, ">") {
		if i := strings.LastIndex(s, "<"); i >= 0 {
			name, email = strings.TrimSpace(s[:i]), s[i+1:len(s)-1]
		}
	}
	if name == "" {
		return commit.Identity{}, fmt.Errorf("identity %q has no name; want 'Name <email>'", s)
	}
	return commit.Identity{Name: name, Email: email}, nil
}

func runCommitCreate(c *cli.Context) error {
	if c.NArg() != 1 {
		return fmt.Errorf("commit create requires exactly one TREE argument, got %d", c.NArg())
	}
	author, err := parseIdentity(c.String("author"))
	if err != nil {
		return fmt.Errorf("--author: %w", err)
	}
	committer := author
	if s := c.String("committer"); s != "" {
		if committer, err = parseIdentity(s); err != nil {
			return fmt.Errorf("--committer: %w", err)
		}
	}
	when := time.Now()
	if s := c.String("date"); s != "" {
		if when, err = time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("--date: %w", err)
		}
	}
	_, offset := when.Zone()
	author.When, author.TZOffset = when.UnixNano(), offset/60
	committer.When, committer.TZOffset = when.UnixNano(), offset/60
	refName := c.String("ref")
	if refName != "" {
		if err := reference.ValidateName(refName); err != nil {
			return err
		}
	}

	objects, refs, err := openStore(c)
	if err != nil {
		return err
	}
	k, err := createCommit(c, objects, refs, commit.Commit{
		Author:    author,
		Committer: committer,
		Message:   c.String("message"),
	}, refName)
	if err := errors.Join(err, closeStore(objects, refs)); err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.App.Writer, k.String())
	return err
}

// createCommit resolves the tree and parent specs into rec, stores the commit
// and, when refName is set, points that reference at it.
func createCommit(c *cli.Context, objects *packstore.Store, refs *refstore.Store, rec commit.Commit, refName string) (key.Key, error) {
	root, path, err := resolveSpec(refs, c.Args().First())
	if err != nil {
		return key.Key{}, err
	}
	if rec.Tree, err = descend(objects, root, path); err != nil {
		return key.Key{}, err
	}
	for _, spec := range c.StringSlice("parent") {
		pk, ppath, err := resolveSpec(refs, spec)
		if err != nil {
			return key.Key{}, fmt.Errorf("--parent %s: %w", spec, err)
		}
		if ppath != "" {
			return key.Key{}, fmt.Errorf("--parent %s: a parent is a commit, not a path within one", spec)
		}
		rec.Parents = append(rec.Parents, pk)
	}
	k, raw, err := rec.Object()
	if err != nil {
		return key.Key{}, err
	}
	for _, child := range append([]key.Key{rec.Tree}, rec.Parents...) {
		ok, err := objects.Has(child)
		if err != nil {
			return key.Key{}, err
		}
		if !ok {
			return key.Key{}, fmt.Errorf("%s is not in the store", child)
		}
	}
	if err := objects.Put(k, raw); err != nil {
		return key.Key{}, err
	}
	if refName == "" {
		return k, nil
	}
	ref := reference.Reference{Name: refName, Key: k[:], CreatedAt: time.Now().UnixNano()}
	refRaw, err := ref.Encode()
	if err == nil {
		var coll *gc.Collector
		if coll, err = openCollector(c, objects, refs, gc.Options{}); err == nil {
			err = errors.Join(putRef(coll, refs, refName, k, refRaw), coll.Close())
		}
	}
	if err != nil {
		return key.Key{}, fmt.Errorf("commit stored (%s) but setting reference %q failed: %w\nretry with: amber-store ref set %q %s",
			k, refName, err, refName, k)
	}
	return k, nil
}

func runCommitShow(c *cli.Context) error {
	if c.NArg() != 1 {
		return fmt.Errorf("commit show requires exactly one KEY or ref:NAME argument, got %d", c.NArg())
	}
	objects, refs, err := openStore(c)
	if err != nil {
		return err
	}
	defer closeStore(objects, refs)
	k, path, err := resolveSpec(refs, c.Args().First())
	if err != nil {
		return err
	}
	if path != "" {
		return fmt.Errorf("commit show takes a commit, not a path within one")
	}
	if k.Type() != key.Commit {
		return fmt.Errorf("%s is not a commit (type %v)", k, k.Type())
	}
	data, err := objects.Get(k)
	if err != nil {
		return err
	}
	rec, err := commit.Decode(data)
	if err != nil {
		return fmt.Errorf("commit %s: %w", k, err)
	}
	return renderCommit(c.App.Writer, k, rec)
}

// renderCommit prints a commit in git's cat-file layout: headers, a blank
// line, then the message indented by four spaces.
func renderCommit(w io.Writer, k key.Key, c commit.Commit) error {
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\ntree %s\n", k, c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\ncommitter %s\n", identityLine(c.Author), identityLine(c.Committer))
	if len(c.Signature) > 0 {
		fmt.Fprintf(&b, "signature %d bytes\n", len(c.Signature))
	}
	if msg := strings.TrimRight(c.Message, "\n"); msg != "" {
		b.WriteString("\n")
		for line := range strings.SplitSeq(msg, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// identityLine renders "Name <email> time", the time in the identity's zone.
func identityLine(id commit.Identity) string {
	who := id.Name
	if id.Email != "" {
		who += " <" + id.Email + ">"
	}
	zone := time.FixedZone("", id.TZOffset*60)
	return who + " " + time.Unix(0, id.When).In(zone).Format(time.RFC3339)
}
```

In `cmd/amber-store/main.go` add `commitCommand(),` to `Commands` after `refCommand(),`.

- [x] **Step 6: Run to verify pass**

Run: `go test ./cmd/amber-store && go vet ./cmd/amber-store`
Expected: `ok`.

- [x] **Step 7: Commit**

```bash
git add cmd/amber-store
git commit -m "amber-store: commit create/show; a commit stands for its tree"
```

---

### Task 7: documentation

**Files:**
- Create: `architecture/commits.md`
- Modify: `architecture/types.md`, `architecture/references.md:3-4`, `README.md`

- [x] **Step 1: Write `architecture/commits.md`**

Sections, in order, with the content of the spec's matching sections restated as reference documentation (present tense, no "considered" asides): intro (what a commit is, relation to references: a reference naming a commit key is a branch); **The record** (both tables, verbatim key numbers and bounds); **Signing** (payload, SSHSIG namespace `amber-store-commit`, consumer concern); **Decoding is strict**; **The key** (type 5, own-length rule, packstore verifies it); **Reachability** (children = tree then parents; transfer, completeness, gc consequences; the PrepareRef cost note); **Golden vector** (the derivation from `commit/commit_test.go` and the four pinned hex values); **CLI** (`commit create`, `commit show`, commits as tree specs).

- [x] **Step 2: Update `architecture/types.md`**

- `The 4-bit type field has 16 slots; 5 are defined.` becomes `… 6 are defined.`
- Add the table row after `XattrSet`, and renumber the reserved row to `6–15`:

  ```
  | 5    | `Commit`   | Snapshot record: a directory root, parent commits, author, committer, message ([commits.md](commits.md)). | own serialized byte length      | `DirLeaf` / `DirNode` (tree), `Commit` (parents) |
  ```
- Length-field bullet `` `Blob`, `XattrSet`: `` becomes `` `Blob`, `XattrSet`, `Commit`: ``.
- In "Root directory metadata", replace the sentence `Snapshot/versioning objects are intentionally out of scope at this stage.` with: `A [Commit](commits.md) object is that reference-side record in content-addressed form: it names a root directory and carries who recorded it, when, why, and which commits it follows.`

- [x] **Step 3: Update `architecture/references.md` and `README.md`**

- `references.md`: `pointing at a store key (a file or a directory)` becomes `pointing at a store key (a file, a directory, or a commit)`.
- `README.md` object-types table: add `` | `Commit`   | Snapshot record: a tree, parent commits, author, committer, message. | ``.
- `README.md` package table: add after `fstree`: `` | `commit` | The commit record: canonical CBOR encoding and validation; signature fields carried opaquely. | ``.
- `README.md` CLI block: add

  ```sh
  amber-store --store ./store commit create --ref main --author 'Ann <ann@example.com>' -m 'first' KEY   # record a tree; --parent KEY|ref:NAME links history
  amber-store --store ./store commit show ref:main        # print a commit, git cat-file style
  ```
  and the sentence: `A commit key, or a reference to one, works wherever a directory KEY does — it stands for the commit's tree.`
- `README.md` architecture table: add `` | [`architecture/commits.md`](architecture/commits.md) | The commit object: record layout, signing convention, reachability, golden vector. | ``.

- [x] **Step 4: Verify the whole repository**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: every package `ok` (`cmd/amber-bench` builds the CLI; remove any binary it leaves behind).

- [x] **Step 5: Commit**

```bash
git add architecture README.md docs/superpowers
git commit -m "docs: the Commit object type; design and plan"
```
