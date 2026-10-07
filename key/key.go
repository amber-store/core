package key

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"slices"

	"github.com/zeebo/blake3"
)

// Size is the fixed byte length of every key.
const Size = 32

// Key is a 32-byte lookup key. It is a value type and is directly comparable,
// so it can be used as a Go map key. Accessors assume the key is canonical
// (produced by New, NewFromHash, or Parse).
//
// The bytes are the (header, length, hash) encoding of architecture/keys.md
// reversed: the truncated hash comes first and the header byte last, so keys
// sort and bucket uniformly on their leading bytes.
type Key [Size]byte

// headerAt is the index of the header byte: the last one.
const headerAt = Size - 1

// Type returns the CAS object type from the header's high nibble.
func (k Key) Type() Type {
	return Type(k[headerAt] >> 4)
}

// LengthSize returns the number of bytes the payload-length field occupies (1..8).
func (k Key) LengthSize() int {
	return int(k[headerAt]&0x07) + 1
}

// Length decodes the payload-length field, which sits just before the header
// byte, least significant byte first.
func (k Key) Length() uint64 {
	var buf [8]byte
	copy(buf[:], k[headerAt-k.LengthSize():headerAt])
	return binary.LittleEndian.Uint64(buf[:])
}

// Hash returns the truncated payload hash (len == Size-1-LengthSize) in digest
// order, a copy: the key stores it reversed.
func (k Key) Hash() []byte {
	h := bytes.Clone(k[:headerAt-k.LengthSize()])
	slices.Reverse(h)
	return h
}

// lengthSizeFor returns the minimum number of bytes needed to hold length
// big-endian with no leading zero. Zero is the special case: a single 0x00 byte.
func lengthSizeFor(length uint64) int {
	if length == 0 {
		return 1
	}
	return (bits.Len64(length) + 7) / 8
}

// New computes the BLAKE3-256 digest of serialized, then assembles a canonical
// key via NewFromHash. length is the logical payload length and is taken as
// given (it need not equal len(serialized) — see NewFromHash).
func New(t Type, length uint64, serialized []byte) (Key, error) {
	return NewFromHash(t, length, blake3.Sum256(serialized))
}

// Validate reports whether k is canonical: the reserved bit is clear, the type
// is defined (0..5), and the length field is minimally encoded (its most
// significant byte, the one next to the header, is non-zero, except for the
// single 0x00 byte that encodes a zero length).
func (k Key) Validate() error {
	if k[headerAt]&0x08 != 0 {
		return ErrReservedBitSet
	}
	if !k.Type().IsValid() {
		return fmt.Errorf("%w: %d", ErrReservedType, uint8(k.Type()))
	}
	if k[headerAt-1] == 0 && !(k.LengthSize() == 1 && k.Length() == 0) {
		return ErrNonCanonicalLength
	}
	return nil
}

// Parse copies b into a Key and validates its canonical form. b must be exactly
// Size bytes.
func Parse(b []byte) (Key, error) {
	if len(b) != Size {
		return Key{}, fmt.Errorf("%w: got %d", ErrBadKeyLength, len(b))
	}
	var k Key
	copy(k[:], b)
	if err := k.Validate(); err != nil {
		return Key{}, err
	}
	return k, nil
}

// String returns the lowercase hex encoding of the key, for logs and errors.
func (k Key) String() string {
	return hex.EncodeToString(k[:])
}

// NewFromHash assembles a canonical key from a CAS object type, a logical
// payload length, and a precomputed full 256-bit BLAKE3 digest. The digest is
// truncated to its leading bytes to fill the key, and the whole encoding is
// byte-reversed (see Key). length is used verbatim: for
// Blob/XattrSet it is the serialized byte length; for FileNode/DirLeaf/DirNode
// and Commit it is a logical size (see architecture/types.md). Returns ErrReservedType
// if t is not a defined type.
func NewFromHash(t Type, length uint64, fullHash [Size]byte) (Key, error) {
	if !t.IsValid() {
		return Key{}, fmt.Errorf("%w: %d", ErrReservedType, uint8(t))
	}
	ls := lengthSizeFor(length)
	var k Key
	k[headerAt] = byte(t)<<4 | byte(ls-1)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], length)
	copy(k[headerAt-ls:headerAt], buf[:ls])
	hash := k[:headerAt-ls]
	copy(hash, fullHash[:])
	slices.Reverse(hash)
	return k, nil
}
