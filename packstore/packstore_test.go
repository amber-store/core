package packstore

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

func openStore(t *testing.T, dir string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutGetHasRoundTrip(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 50)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range objs {
		has, err := s.Has(o.Key)
		if err != nil || !has {
			t.Fatalf("Has(%s) = %v, %v", o.Key, has, err)
		}
		data, err := s.Get(o.Key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): payload mismatch", o.Key)
		}
	}
}

func TestGetRecordRoundTrip(t *testing.T) {
	// GetRecord returns the on-disk record verbatim — identical to EncodeRecord —
	// for objects in both the active segment and sealed segments. The tiny
	// segment size forces most objects into sealed segments. Payloads mix
	// compressible (stored zstd) and incompressible (stored raw) so both flag
	// paths are exercised.
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(2048))
	var objs []Object
	for i := 0; i < 30; i++ {
		var data []byte
		if i%2 == 0 {
			data = append(compressible(1500), byte(i))
		} else {
			data = append(incompressible(1500), byte(i))
		}
		objs = append(objs, blobObj(t, data))
	}
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range objs {
		want, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRecord(o.Key)
		if err != nil {
			t.Fatalf("GetRecord(%s): %v", o.Key, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("GetRecord(%s): record mismatch (len got %d, want %d)", o.Key, len(got), len(want))
		}
		// The returned record must decode back to the original payload.
		rec, err := amberpack.ParseRecord(got)
		if err != nil {
			t.Fatalf("ParseRecord(%s): %v", o.Key, err)
		}
		payload, err := amberpack.DecodePayload(rec.Flags, rec.Ulen, got[amberpack.RecHeaderSize:])
		if err != nil {
			t.Fatalf("DecodePayload(%s): %v", o.Key, err)
		}
		if !bytes.Equal(payload, o.Data) {
			t.Fatalf("GetRecord(%s): decoded payload mismatch", o.Key)
		}
	}
}

