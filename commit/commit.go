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
