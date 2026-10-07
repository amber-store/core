// Package packstore persists Amber-Store CAS objects in log-structured,
// append-only segment (pack) files. Sealed segments are immutable, mmap'd
// whole, and self-indexed by a footer (fanout index on the first key byte +
// binary fuse filter + fixed trailer). An active segment is indexed in its
// owner's memory and, for everybody else and for the next open, by a sidecar
// file beside it (sidecar.go). There is no global index. A directory may be
// open in any number of stores, in any number of processes: a writer owns an
// active segment of its own (active.go), readers lock nothing and look at
// the directory again when they miss (view.go), and one lock file keeps
// writers and a GC sweep apart (gate.go). All format integers are big-endian.
// Record framing lives in the amberpack package. See
// architecture/packstore.md.
package packstore

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

const (
	tagSeal   byte = 0xF0 // first byte of the footer
	tagDelete byte = 0x02 // reserved for v2 GC; never written in v1
)

// Segment format versions: the last byte of a segment's header. The two have
// the same layout; the version says which codecs the segment's records may
// use. A segment at versionBase holds only raw and zstd records, which is all
// that releases up to 0.9.0 read. Those releases refuse a segment at any
// other version, and would misread an lz4 record in one they accept, so an
// lz4 record is only ever written to a segment at versionLZ4. A later codec
// takes a later version in the same way: a release must refuse the segments
// whose records it cannot read (architecture/packstore.md).
const (
	versionBase byte = 2
	versionLZ4  byte = 3
)

var (
	magicHeader  = []byte("AMBERSG\x02") // a header at versionBase; the last byte is the format version
	magicTrailer = []byte("AMBERSGF")
	castagnoli   = crc32.MakeTable(crc32.Castagnoli) // footer CRC; record CRC lives in amberpack
)

// ErrCorrupt wraps every structural-corruption error (bad record framing, bad
// footer, scrub findings). It aliases amberpack's record-corruption sentinel so
// a single errors.Is target covers both record- and footer-level corruption.
var ErrCorrupt = amberpack.ErrCorrupt

// ErrUnsupportedVersion reports a segment whose header is this format's magic
// with another version byte: data written by a release with a different
// layout. Such a file is neither read nor modified.
var ErrUnsupportedVersion = errors.New("packstore: unsupported segment format version")

// checkVersion returns ErrUnsupportedVersion when b starts with the segment
// magic of another format version, and nil otherwise: a missing, torn or
// foreign header is the caller's to judge.
func checkVersion(b []byte) error {
	n := len(magicHeader) - 1
	if len(b) <= n || !bytes.Equal(b[:n], magicHeader[:n]) || readsVersion(b[n]) {
		return nil
	}
	return fmt.Errorf("%w: %d, this release reads %d and %d", ErrUnsupportedVersion, b[n], versionBase, versionLZ4)
}

func readsVersion(v byte) bool { return v == versionBase || v == versionLZ4 }

// isHeader reports whether b is a segment header of a version this release
// reads.
func isHeader(b []byte) bool {
	n := len(magicHeader) - 1
	return len(b) == len(magicHeader) && bytes.Equal(b[:n], magicHeader[:n]) && readsVersion(b[n])
}

// headerAt returns the segment header for a format version.
func headerAt(version byte) []byte {
	h := bytes.Clone(magicHeader)
	h[len(h)-1] = version
	return h
}

// versionFor returns the lowest segment version that may hold a record with
// the given flags byte.
func versionFor(flags byte) byte {
	if amberpack.Algorithm(flags) > amberpack.Zstd {
		return versionLZ4
	}
	return versionBase
}

// Object is one CAS object to store: its key and either its serialized
// bytes (Data) or, for an object that was encoded elsewhere, the complete
// record as amberpack.EncodeRecordWith produced it (Record). Exactly one of
// the two is set. A Record is parsed (framing, CRC, canonical key, key
// equal to Key) and appended verbatim, so a caller that already holds
// encoded records, say a pack it staged on disk, skips the compression
// round trip; with WriteOpts.Verify its payload is decoded and rehashed
// like Data is.
type Object struct {
	Key    key.Key
	Data   []byte
	Record []byte
}
