package amberpack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"

	"github.com/amber-store/core/key"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

const (
	// RecHeaderSize is the fixed record-header length:
	// tag(1) + key(32) + flags(1) + ulen(4) + slen(4) + crc(4). Payload follows.
	// The flags byte holds the payload's codec id, an Algorithm value.
	RecHeaderSize = 46

	tagChunk byte = 0x01

	// MaxPayload bounds one object's payload, stored or decoded. The length
	// fields are untrusted and size allocations. Real objects are ~1 MiB.
	MaxPayload = 256 << 20
)

var (
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	zero4      [4]byte
)

// ErrCorrupt wraps every record-level corruption error surfaced by ParseRecord
// and DecodePayload (bad framing, bad flags, CRC mismatch, length
// inconsistency, non-canonical key). It is the record-level counterpart to the
// stream-level ErrMalformed; distinguish either with errors.Is. The packstore
// package aliases this sentinel for its footer- and scrub-level corruption too,
// so the message stays deliberately general rather than naming "record".
var ErrCorrupt = errors.New("amberpack: corrupt pack data")

// Record describes a parsed record header. The payload lives at
// [RecHeaderSize : RecHeaderSize+Slen] within the record's bytes.
type Record struct {
	Key key.Key
	// Flags is the raw record flag byte; pass it to DecodePayload unchanged.
	Flags byte
	Ulen  uint32
	Slen  uint32
}

// zstdDec decodes every zstd record; DecodeAll is safe for concurrent use.
var zstdDec *zstd.Decoder

func init() {
	var err error
	// CapLimit stops DecodeAll at the dst capacity DecodePayload sizes from ulen.
	if zstdDec, err = zstd.NewReader(nil, zstd.WithDecodeAllCapLimit(true), zstd.WithDecoderMaxMemory(MaxPayload)); err != nil {
		panic(err)
	}
}

// zstdEncs pools the encoders of each klauspost tier. An encoder keeps one
// set of tables per unit of its concurrency for as long as it lives, tens of
// megabytes each at the best tier, so each is built with a concurrency of
// one and pooled: a process holds as many as it has goroutines encoding at
// once, and the pool lets go of the idle ones.
var zstdEncs [zstd.SpeedBestCompression]sync.Pool

// zstdEncodeAll appends data, compressed at a zstd level from 0 to 22, to
// dst. klauspost has four tiers where zstd has 22 levels;
// EncoderLevelFromZstd picks the nearest (1–2 fastest, 3–5 default, 6–9
// better, 10–22 best).
func zstdEncodeAll(level int, data, dst []byte) []byte {
	if level == 0 {
		level = 3 // zstd's own default
	}
	tier := zstd.EncoderLevelFromZstd(level)
	pool := &zstdEncs[tier-zstd.SpeedFastest]
	enc, _ := pool.Get().(*zstd.Encoder)
	if enc == nil {
		var err error
		enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(tier), zstd.WithEncoderConcurrency(1))
		if err != nil {
			panic(err) // the options are fixed and valid
		}
	}
	out := enc.EncodeAll(data, dst)
	pool.Put(enc)
	return out
}

// The lz4 compressors carry hash tables and are not safe for concurrent use,
// so they are pooled. Each resets its tables per block: the output depends
// on the input alone.
var (
	lz4Fast = sync.Pool{New: func() any { return new(lz4.Compressor) }}
	lz4HC   = sync.Pool{New: func() any { return new(lz4.CompressorHC) }}
)

// compress returns data compressed as c says, or nil when the payload is to
// be stored raw: c is None, data is empty, the result would not be strictly
// smaller, or the compressor reports data incompressible. c must be valid.
func compress(c Compression, data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	switch c.Algorithm {
	case Zstd:
		if out := zstdEncodeAll(c.Level, data, make([]byte, 0, len(data))); len(out) < len(data) {
			return out
		}
	case LZ4:
		// One byte short of data: a block that is not strictly smaller does
		// not fit, which the compressor reports with 0 or an error.
		dst := make([]byte, len(data)-1)
		var n int
		var err error
		if c.Level == 0 {
			lc := lz4Fast.Get().(*lz4.Compressor)
			n, err = lc.CompressBlock(data, dst)
			lz4Fast.Put(lc)
		} else {
			hc := lz4HC.Get().(*lz4.CompressorHC)
			// The HC levels here stop at 9; level n is a search depth of 1<<(8+n).
			hc.Level = lz4.CompressionLevel(1 << (8 + min(c.Level, 9)))
			n, err = hc.CompressBlock(data, dst)
			lz4HC.Put(hc)
		}
		if err == nil && n > 0 {
			return dst[:n]
		}
	}
	return nil
}

// EncodeRecord serializes (k, data) into a complete record with the payload
// stored raw. It is EncodeRecordWith with no compression.
func EncodeRecord(k key.Key, data []byte) ([]byte, error) {
	return EncodeRecordWith(k, data, Compression{})
}

