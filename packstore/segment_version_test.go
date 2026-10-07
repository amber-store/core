package packstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// segmentRecords reads a segment file, sealed or active, and returns the
// version byte of its header and the codec of each record, in file order.
func segmentRecords(t *testing.T, path string) (version byte, codecs []amberpack.Algorithm) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 8 || string(b[:7]) != "AMBERSG" {
		t.Fatalf("%s: no segment header", path)
	}
	// A record starts with its tag, 0x01; a footer starts with another byte.
	for off := 8; off < len(b) && b[off] == 0x01; {
		rec, err := amberpack.ParseRecord(b[off:])
		if err != nil {
			t.Fatalf("%s: record at offset %d: %v", path, off, err)
		}
		codecs = append(codecs, amberpack.Algorithm(rec.Flags))
		off += amberpack.RecHeaderSize + int(rec.Slen)
	}
	return b[7], codecs
}

// versionCount is what segmentVersions reports for one format version.
type versionCount struct {
	segments int // segment files at this version, sealed and active
	lz4      int // records beyond zstd that they hold
}

// segmentVersions walks every segment in dir and checks the gate's rule: a
// version-2 segment holds no record beyond zstd. It returns the counts per
// version.
func segmentVersions(t *testing.T, dir string) map[byte]versionCount {
	t.Helper()
	out := map[byte]versionCount{}
	for _, suffix := range []string{sealedSuffix, activeSuffix} {
		paths, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			v, codecs := segmentRecords(t, p)
			c := out[v]
			c.segments++
			for _, a := range codecs {
				if a <= amberpack.Zstd {
					continue
				}
				c.lz4++
				if v == 2 {
					t.Fatalf("%s is a version-2 segment and holds an %s record", filepath.Base(p), a)
				}
			}
			out[v] = c
		}
	}
	return out
}

// switchable returns a callback that picks lz4 while on is set and the
// store's setting otherwise.
func switchable(on *atomic.Bool) CompressionFunc {
	return func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
		if on.Load() {
			return lz4Fast
		}
		return def
	}
}

func TestSegmentsStayAtVersion2WithoutLZ4(t *testing.T) {
	for _, c := range []amberpack.Compression{{}, zstdDefault, zstd19} {
		dir := t.TempDir()
		s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false), WithCompression(c))
		if err != nil {
			t.Fatal(err)
		}
		putAll(t, s, testObjects(t, 40))
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		got := segmentVersions(t, dir)
		if len(got) != 1 || got[2].segments < 2 {
			t.Fatalf("%s: segments per version = %+v, want several, all at version 2", c, got)
		}
	}
}

