package amberpack

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"math"
	"math/rand/v2"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/amber-store/core/key"
)

// incompressible returns n deterministic pseudo-random bytes (zstd cannot shrink them).
func incompressible(n int) []byte {
	r := rand.New(rand.NewPCG(42, 7))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint64())
	}
	return b
}

// compressible returns n highly repetitive bytes (zstd shrinks them a lot).
func compressible(n int) []byte {
	return bytes.Repeat([]byte("abcdefgh"), n/8+1)[:n]
}

// zstdDefault is what EncodeRecord did before compression became a choice.
var zstdDefault = Compression{Algorithm: Zstd}

// r0ulen reads the ulen field of a record.
func r0ulen(rec []byte) uint32 { return binary.BigEndian.Uint32(rec[34:38]) }

// fixCRC recomputes a record's CRC after test tampering.
func fixCRC(rec []byte) {
	binary.BigEndian.PutUint32(rec[42:46], 0)
	binary.BigEndian.PutUint32(rec[42:46], crc32.Checksum(rec, castagnoli))
}

func TestRecordRoundTripRaw(t *testing.T) {
	o := mkObj(t, incompressible(4096))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Key != o.Key {
		t.Fatalf("key mismatch: %s != %s", r.Key, o.Key)
	}
	if r.Flags != 0 {
		t.Fatalf("random data must be stored raw, got flags %#x", r.Flags)
	}
	if r.Ulen != r.Slen || int(r.Slen) != len(o.Bytes) {
		t.Fatalf("raw lens: ulen=%d slen=%d want %d", r.Ulen, r.Slen, len(o.Bytes))
	}
	got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:RecHeaderSize+int(r.Slen)])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, o.Bytes) {
		t.Fatal("payload mismatch")
	}
}

func TestRecordRoundTripCompressed(t *testing.T) {
	o := mkObj(t, compressible(64<<10))
	rec, err := EncodeRecordWith(o.Key, o.Bytes, zstdDefault)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Flags != byte(Zstd) {
		t.Fatalf("repetitive data must compress, got flags %#x", r.Flags)
	}
	if r.Slen >= r.Ulen {
		t.Fatalf("compressed slen=%d must be < ulen=%d", r.Slen, r.Ulen)
	}
	got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:RecHeaderSize+int(r.Slen)])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, o.Bytes) {
		t.Fatal("payload mismatch after decompression")
	}
}

func TestRecordEmptyPayload(t *testing.T) {
	o := mkObj(t, nil)
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ulen != 0 || r.Slen != 0 || r.Flags != 0 {
		t.Fatalf("empty payload: ulen=%d slen=%d flags=%#x", r.Ulen, r.Slen, r.Flags)
	}
}

func TestRecordTooLarge(t *testing.T) {
	if !payloadFits(MaxPayload) {
		t.Fatal("payloadFits must accept exactly MaxPayload")
	}
	if payloadFits(MaxPayload + 1) {
		t.Fatal("payloadFits must reject MaxPayload+1")
	}
	if !payloadFits(0) {
		t.Fatal("payloadFits must accept empty payloads")
	}
}

func TestParseRecordRejectsOversizedUlen(t *testing.T) {
	o := mkObj(t, compressible(4096))
	rec, err := EncodeRecordWith(o.Key, o.Bytes, zstdDefault)
	if err != nil {
		t.Fatal(err)
	}
	if rec[33] != byte(Zstd) {
		t.Fatal("test needs a compressed record")
	}
	binary.BigEndian.PutUint32(rec[34:38], math.MaxUint32)
	fixCRC(rec)
	if _, err := ParseRecord(rec); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
}