func TestGetRecordAbsentReturnsErrNotFound(t *testing.T) {
	s := openStore(t, t.TempDir())
	if _, err := s.GetRecord(blobObj(t, []byte("nope")).Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoredSizeMatchesRecordPayload(t *testing.T) {
	// StoredSize returns the stored (post-compression) payload length: the
	// record length minus the fixed header, for both active and sealed objects.
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(2048))
	var objs []Object
	for i := 0; i < 30; i++ {
		var data []byte
		if i%2 == 0 {
			data = append(compressible(1500), byte(i))
		} else {
			data = append(incompressible(1500), byte(i))
		}
		objs = append(objs, blobObj(t, data))
	}
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range objs {
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		want := uint64(len(rec) - amberpack.RecHeaderSize)
		got, ok, err := s.StoredSize(o.Key)
		if err != nil || !ok {
			t.Fatalf("StoredSize(%s) = %d, %v, %v", o.Key, got, ok, err)
		}
		if got != want {
			t.Fatalf("StoredSize(%s) = %d, want %d", o.Key, got, want)
		}
	}
	if _, ok, err := s.StoredSize(blobObj(t, []byte("nope")).Key); ok || err != nil {
		t.Fatalf("StoredSize(absent) = ok %v, err %v, want false, nil", ok, err)
	}
}

func TestSortByLocationOrdersByDiskLayout(t *testing.T) {
	// Objects spread across several sealed segments and the active segment. Read
	// keys handed to SortByLocation in a non-disk order must come back ordered by
	// physical layout — grouped by segment, ascending offset within a segment —
	// so a read sweep follows the files instead of jumping around.
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(2048))
	var keys []key.Key
	for i := 0; i < 60; i++ {
		o := blobObj(t, append(incompressible(1500), byte(i), byte(i>>8)))
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	// Reverse so the input is not already in disk order; SortByLocation must fix
	// it regardless of the starting permutation.
	slices.Reverse(keys)

	s.SortByLocation(keys)

	s.mu.RLock()
	defer s.mu.RUnlock()
	var prevSeg, prevOff uint64
	for i, k := range keys {
		seg, off, ok := s.locateLocked(k)
		if !ok {
			t.Fatalf("locate %s: not found", k)
		}
		if i > 0 && (seg < prevSeg || (seg == prevSeg && off < prevOff)) {
			t.Fatalf("out of disk order at %d: (%d,%d) follows (%d,%d)", i, seg, off, prevSeg, prevOff)
		}
		prevSeg, prevOff = seg, off
	}
}

func TestSortByLocationPutsAbsentKeysLast(t *testing.T) {
	s := openStore(t, t.TempDir())
	present := blobObj(t, []byte("here"))
	if err := s.Put(present.Key, present.Data); err != nil {
		t.Fatal(err)
	}
	absent := blobObj(t, []byte("gone")).Key
	keys := []key.Key{absent, present.Key}
	s.SortByLocation(keys)
	if keys[0] != present.Key || keys[1] != absent {
		t.Fatalf("absent key not sorted last: got %v", keys)
	}
}

func TestGetAbsentReturnsErrNotFound(t *testing.T) {
	s := openStore(t, t.TempDir())
	_, err := s.Get(blobObj(t, []byte("nope")).Key)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	has, err := s.Has(blobObj(t, []byte("nope")).Key)
	if err != nil || has {
		t.Fatalf("Has = %v, %v", has, err)
	}
}

func TestPutIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	o := blobObj(t, incompressible(1000))
	for range 3 {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	// Three identical Puts must append exactly one record.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.seg.active"))
	if err != nil || len(files) != 1 {
		t.Fatalf("glob: %v %v", files, err)
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := amberpack.EncodeRecord(o.Key, o.Data)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(magicHeader) + len(rec)); st.Size() != want {
		t.Fatalf("active size = %d, want %d", st.Size(), want)
	}
}

func TestReopenResumesActiveSegment(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	first := testObjects(t, 10)
	for _, o := range first {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openStore(t, dir)
	for _, o := range first {
		data, err := s2.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("after reopen Get(%s): %v", o.Key, err)
		}
	}
	more := blobObj(t, []byte("written after reopen"))
	if err := s2.Put(more.Key, more.Data); err != nil {
		t.Fatal(err)
	}
	if data, err := s2.Get(more.Key); err != nil || !bytes.Equal(data, more.Data) {
		t.Fatalf("Get after resume append: %v", err)
	}
	// Still exactly one active segment file, no sealed ones.
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	actives, _ := filepath.Glob(filepath.Join(dir, "*.seg.active"))
	if len(segs) != 0 || len(actives) != 1 {
		t.Fatalf("segs=%v actives=%v", segs, actives)
	}
}

func TestReopenTruncatesCorruptTail(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	objs := testObjects(t, 5)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a torn write: append garbage to the active file.
	actives, _ := filepath.Glob(filepath.Join(dir, "*.seg.active"))
	f, err := os.OpenFile(actives[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x55, 0x44, 0x33}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	s2 := openStore(t, dir)
	for _, o := range objs {
		if data, err := s2.Get(o.Key); err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after torn-tail recovery: %v", o.Key, err)
		}
	}
	// New writes land cleanly after the truncated tail.
	o := blobObj(t, []byte("post-recovery write"))
	if err := s2.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := openStore(t, dir)
	if data, err := s3.Get(o.Key); err != nil || !bytes.Equal(data, o.Data) {
		t.Fatalf("Get after second reopen: %v", err)
	}
}

func TestSecondOpenFails(t *testing.T) {
	dir := t.TempDir()
	_ = openStore(t, dir)
	if _, err := Open(dir); err == nil {
		t.Fatal("second Open must fail while the first holds the flock")
	}
}

func TestMultipleActiveFilesFailOpen(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0000000000000001.seg.active", "0000000000000002.seg.active"} {
		if err := os.WriteFile(filepath.Join(dir, name), magicHeader, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("want error for two active segments")
	}
}

func TestClosedStoreErrors(t *testing.T) {
	s := openStore(t, t.TempDir())
	o := blobObj(t, []byte("x"))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(o.Key); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	if err := s.Put(o.Key, o.Data); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put after Close: %v", err)
	}
	if _, err := s.Has(o.Key); !errors.Is(err, ErrClosed) {
		t.Fatalf("Has after Close: %v", err)
	}
}

func TestWithSyncFalse(t *testing.T) {
	s := openStore(t, t.TempDir(), WithSync(false))
	o := blobObj(t, incompressible(100))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if data, err := s.Get(o.Key); err != nil || !bytes.Equal(data, o.Data) {
		t.Fatalf("Get: %v", err)
	}
}