func TestLZ4HandleWritesVersion3FromTheStart(t *testing.T) {
	dir := t.TempDir()
	objs := testObjects(t, 40)
	s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false), WithCompression(lz4Fast))
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, s, objs)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got := segmentVersions(t, dir)
	if len(got) != 1 || got[3].segments < 2 || got[3].lz4 == 0 {
		t.Fatalf("segments per version = %+v, want several, all at version 3, with lz4 records", got)
	}
	// A handle on the default reads and scrubs them.
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, o := range objs {
		mustGet(t, r, o)
	}
	if err := r.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFirstLZ4RecordMovesTheHandleToVersion3(t *testing.T) {
	var lz4On atomic.Bool
	dir := t.TempDir()
	s, err := Open(dir, WithSync(false), WithCompression(zstdDefault), WithCompressionFor(switchable(&lz4On)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	objs := distinct(t, 5)
	put := func(o Object) {
		t.Helper()
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string, v2, v3 versionCount) {
		t.Helper()
		got := segmentVersions(t, dir)
		if got[2] != v2 || got[3] != v3 || len(got) > 2 {
			t.Fatalf("%s: segments per version = %+v, want version 2 %+v and version 3 %+v", when, got, v2, v3)
		}
	}

	put(objs[0])
	put(objs[1])
	check("two zstd records", versionCount{segments: 1}, versionCount{})

	lz4On.Store(true)
	put(objs[2])
	check("the first lz4 record", versionCount{segments: 1}, versionCount{segments: 1, lz4: 1})
	if sealed, _ := filepath.Glob(filepath.Join(dir, "*"+sealedSuffix)); len(sealed) != 1 {
		t.Fatalf("sealed segments = %v, want the version-2 one the handle left", sealed)
	}

	// A zstd record after that goes on in the version-3 segment.
	lz4On.Store(false)
	put(objs[3])
	check("a zstd record after it", versionCount{segments: 1}, versionCount{segments: 1, lz4: 1})

	// And the handle's next segment is at version 3 as well, whatever it
	// starts with: it pays for one early seal, not for one per segment.
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	put(objs[4])
	check("the next segment", versionCount{segments: 1}, versionCount{segments: 2, lz4: 1})

	for _, o := range objs {
		mustGet(t, s, o)
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPreEncodedLZ4RecordMovesTheHandleToVersion3(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithSync(false), WithCompression(zstdDefault))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	objs := distinct(t, 2)
	if err := s.Put(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	rec, err := amberpack.EncodeRecordWith(objs[1].Key, objs[1].Data, lz4Fast)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatch(objSeq([]Object{{Key: objs[1].Key, Record: rec}}, -1)); err != nil {
		t.Fatal(err)
	}
	got := segmentVersions(t, dir)
	if got[2] != (versionCount{segments: 1}) || got[3] != (versionCount{segments: 1, lz4: 1}) {
		t.Fatalf("segments per version = %+v", got)
	}
	for _, o := range objs {
		mustGet(t, s, o)
	}
}

func TestCompactionKeepsLZ4RecordsInVersion3Segments(t *testing.T) {
	dir := t.TempDir()
	objs := testObjects(t, 40)
	w, err := Open(dir, WithSegmentSize(8<<10), WithSync(false), WithCompression(lz4Fast))
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, w, objs)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// The compacting handle never asked for lz4: what it copies decides.
	s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := func(k key.Key) bool { return k[0]%2 == 0 }
	stats, err := s.Compact(live, CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied == 0 {
		t.Fatalf("the pass copied nothing: %+v", stats)
	}
	got := segmentVersions(t, dir) // fails on an lz4 record in a version-2 segment
	if got[3].lz4 == 0 {
		t.Fatalf("no lz4 record survived the pass: %+v", got)
	}
	for _, o := range objs {
		if live(o.Key) {
			mustGet(t, s, o)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyVersion2ActiveIsLetGoNotRewritten(t *testing.T) {
	var lz4On atomic.Bool
	dir := t.TempDir()
	s, err := Open(dir, WithSync(false), WithCompression(zstdDefault), WithCompressionFor(switchable(&lz4On)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Own an active segment with nothing in it, as a store does after
	// adopting one that nobody wrote to.
	if err := s.ensureActiveLocked(); err != nil {
		t.Fatal(err)
	}
	empty := s.active.path
	before, err := os.ReadFile(empty)
	if err != nil || len(before) != len(magicHeader) || before[7] != 2 {
		t.Fatalf("the empty segment: %q, %v", before, err)
	}

	lz4On.Store(true)
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	// A running older process may already have read that header: the
	// segment is left as it is, for a writer that can use it.
	if after, err := os.ReadFile(empty); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the empty version-2 segment was rewritten or removed: %q, %v", after, err)
	}
	got := segmentVersions(t, dir)
	if got[2] != (versionCount{segments: 1}) || got[3] != (versionCount{segments: 1, lz4: 1}) {
		t.Fatalf("segments per version = %+v", got)
	}
	mustGet(t, s, o)
}

func TestAdoptionRespectsTheSegmentVersion(t *testing.T) {
	objs := distinct(t, 2)
	writeOne := func(dir string, c amberpack.Compression, o Object) {
		t.Helper()
		s, err := Open(dir, WithSync(false), WithCompression(c))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an lz4 handle leaves a version-2 segment alone", func(t *testing.T) {
		dir := t.TempDir()
		writeOne(dir, zstdDefault, objs[0])
		orphan := onlyActive(t, dir)
		writeOne(dir, lz4Fast, objs[1])
		if v, codecs := segmentRecords(t, orphan); v != 2 || len(codecs) != 1 || codecs[0] != amberpack.Zstd {
			t.Fatalf("the version-2 segment now reads version %d, records %v", v, codecs)
		}
		got := segmentVersions(t, dir)
		if got[2] != (versionCount{segments: 1}) || got[3] != (versionCount{segments: 1, lz4: 1}) {
			t.Fatalf("segments per version = %+v", got)
		}
	})

	t.Run("a zstd handle adopts a version-3 segment", func(t *testing.T) {
		dir := t.TempDir()
		writeOne(dir, lz4Fast, objs[0])
		orphan := onlyActive(t, dir)
		writeOne(dir, zstdDefault, objs[1])
		v, codecs := segmentRecords(t, orphan)
		if v != 3 || len(codecs) != 2 || codecs[0] != amberpack.LZ4 || codecs[1] != amberpack.Zstd {
			t.Fatalf("the version-3 segment reads version %d, records %v; want it adopted and extended", v, codecs)
		}
		if got := segmentVersions(t, dir); len(got) != 1 {
			t.Fatalf("segments per version = %+v, want only the adopted one", got)
		}
	})
}

func TestRepairRaisesTheVersionOnlyForAnLZ4Replacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		lz4  bool
		want byte
	}{
		{"a zstd replacement keeps version 2", false, 2},
		{"an lz4 replacement raises it to 3", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lz4On atomic.Bool
			s := rawStore(t, WithCompression(zstdDefault), WithCompressionFor(switchable(&lz4On)))
			o := blobObj(t, compressible(8192))
			if err := s.Put(o.Key, o.Data); err != nil {
				t.Fatal(err)
			}
			off := s.active.index[o.Key].off
			if err := s.sealActiveLocked(); err != nil {
				t.Fatal(err)
			}
			path := s.sealed[0].path
			damageRepairRecord(t, path, off+amberpack.RecHeaderSize)
			lz4On.Store(tc.lz4)
			if err := s.PutVerified(o.Key, o.Data); err != nil {
				t.Fatal(err)
			}
			v, codecs := segmentRecords(t, path) // a repair keeps the segment's name
			wantCodec := amberpack.Zstd
			if tc.lz4 {
				wantCodec = amberpack.LZ4
			}
			if v != tc.want || len(codecs) != 1 || codecs[0] != wantCodec {
				t.Fatalf("the repaired segment reads version %d, records %v; want version %d, [%s]", v, codecs, tc.want, wantCodec)
			}
			mustGet(t, s, o)
			if err := s.Verify(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifyRejectsAnLZ4RecordInAVersion2Segment(t *testing.T) {
	// Built by hand: no writer of this release produces it.
	o := blobObj(t, compressible(4096))
	rec, err := amberpack.EncodeRecordWith(o.Key, o.Data, lz4Fast)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version byte
		corrupt bool
	}{{2, true}, {3, false}} {
		body := append(bytes.Clone(magicHeader), rec...)
		body[7] = tc.version
		entry := indexEntry{k: o.Key, off: uint64(len(magicHeader)), slen: uint32(len(rec) - amberpack.RecHeaderSize)}
		footer, err := buildFooter(int64(len(body)), []indexEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "0000000000000001.seg"), append(body, footer...), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("version %d: %v", tc.version, err)
		}
		mustGet(t, s, o) // reading decodes by the record's own codec
		err = s.Verify(context.Background())
		if tc.corrupt != errors.Is(err, ErrCorrupt) || (!tc.corrupt && err != nil) {
			t.Fatalf("version %d: Verify = %v; an lz4 record is corruption in a version-2 segment only", tc.version, err)
		}
		s.Close()
	}
}

func TestOpenRefusesAVersionBeyondTheOnesItReads(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithSegmentSize(4<<10), WithSync(false), WithCompression(lz4Fast))
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, s, testObjects(t, 20))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := filepath.Glob(filepath.Join(dir, "*"+sealedSuffix))
	if err != nil || len(sealed) == 0 {
		t.Fatalf("sealed segments: %v, %v", sealed, err)
	}
	b, err := os.ReadFile(sealed[0])
	if err != nil {
		t.Fatal(err)
	}
	b[7] = 4
	if err := os.WriteFile(sealed[0], b, 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); !errors.Is(err, ErrUnsupportedVersion) {
		if err == nil {
			s.Close()
		}
		t.Fatalf("Open: err = %v, want ErrUnsupportedVersion", err)
	}
}