func TestParseRecordRejectsCorruption(t *testing.T) {
	o := mkObj(t, incompressible(1024))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("truncated header", func(t *testing.T) {
		if _, err := ParseRecord(rec[:RecHeaderSize-1]); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("truncated payload", func(t *testing.T) {
		if _, err := ParseRecord(rec[:len(rec)-1]); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("bad tag", func(t *testing.T) {
		bad := bytes.Clone(rec)
		bad[0] = 0x7F
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("bad flags", func(t *testing.T) {
		bad := bytes.Clone(rec)
		bad[33] = 0x80
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("flipped payload byte fails CRC", func(t *testing.T) {
		bad := bytes.Clone(rec)
		bad[len(bad)-1] ^= 0x01
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("flipped length fails CRC", func(t *testing.T) {
		bad := bytes.Clone(rec)
		bad[39] ^= 0x01 // inside slen, keeps record long enough to parse
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("non-canonical key", func(t *testing.T) {
		// Encode with an invalid type nibble; EncodeRecord does not validate
		// keys (callers supply canonical keys), ParseRecord must.
		var k key.Key
		copy(k[:], o.Key[:])
		k[key.Size-1] = 0xF0 // type 15: reserved
		bad, err := EncodeRecord(k, o.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("raw ulen != slen", func(t *testing.T) {
		bad := bytes.Clone(rec)
		binary.BigEndian.PutUint32(bad[34:38], r0ulen(bad)+1)
		fixCRC(bad)
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
}

// TestParseRecordIgnoresTrailingBytes checks the invariant that ParseRecord is
// called on mmap slices and tail-scan buffers that extend past the current
// record; trailing bytes must not affect the parse result or the CRC.
func TestParseRecordIgnoresTrailingBytes(t *testing.T) {
	o := mkObj(t, incompressible(512))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	extended := append(bytes.Clone(rec), incompressible(1000)...)
	r, err := ParseRecord(extended)
	if err != nil {
		t.Fatal(err)
	}
	if r.Key != o.Key || int(r.Slen) != len(o.Bytes) {
		t.Fatalf("parse over extended buffer: %+v", r)
	}
}

func TestDecodePayloadErrors(t *testing.T) {
	t.Run("bad zstd frame", func(t *testing.T) {
		if _, err := DecodePayload(byte(Zstd), 100, []byte("not a zstd frame")); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
	t.Run("bomb stops at ulen", func(t *testing.T) {
		bomb := zstdEncoder(0).EncodeAll(make([]byte, 64<<20), nil)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodePayload(byte(Zstd), 1024, bomb)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
			t.Fatalf("decoding allocated %d bytes for a 1 KiB ulen", grew)
		}
	})
	t.Run("ulen mismatch", func(t *testing.T) {
		comp := zstdEncoder(0).EncodeAll([]byte("hello world"), nil)
		if _, err := DecodePayload(byte(Zstd), 5, comp); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
	})
}

func TestDecodePayloadRawDoesNotAlias(t *testing.T) {
	stored := []byte{1, 2, 3, 4}
	out, err := DecodePayload(0, 4, stored)
	if err != nil {
		t.Fatal(err)
	}
	out[0] = 99
	if stored[0] != 1 {
		t.Fatal("DecodePayload raw path must copy, not alias")
	}
}

func TestEncodeRecordDefaultIsRaw(t *testing.T) {
	o := mkObj(t, compressible(64<<10))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	with, err := EncodeRecordWith(o.Key, o.Bytes, Compression{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec, with) {
		t.Fatal("EncodeRecord differs from EncodeRecordWith with the zero Compression")
	}
	r, err := ParseRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Flags != 0 || r.Ulen != r.Slen || int(r.Slen) != len(o.Bytes) {
		t.Fatalf("default record: flags=%#x ulen=%d slen=%d, want raw of %d bytes", r.Flags, r.Ulen, r.Slen, len(o.Bytes))
	}
}

func TestEncodeRecordWithRoundTrip(t *testing.T) {
	o := mkObj(t, compressible(64<<10))
	for _, c := range []Compression{
		{Zstd, 0}, {Zstd, 1}, {Zstd, 3}, {Zstd, 9}, {Zstd, 19}, {Zstd, 22},
		{LZ4, 0}, {LZ4, 1}, {LZ4, 6}, {LZ4, 9}, {LZ4, 12},
	} {
		t.Run(c.String(), func(t *testing.T) {
			rec, err := EncodeRecordWith(o.Key, o.Bytes, c)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ParseRecord(rec)
			if err != nil {
				t.Fatal(err)
			}
			if r.Flags != byte(c.Algorithm) {
				t.Fatalf("flags = %#x, want %#x", r.Flags, byte(c.Algorithm))
			}
			if r.Slen >= r.Ulen || int(r.Ulen) != len(o.Bytes) {
				t.Fatalf("ulen=%d slen=%d for %d payload bytes", r.Ulen, r.Slen, len(o.Bytes))
			}
			if len(rec) != RecHeaderSize+int(r.Slen) {
				t.Fatalf("record is %d bytes, header says %d", len(rec), RecHeaderSize+int(r.Slen))
			}
			got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, o.Bytes) {
				t.Fatal("payload mismatch")
			}
		})
	}
}

func TestIncompressiblePayloadFallsBackToRaw(t *testing.T) {
	for _, c := range []Compression{{}, {Zstd, 0}, {Zstd, 19}, {LZ4, 0}, {LZ4, 9}} {
		for _, data := range [][]byte{nil, {7}, incompressible(13), incompressible(4096)} {
			o := mkObj(t, data)
			rec, err := EncodeRecordWith(o.Key, o.Bytes, c)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, len(data), err)
			}
			r, err := ParseRecord(rec)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, len(data), err)
			}
			if r.Flags != 0 || r.Ulen != r.Slen {
				t.Fatalf("%s, %d bytes: flags=%#x ulen=%d slen=%d, want raw", c, len(data), r.Flags, r.Ulen, r.Slen)
			}
			got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:])
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s, %d bytes: round trip failed: %v", c, len(data), err)
			}
		}
	}
}

// TestTinyPayloadsRoundTrip covers payloads around the sizes where a
// compressor's own framing outweighs what it saves. Which codec the record
// ends up with is the encoder's business; that it parses and decodes is not.
func TestTinyPayloadsRoundTrip(t *testing.T) {
	for _, c := range []Compression{{Zstd, 0}, {Zstd, 22}, {LZ4, 0}, {LZ4, 12}} {
		for n := 0; n <= 64; n++ {
			data := bytes.Repeat([]byte{'a'}, n)
			o := mkObj(t, data)
			rec, err := EncodeRecordWith(o.Key, o.Bytes, c)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, n, err)
			}
			r, err := ParseRecord(rec)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, n, err)
			}
			got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:])
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s, %d bytes: round trip failed: %v", c, n, err)
			}
		}
	}
}

func TestEncodeRecordWithRejectsInvalidCompression(t *testing.T) {
	o := mkObj(t, compressible(1024))
	for _, c := range []Compression{{Zstd, 23}, {LZ4, 13}, {None, 1}, {Algorithm(3), 0}} {
		if _, err := EncodeRecordWith(o.Key, o.Bytes, c); !errors.Is(err, ErrInvalidCompression) {
			t.Errorf("%+v: err = %v, want ErrInvalidCompression", c, err)
		}
	}
}

// TestZstdDefaultMatchesV090 pins the bytes EncodeRecord produced up to
// v0.9.0, when zstd at the default level was all it did: asking for zstd at
// level 0 must give the same record. The second input is one on which
// klauspost's four tiers give four different results, so it also pins the
// tier that level 0 maps to. An upgrade of klauspost/compress may change
// these bytes; re-pin them then, from a run at level 3 before the upgrade's
// own changes to this file.
func TestZstdDefaultMatchesV090(t *testing.T) {
	small := bytes.Repeat([]byte("abcdefgh"), 32)
	k, err := key.New(key.Blob, uint64(len(small)), small)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := EncodeRecordWith(k, small, zstdDefault)
	if err != nil {
		t.Fatal(err)
	}
	const want = "016806850136466d9b5bc95a74cd7721416792f8662bfbd6cd3a5c019f5f0001" +
		"0101000001000000001f3ea9f10728b52ffd4400000085000040616263646566" +
		"6768015408032bf505d630077f"
	if got := hex.EncodeToString(rec); got != want {
		t.Fatalf("small record:\n got %s\nwant %s", got, want)
	}

	var b bytes.Buffer
	for i := 0; b.Len() < 64<<10; i++ {
		b.WriteString(strconv.Itoa(i * 7919))
		b.WriteByte(' ')
	}
	big := b.Bytes()[:64<<10]
	if k, err = key.New(key.Blob, uint64(len(big)), big); err != nil {
		t.Fatal(err)
	}
	for _, level := range []int{0, 3} {
		rec, err := EncodeRecordWith(k, big, Compression{Algorithm: Zstd, Level: level})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(rec)
		const wantSum = "51cc65a2ec3525b4e09a60f82f91cce7301328b3a164ee0ce9d63f29a95c6541"
		if len(rec) != 23092 || hex.EncodeToString(sum[:]) != wantSum {
			t.Fatalf("level %d: record of %d bytes, sha256 %x; want 23092 bytes, %s", level, len(rec), sum, wantSum)
		}
	}
}

func TestParseRecordRejectsUnknownCodec(t *testing.T) {
	o := mkObj(t, incompressible(1024))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range []byte{3, 4, 0x80, 0xff} {
		bad := bytes.Clone(rec)
		bad[33] = flags
		fixCRC(bad)
		_, err := ParseRecord(bad)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "unknown record flags") {
			t.Errorf("flags %#x: err = %v, want ErrCorrupt naming unknown record flags", flags, err)
		}
	}
	// A compressed codec on a payload that is not smaller breaks the invariant.
	for _, flags := range []byte{byte(Zstd), byte(LZ4)} {
		bad := bytes.Clone(rec)
		bad[33] = flags
		fixCRC(bad)
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("flags %#x with slen == ulen: err = %v, want ErrCorrupt", flags, err)
		}
	}
}

func TestDecodePayloadLZ4Errors(t *testing.T) {
	data := compressible(4096)
	block := compress(Compression{Algorithm: LZ4}, data)
	if block == nil {
		t.Fatal("test needs an lz4 block")
	}
	if got, err := DecodePayload(byte(LZ4), uint32(len(data)), block); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the block itself must decode: %v", err)
	}
	cases := map[string]struct {
		ulen   uint32
		stored []byte
	}{
		"garbage":            {100, []byte("definitely not lz4 \xff\xff\xff\xff")},
		"truncated block":    {uint32(len(data)), block[:len(block)/2]},
		"empty block":        {uint32(len(data)), nil},
		"shorter than ulen":  {uint32(len(data)) + 1, block},
		"longer than ulen":   {uint32(len(data)) - 1, block},
		"much longer":        {16, block},
		"claims zero length": {0, block},
	}
	for name, c := range cases {
		if _, err := DecodePayload(byte(LZ4), c.ulen, c.stored); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	t.Run("bomb stops at ulen", func(t *testing.T) {
		bomb := compress(Compression{Algorithm: LZ4}, make([]byte, 64<<20))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodePayload(byte(LZ4), 1024, bomb)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Fatalf("decoding allocated %d bytes for a 1 KiB ulen", grew)
		}
	})
}

func TestDecodePayloadRejectsUnknownCodec(t *testing.T) {
	for _, flags := range []byte{3, 0x80, 0xff} {
		if _, err := DecodePayload(flags, 4, []byte{1, 2, 3, 4}); !errors.Is(err, ErrCorrupt) {
			t.Errorf("flags %#x: err = %v, want ErrCorrupt", flags, err)
		}
	}
}