func TestConcurrentPutGetHas(t *testing.T) {
	s := openStore(t, t.TempDir(), WithSync(false))
	objs := testObjects(t, 200)

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Overlapping key ranges: every writer writes every other object,
			// so same-key Put races are exercised.
			for i := w % 2; i < len(objs); i += 2 {
				if err := s.Put(objs[i].Key, objs[i].Data); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, o := range objs {
				if _, err := s.Has(o.Key); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Get(o.Key); err != nil && !errors.Is(err, ErrNotFound) {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after concurrent writes: %v", o.Key, err)
		}
	}
}

func TestRotationSealsSegments(t *testing.T) {
	dir := t.TempDir()
	// Tiny threshold + incompressible payloads: every record (~2 KB stored)
	// crosses 1024 bytes, so every Put seals a segment. (testObjects would
	// not work here: its compressible payloads store as ~76-byte records.)
	s := openStore(t, dir, WithSegmentSize(1024))
	var objs []Object
	for i := 0; i < 30; i++ {
		objs = append(objs, blobObj(t, append(incompressible(2000), byte(i), byte(i>>8))))
	}
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) != 30 {
		t.Fatalf("%d sealed segments, want 30", len(segs))
	}
	// All objects must be served from sealed segments.
	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) from sealed: %v", o.Key, err)
		}
	}
	// And survive a reopen.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, dir)
	for _, o := range objs {
		data, err := s2.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after reopen: %v", o.Key, err)
		}
	}
}

func TestRotationMidStreamKeepsAllObjects(t *testing.T) {
	// Mixed compressible/incompressible objects across several rotations:
	// every object must remain reachable from whichever segment it landed in.
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(8<<10))
	objs := testObjects(t, 100)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): %v", o.Key, err)
		}
	}
}

func TestCrashBetweenFooterAndRename(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	objs := testObjects(t, 10)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the crash window: append a valid footer to the .active file
	// but do not rename it.
	actives, _ := filepath.Glob(filepath.Join(dir, "*.seg.active"))
	res, err := scanActive(actives[0])
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]indexEntry, 0, len(res.index))
	for k, loc := range res.index {
		entries = append(entries, indexEntry{k: k, off: uint64(loc.off), slen: loc.slen})
	}
	footer, err := buildFooter(res.size, entries)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(actives[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(footer); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Open must complete the rename and serve everything from the sealed file.
	s2 := openStore(t, dir)
	for _, o := range objs {
		data, err := s2.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): %v", o.Key, err)
		}
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	actives, _ = filepath.Glob(filepath.Join(dir, "*.seg.active"))
	if len(segs) != 1 || len(actives) != 0 {
		t.Fatalf("segs=%v actives=%v", segs, actives)
	}
}

func TestOpenFailsOnCorruptSealedSegment(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(1))
	o := blobObj(t, incompressible(500))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) != 1 {
		t.Fatalf("segs=%v", segs)
	}
	b, err := os.ReadFile(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xFF // trailer magic
	if err := os.WriteFile(segs[0], b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrCorrupt", err)
	}
}

func TestConcurrentRotationReads(t *testing.T) {
	// Rotation under read load: sealing must never surface spurious errors
	// for present keys (the fd may only close after the reader-visible swap).
	s := openStore(t, t.TempDir(), WithSegmentSize(1024))
	var objs []Object
	for i := 0; i < 120; i++ {
		objs = append(objs, blobObj(t, append(incompressible(600), byte(i), byte(i>>8))))
	}
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(objs); i += 2 {
				if err := s.Put(objs[i].Key, objs[i].Data); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 3; round++ {
				for _, o := range objs {
					if _, err := s.Get(o.Key); err != nil && !errors.Is(err, ErrNotFound) {
						t.Errorf("spurious read error during rotation: %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after rotations: %v", o.Key, err)
		}
	}
}

func objSeq(objs []Object, failAfter int) iter.Seq2[Object, error] {
	return func(yield func(Object, error) bool) {
		for i, o := range objs {
			if failAfter >= 0 && i == failAfter {
				yield(Object{}, fmt.Errorf("synthetic iterator error"))
				return
			}
			if !yield(o, nil) {
				return
			}
		}
	}
}

func TestWriteBatchStoresAll(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 100)
	// Duplicate some objects within the batch; they must be written once.
	batch := append(append([]Object{}, objs...), objs[:10]...)
	if err := s.WriteBatch(objSeq(batch, -1)); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): %v", o.Key, err)
		}
	}
}

