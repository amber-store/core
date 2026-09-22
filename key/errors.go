package key

import "errors"

// Sentinel errors returned by Parse, Validate, and the constructors. Wrap them
// with %w and match with errors.Is.
var (
	// ErrBadKeyLength means the input to Parse was not exactly Size bytes.
	ErrBadKeyLength = errors.New("key: data is not 32 bytes")
	// ErrReservedBitSet means the header's reserved bit (bit 3) is set.
	ErrReservedBitSet = errors.New("key: reserved header bit is set")
	// ErrReservedType means the object type is reserved (6..15) or out of range.
	ErrReservedType = errors.New("key: reserved object type")
	// ErrNonCanonicalLength means the length field has leading-zero padding.
	ErrNonCanonicalLength = errors.New("key: non-canonical length encoding")
)
