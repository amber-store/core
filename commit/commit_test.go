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
func bstr32(k key.Key) []byte    { return append([]byte{0x58, 0x20}, k[:]...) }
func u64(n int64) []byte         { return binary.BigEndian.AppendUint64([]byte{0x1b}, uint64(n)) }
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
		"zero tree":             func(c *commit.Commit) { c.Tree = key.Key{} },
		"file tree":             func(c *commit.Commit) { c.Tree = fileNode },
		"non-canonical tree":    func(c *commit.Commit) { c.Tree[0] |= 0x08 },
		"parent not a commit":   func(c *commit.Commit) { c.Parents[0] = c.Tree },
		"duplicate parents":     func(c *commit.Commit) { c.Parents[1] = c.Parents[0] },
		"too many parents":      func(c *commit.Commit) { c.Parents = commitKeys(t, commit.MaxParents+1) },
		"empty author name":     func(c *commit.Commit) { c.Author.Name = "" },
		"empty committer name":  func(c *commit.Commit) { c.Committer.Name = "" },
		"control char in name":  func(c *commit.Commit) { c.Author.Name = "An\nn" },
		"DEL in email":          func(c *commit.Commit) { c.Author.Email = "a\x7fb" },
		"invalid UTF-8 name":    func(c *commit.Commit) { c.Author.Name = "A\xffnn" },
		"name too long":         func(c *commit.Commit) { c.Author.Name = strings.Repeat("n", commit.MaxIdentityLen+1) },
		"email too long":        func(c *commit.Commit) { c.Committer.Email = strings.Repeat("e", commit.MaxIdentityLen+1) },
		"tz too far east":       func(c *commit.Commit) { c.Author.TZOffset = commit.MaxTZOffset + 1 },
		"tz too far west":       func(c *commit.Commit) { c.Committer.TZOffset = -commit.MaxTZOffset - 1 },
		"message too long":      func(c *commit.Commit) { c.Message = strings.Repeat("m", commit.MaxMessageLen+1) },
		"invalid UTF-8 message": func(c *commit.Commit) { c.Message = "bad \xff" },
		"signature too long":    func(c *commit.Commit) { c.Signature = make([]byte, commit.MaxSignatureLen+1) },
		"public key too long":   func(c *commit.Commit) { c.PublicKey = make([]byte, commit.MaxPublicKeyLen+1) },
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
		"garbage":         []byte("not cbor at all"),
		"empty":           {},
		"trailing byte":   append(bytes.Clone(good), 0x00),
		"unknown key 7":   append(withHeader(good, 0xa6), 0x07, 0x00),
		"missing message": withHeader(bytes.TrimSuffix(good, message), 0xa4),
		"non-minimal tz":  replaceOnce(t, good, []byte{0x03, 0x18, 0x78}, []byte{0x03, 0x19, 0x00, 0x78}),
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
	goldenParentA = "5073d980bd63330e7b37ddd0989bea896cd6a35988e973dfc4b1b28808930a7c"
	goldenParentB = "5073c8825d499d27183b57319a8b637c7868ef9783da1d19369231d0bbe48831"
	goldenBytes   = "a50058202001bbe6a9f5a0146a1f4d0381e9b0ed1ac2f1a979ce9d5ad84e46ff0b58f36b018258205073d980bd63330e7b37ddd0989bea896cd6a35988e973dfc4b1b28808930a7c58205073c8825d499d27183b57319a8b637c7868ef9783da1d19369231d0bbe4883102a40063416e6e016f616e6e406578616d706c652e636f6d021b17979cfe362a000003187803a40063426f620160021b17979cfe362a00010339012b04666d657267650a"
	goldenKey     = "50ae7b19332c07e0f197c8a9d410c0cc3ed2d7fd6cf34d3a7cb3c43d6c514980"
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