func TestWriteBatchIteratorError(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 10)
	err := s.WriteBatch(objSeq(objs, 5))
	if err == nil || err.Error() != "synthetic iterator error" {
		t.Fatalf("err = %v", err)
	}
	// The already-appended prefix may remain (documented packstore semantics:
	// durable-on-return, not atomic; valid CAS objects are harmless).
	for _, o := range objs[:5] {
		has, err := s.Has(o.Key)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("prefix object %s lost", o.Key)
		}
	}
}

func TestWriteBatchOnClosedStore(t *testing.T) {
	s := openStore(t, t.TempDir())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	objs := testObjects(t, 3)
	if err := s.WriteBatch(objSeq(objs, -1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if err := s.WriteBatch(objSeq(nil, -1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("empty batch err = %v, want ErrClosed", err)
	}
}

func TestWriteBatchSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(16<<10))
	objs := testObjects(t, 100)
	if err := s.WriteBatch(objSeq(objs, -1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, dir)
	for _, o := range objs {
		data, err := s2.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after reopen: %v", o.Key, err)
		}
	}
}

func TestWriteBatchRotates(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(16<<10))
	objs := testObjects(t, 100)
	if err := s.WriteBatch(objSeq(objs, -1)); err != nil {
		t.Fatal(err)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) == 0 {
		t.Fatal("expected at least one sealed segment")
	}
	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): %v", o.Key, err)
		}
	}
}

func TestWipe(t *testing.T) {
	dir := t.TempDir()
	// Small segments so the store holds sealed segments AND an active one.
	s := openStore(t, dir, WithSegmentSize(1024))
	objs := testObjects(t, 40)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Wipe(); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if _, err := s.Get(o.Key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%s) after Wipe: %v, want ErrNotFound", o.Key, err)
		}
		if has, err := s.Has(o.Key); err != nil || has {
			t.Fatalf("Has(%s) after Wipe: %v, %v", o.Key, has, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".seg") || strings.HasSuffix(e.Name(), ".seg.active") {
			t.Fatalf("segment file %s survived Wipe", e.Name())
		}
	}
	// The store stays usable: writes and reads work after the wipe.
	more := testObjects(t, 5)
	for _, o := range more {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range more {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after post-Wipe Put: %v", o.Key, err)
		}
	}
	// And it survives a reopen.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, dir, WithSegmentSize(1024))
	for _, o := range more {
		if data, err := s2.Get(o.Key); err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after reopen: %v", o.Key, err)
		}
	}
}

func TestWipeWithConcurrentReaders(t *testing.T) {
	s := openStore(t, t.TempDir(), WithSegmentSize(1024))
	objs := testObjects(t, 40)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, o := range objs {
					data, err := s.Get(o.Key)
					switch {
					case err == nil:
						if !bytes.Equal(data, o.Data) {
							t.Error("reader saw corrupt data")
							return
						}
					case errors.Is(err, ErrNotFound):
						// wiped under us; fine
					default:
						t.Errorf("reader: %v", err)
						return
					}
				}
			}
		}()
	}
	if err := s.Wipe(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}

func TestWipeClearsPoisonedWritePath(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 3)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	// Poison the write path the way a failed fsync would.
	s.setFailed(errors.New("simulated fsync failure"))
	if err := s.Put(objs[0].Key, objs[0].Data); err == nil {
		t.Fatal("poisoned store accepted a write")
	}
	// The wipe destroys the data the poison was protecting; writes must work.
	if err := s.Wipe(); err != nil {
		t.Fatal(err)
	}
	more := testObjects(t, 2)
	for _, o := range more {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatalf("Put after Wipe on previously-poisoned store: %v", err)
		}
	}
}

// A failed WriteAt can leave junk past a.size. Sealing must not put the
// footer before it, since parseFooter anchors at EOF.
func TestSealTruncatesStaleBytesPastLogicalEnd(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(4096))
	first := blobObj(t, append(incompressible(1000), 1))
	if err := s.Put(first.Key, first.Data); err != nil {
		t.Fatal(err)
	}
	active, err := filepath.Glob(filepath.Join(dir, "*"+activeSuffix))
	if err != nil || len(active) != 1 {
		t.Fatalf("active segments: %v %v", active, err)
	}
	f, err := os.OpenFile(active[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xEE}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Push the segment over the threshold so this Put seals it.
	second := blobObj(t, append(incompressible(4000), 2))
	if err := s.Put(second.Key, second.Data); err != nil {
		t.Fatalf("sealing Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openStore(t, dir)
	for _, o := range []Object{first, second} {
		data, err := s2.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s) after reopen: %v", o.Key, err)
		}
	}
}
