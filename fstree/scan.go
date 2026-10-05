package fstree

import (
	"bytes"
	"math"

	"github.com/fxamacker/cbor/v2"
)

// Name lookups in DirLeaf and DirNode bodies, without decoding them.
//
// DecodeDirLeaf and DecodeDirNode check a whole body for well-formedness and
// then allocate every entry of it, to find one name. A scan finds the name in
// one pass with no allocation. It answers only for the canonical form the
// encoders write (encode.go). The decoders take much more than that: other
// integer widths, indefinite lengths, tags, nulls, keys in any order. Any such
// body, and every malformed one, is scanUnsure and goes to the full decoder,
// so results, errors and error text stay what the decoder gives.

// scanMaxItems is fxamacker's default MaxArrayElements: the decoders refuse a
// longer array.
const scanMaxItems = 131072

// scanResult is what a scan can say about a name.
type scanResult uint8

const (
	// scanUnsure: the body is not in the canonical form. Ask the full decoder.
	scanUnsure scanResult = iota
	// scanFound: the name is in the DirLeaf, or the DirNode has a child that
	// may hold it.
	scanFound
	// scanMissing: the body is canonical and the directory has no such name.
	scanMissing
)

// scanCursor reads canonical CBOR items off b. Every read reports false on
// anything else, and never reads past the end of b.
type scanCursor struct {
	b   []byte
	off int
}

// head reads one item head: its major type and its argument, which must be in
// the shortest form.
func (c *scanCursor) head() (major byte, arg uint64, ok bool) {
	if c.off >= len(c.b) {
		return 0, 0, false
	}
	ib := c.b[c.off]
	c.off++
	major, ai := ib>>5, ib&0x1f
	if ai < 24 {
		return major, uint64(ai), true
	}
	var n int        // bytes of argument that follow
	var least uint64 // the smallest argument that needs this many
	switch ai {
	case 24:
		n, least = 1, 24
	case 25:
		n, least = 2, 1<<8
	case 26:
		n, least = 4, 1<<16
	case 27:
		n, least = 8, 1<<32
	default: // 28-30 are reserved, 31 is an indefinite length
		return 0, 0, false
	}
	if len(c.b)-c.off < n {
		return 0, 0, false
	}
	for _, x := range c.b[c.off : c.off+n] {
		arg = arg<<8 | uint64(x)
	}
	c.off += n
	if arg < least {
		return 0, 0, false
	}
	return major, arg, true
}

// uint reads an unsigned integer.
func (c *scanCursor) uint() (uint64, bool) {
	major, v, ok := c.head()
	return v, ok && major == 0
}

// int reads an integer that fits an int64.
func (c *scanCursor) int() bool {
	major, v, ok := c.head()
	return ok && major <= 1 && v <= math.MaxInt64
}

// bytes reads a byte string. The result aliases c.b.
func (c *scanCursor) bytes() ([]byte, bool) {
	major, n, ok := c.head()
	if !ok || major != 2 || n > uint64(len(c.b)-c.off) {
		return nil, false
	}
	s := c.b[c.off : c.off+int(n)]
	c.off += int(n)
	return s, true
}

// array reads the head of an array the decoders would take, and returns its
// length.
func (c *scanCursor) array() (int, bool) {
	major, n, ok := c.head()
	if !ok || major != 4 || n > scanMaxItems {
		return 0, false
	}
	return int(n), true
}

// uints reads an array of unsigned integers.
func (c *scanCursor) uints() bool {
	n, ok := c.array()
	if !ok {
		return false
	}
	for range n {
		if _, ok := c.uint(); !ok {
			return false
		}
	}
	return true
}

// done reports whether all of b was read.
func (c *scanCursor) done() bool { return c.off == len(c.b) }

// scanLeaf looks name up in the DirLeaf body b. On scanFound, entry is the
// encoding of the one entry map with that name, a part of b.
func scanLeaf(b, name []byte) (entry []byte, res scanResult) {
	c := scanCursor{b: b}
	n, ok := c.array()
	if !ok {
		return nil, scanUnsure
	}
	var prev []byte
	found := false
	for i := range n {
		start := c.off
		// Keys 0-4 are always written, 5-9 when they are set.
		major, pairs, ok := c.head()
		if !ok || major != 5 || pairs < 5 || pairs > 10 {
			return nil, scanUnsure
		}
		if k, ok := c.uint(); !ok || k != 0 {
			return nil, scanUnsure
		}
		ename, ok := c.bytes()
		if !ok {
			return nil, scanUnsure
		}
		// The decoder's caller binary-searches the names, so the scan
		// answers only when they strictly ascend.
		if i > 0 && bytes.Compare(prev, ename) >= 0 {
			return nil, scanUnsure
		}
		prev = ename
		last := uint64(0)
		for range pairs - 1 {
			k, ok := c.uint()
			if !ok || k <= last {
				return nil, scanUnsure
			}
			last = k
			switch k {
			case 1, 2, 3:
				_, ok = c.uint()
			case 4:
				ok = c.int()
			case 5, 6, 9:
				_, ok = c.bytes()
			case 7:
				ok = c.uints()
			default:
				// 8: inline xattrs are raw CBOR the decoder validates.
				// Above 9: not a key of an entry.
				ok = false
			}
			if !ok {
				return nil, scanUnsure
			}
		}
		if bytes.Equal(ename, name) {
			entry, found = b[start:c.off], true
		}
	}
	if !c.done() {
		return nil, scanUnsure
	}
	if !found {
		return nil, scanMissing
	}
	return entry, scanFound
}

// scanLeafEntry returns the entry called name in the DirLeaf body b. Only that
// entry goes through the decoder, so it is the value DecodeDirLeaf gives for
// it: its byte slices are copies, and nothing in it aliases b.
func scanLeafEntry(b, name []byte) (Entry, scanResult) {
	enc, res := scanLeaf(b, name)
	if res != scanFound {
		return Entry{}, res
	}
	var e Entry
	if err := cbor.Unmarshal(enc, &e); err != nil {
		return Entry{}, scanUnsure
	}
	return e, scanFound
}

// scanNode looks name up in the DirNode body b. On scanFound, child is the
// child key of the first pair whose separator is not below name, a part of b:
// the only subtree that can hold name.
func scanNode(b, name []byte) (child []byte, res scanResult) {
	c := scanCursor{b: b}
	n, ok := c.array()
	if !ok {
		return nil, scanUnsure
	}
	var prev []byte
	found := false
	for i := range n {
		if major, arg, ok := c.head(); !ok || major != 4 || arg != 2 {
			return nil, scanUnsure
		}
		sep, ok := c.bytes()
		if !ok {
			return nil, scanUnsure
		}
		ck, ok := c.bytes()
		if !ok {
			return nil, scanUnsure
		}
		if i > 0 && bytes.Compare(prev, sep) >= 0 {
			return nil, scanUnsure
		}
		prev = sep
		if !found && bytes.Compare(sep, name) >= 0 {
			child, found = ck, true
		}
	}
	if !c.done() {
		return nil, scanUnsure
	}
	if !found {
		return nil, scanMissing
	}
	return child, scanFound
}