// EncodeRecordWith serializes (k, data) into a complete record, compressing
// the payload as c says when that makes it strictly smaller and storing it
// raw otherwise. An invalid c is an error wrapping ErrInvalidCompression. k
// is written as given; canonical-form validation happens on the read side.
func EncodeRecordWith(k key.Key, data []byte, c Compression) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if !payloadFits(len(data)) {
		return nil, fmt.Errorf("amberpack: object %s too large: %d bytes", k, len(data))
	}
	payload, flags := data, byte(None)
	if comp := compress(c, data); comp != nil {
		payload, flags = comp, byte(c.Algorithm)
	}
	rec := make([]byte, RecHeaderSize+len(payload))
	rec[0] = tagChunk
	copy(rec[1:33], k[:])
	rec[33] = flags
	binary.BigEndian.PutUint32(rec[34:38], uint32(len(data)))
	binary.BigEndian.PutUint32(rec[38:42], uint32(len(payload)))
	copy(rec[RecHeaderSize:], payload)
	// CRC over the whole record; the crc field itself is still zero here.
	binary.BigEndian.PutUint32(rec[42:46], crc32.Checksum(rec, castagnoli))
	return rec, nil
}

// payloadFits reports whether a payload of n bytes is within MaxPayload. Split
// out of EncodeRecord so the bound is testable without allocating it.
func payloadFits(n int) bool {
	return n <= MaxPayload
}

// ParseRecord validates the record at the start of b (which may extend past it)
// and returns its header. It checks framing, flags, key canonicality, and the
// CRC, without mutating b (b may be a read-only mmap).
func ParseRecord(b []byte) (Record, error) {
	if len(b) < RecHeaderSize {
		return Record{}, fmt.Errorf("%w: truncated record header", ErrCorrupt)
	}
	if b[0] != tagChunk {
		return Record{}, fmt.Errorf("%w: unexpected record tag %#x", ErrCorrupt, b[0])
	}
	flags := b[33]
	if flags > byte(LZ4) {
		return Record{}, fmt.Errorf("%w: unknown record flags %#x", ErrCorrupt, flags)
	}
	ulen := binary.BigEndian.Uint32(b[34:38])
	slen := binary.BigEndian.Uint32(b[38:42])
	if int64(len(b)) < RecHeaderSize+int64(slen) {
		return Record{}, fmt.Errorf("%w: truncated record payload", ErrCorrupt)
	}
	if flags == byte(None) && ulen != slen {
		return Record{}, fmt.Errorf("%w: raw record with ulen %d != slen %d", ErrCorrupt, ulen, slen)
	}
	if ulen > MaxPayload {
		return Record{}, fmt.Errorf("%w: record ulen %d exceeds limit %d", ErrCorrupt, ulen, MaxPayload)
	}
	if flags != byte(None) && slen >= ulen {
		return Record{}, fmt.Errorf("%w: compressed record with slen %d >= ulen %d", ErrCorrupt, slen, ulen)
	}
	c := crc32.Update(0, castagnoli, b[:42])
	c = crc32.Update(c, castagnoli, zero4[:])
	c = crc32.Update(c, castagnoli, b[RecHeaderSize:RecHeaderSize+int(slen)])
	if c != binary.BigEndian.Uint32(b[42:46]) {
		return Record{}, fmt.Errorf("%w: record CRC mismatch", ErrCorrupt)
	}
	k, err := key.Parse(b[1:33])
	if err != nil {
		return Record{}, fmt.Errorf("%w: record key: %v", ErrCorrupt, err)
	}
	return Record{Key: k, Flags: flags, Ulen: ulen, Slen: slen}, nil
}

// DecodePayload returns caller-owned payload bytes from a record's stored
// payload, decoded as the record's flags byte says. stored may be a read-only
// mmap slice and is never retained.
func DecodePayload(flags byte, ulen uint32, stored []byte) ([]byte, error) {
	switch Algorithm(flags) {
	case None:
		out := make([]byte, len(stored))
		copy(out, stored)
		return out, nil
	case Zstd:
		out, err := zstdDec.DecodeAll(stored, make([]byte, 0, ulen))
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
		}
		if uint32(len(out)) != ulen {
			return nil, fmt.Errorf("%w: decompressed to %d bytes, header says %d", ErrCorrupt, len(out), ulen)
		}
		return out, nil
	case LZ4:
		// The block carries no length of its own: ulen sizes the output, and
		// a block that would run past it fails inside the decoder.
		out := make([]byte, ulen)
		n, err := lz4.UncompressBlock(stored, out)
		if err != nil {
			return nil, fmt.Errorf("%w: lz4: %v", ErrCorrupt, err)
		}
		if uint32(n) != ulen {
			return nil, fmt.Errorf("%w: decompressed to %d bytes, header says %d", ErrCorrupt, n, ulen)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: unknown record flags %#x", ErrCorrupt, flags)
}
