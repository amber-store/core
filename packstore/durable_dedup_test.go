package packstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// ownCopy reports whether the store's own active segment holds k.
func ownCopy(s *Store, k key.Key) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return false
	}
	_, ok := s.active.index[k]
	return ok
}

// A store that syncs acknowledges a write only when losing power would not
// lose the object. A copy in another store's active segment counts as a
// duplicate only as far as that store has synced it.
func TestDedupDoesNotRelyOnAnotherStoresUnsyncedRecords(t *testing.T) {
	o := testObjects(t, 1)[0]

	dir := t.TempDir()
	putAll(t, openStore(t, dir, WithSync(false)), []Object{o}) // written, never synced, its writer still open
	s := openStore(t, dir)                                     // syncs
	wantObjects(t, s, []Object{o})
	putAll(t, s, []Object{o})
	if !ownCopy(s, o.Key) {
		t.Fatal("a syncing store acknowledged a write whose only copy another store has yet to sync")
	}

	dir = t.TempDir()
	putAll(t, openStore(t, dir), []Object{o}) // written and synced
	s = openStore(t, dir)
	wantObjects(t, s, []Object{o})
	putAll(t, s, []Object{o})
	if ownCopy(s, o.Key) {
		t.Fatal("a second copy was written of a record another store had synced")
	}
}

// Compaction must not delete a synced copy on the strength of one that a live
// writer has yet to sync.
func TestCompactDoesNotLeaveTheOnlyCopyUnsynced(t *testing.T) {
	fill := func(tag byte) Object {
		data := incompressible(4 << 10)
		data[0] = tag
		return blobObj(t, data)
	}
	kept, garbage := fill(1), fill(2)

	dir := t.TempDir()
	putAll(t, openStore(t, dir, WithSync(false)), []Object{kept}) // a live writer that never syncs

	s := openStore(t, dir, WithSegmentSize(8<<10)) // syncs
	rec, err := amberpack.EncodeRecordWith(kept.Key, kept.Data, zstdDefault)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRecord(kept.Key, rec); err != nil { // a copy of its own, whatever the duplicate check thinks
		t.Fatal(err)
	}
	putAll(t, s, []Object{garbage}) // fills the segment: sealed, and synced
	if _, err := s.Compact(func(k key.Key) bool { return k == kept.Key }, CompactOpts{MinDeadRatio: 0.1}); err != nil {
		t.Fatal(err)
	}

	s.mu.RLock()
	sealedCopy := false
	for _, g := range s.sealed {
		sealedCopy = sealedCopy || g.has(kept.Key)
	}
	s.mu.RUnlock()
	if !sealedCopy && !ownCopy(s, kept.Key) {
		t.Fatal("the pass deleted a synced copy and left only the one another store has yet to sync")
	}
}

// killedWriter returns a directory in the state a writer's death leaves: its
// active segment holding objs, appended and never synced, and nobody holding its
// locks. A copy of the directory is that state exactly: the files are there,
// nothing has fsynced them.
func killedWriter(t *testing.T, objs []Object) string {
	t.Helper()
	src := t.TempDir()
	w, err := Open(src) // a store that syncs, whose deferred puts do not yet
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, o := range objs {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	dst := t.TempDir()
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// durableMark is how much of the store's active segment its sidecar knows durable.
func durableMark(t *testing.T, s *Store) int64 {
	t.Helper()
	side, err := os.ReadFile(s.active.path + sidecarSuffix)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := readSidecar(side, true)
	var durable int64
	for _, r := range recs {
		if r.kind == sidecarSynced {
			durable = max(durable, int64(r.off))
		}
	}
	return durable
}

// #27. A writer died with records it never synced. A store that takes over its
// segment now holds them in its own index, and acknowledges what its own index
// holds on the grounds that its own fsync covers it. So it has to fsync them.
func TestAPutAgainstAnAdoptedSegmentIsDurable(t *testing.T) {
	objs := testObjects(t, 3)
	s := openStore(t, killedWriter(t, objs))
	for i, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if got, want := durableMark(t, s), s.active.size; got < want {
			t.Fatalf("object %d: Put acknowledged, the sidecar knows %d durable of %d", i, got, want)
		}
	}
	// A retried ingest of the whole tail costs one fsync, not one per object.
	if n := s.fsyncs.Load(); n != 1 {
		t.Fatalf("%d fsyncs for puts of an adopted tail, want 1", n)
	}
	if s.active.size != durableMark(t, s) {
		t.Fatal("the tail grew: an object already there was written again")
	}
}

func TestAPutVerifiedAgainstAnAdoptedSegmentIsDurable(t *testing.T) {
	objs := testObjects(t, 3)
	s := openStore(t, killedWriter(t, objs))
	if err := s.PutVerified(objs[1].Key, objs[1].Data); err != nil {
		t.Fatal(err)
	}
	if got, want := durableMark(t, s), s.active.size; got < want {
		t.Fatalf("PutVerified acknowledged, the sidecar knows %d durable of %d", got, want)
	}
}

func TestAdoptionSyncsOnlyWhatIsUnsynced(t *testing.T) {
	objs := testObjects(t, 3)
	adopt := func(t *testing.T, dir string, opts ...Option) *Store {
		t.Helper()
		s := openStore(t, dir, opts...)
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		if err := s.ensureActiveLocked(); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("a tail its writer never synced", func(t *testing.T) {
		s := adopt(t, killedWriter(t, objs))
		if n := s.fsyncs.Load(); n != 1 {
			t.Errorf("%d fsyncs, want 1", n)
		}
		if got, want := durableMark(t, s), s.active.size; got != want {
			t.Errorf("the sidecar knows %d durable of %d", got, want)
		}
	})
	t.Run("no sidecar: nothing is known durable", func(t *testing.T) {
		dir := killedWriter(t, objs)
		sidecars, err := filepath.Glob(filepath.Join(dir, "*"+sidecarSuffix))
		if err != nil || len(sidecars) != 1 {
			t.Fatalf("sidecars: %v, %v", sidecars, err)
		}
		if err := os.Remove(sidecars[0]); err != nil {
			t.Fatal(err)
		}
		s := adopt(t, dir)
		if n := s.fsyncs.Load(); n != 1 {
			t.Errorf("%d fsyncs, want 1", n)
		}
		if got, want := durableMark(t, s), s.active.size; got != want {
			t.Errorf("the sidecar knows %d durable of %d", got, want)
		}
	})
	t.Run("a writer that closed cleanly: a process per command pays nothing", func(t *testing.T) {
		dir := t.TempDir()
		w, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		putAll(t, w, objs)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if s := adopt(t, dir); s.fsyncs.Load() != 0 {
			t.Errorf("%d fsyncs adopting a synced segment, want 0", s.fsyncs.Load())
		}
	})
	t.Run("an empty segment", func(t *testing.T) {
		dir := t.TempDir()
		w, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		w.appendMu.Lock()
		err = w.ensureActiveLocked()
		w.appendMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if s := adopt(t, dir); s.fsyncs.Load() != 0 {
			t.Errorf("%d fsyncs adopting an empty segment, want 0", s.fsyncs.Load())
		}
	})
	t.Run("a store that does not sync promises nothing", func(t *testing.T) {
		s := adopt(t, killedWriter(t, objs), WithSync(false))
		if n := s.fsyncs.Load(); n != 0 {
			t.Errorf("%d fsyncs, want 0", n)
		}
		if got := durableMark(t, s); got != 0 {
			t.Errorf("the sidecar claims %d durable, and nothing synced it", got)
		}
	})
}

// #27's other door. A writer killed during a seal leaves a whole footer over a body
// nobody has fsynced, still under its active name. A view maps such a file as the
// sealed segment it is about to be, and a sealed segment counted as durable.
func TestACrashedSealIsNotAcknowledgedUnsynced(t *testing.T) {
	objs := testObjects(t, 3)
	src := t.TempDir()
	w, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, o := range objs {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	copyDir := func() string {
		t.Helper()
		dst := t.TempDir()
		ents, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dst
	}
	o := objs[0]
	for name, write := range map[string]func(s *Store) error{
		"Put":         func(s *Store) error { return s.Put(o.Key, o.Data) },
		"PutVerified": func(s *Store) error { return s.PutVerified(o.Key, o.Data) },
		"WriteBatch": func(s *Store) error {
			return s.WriteBatch(func(y func(Object, error) bool) { y(o, nil) })
		},
		"WriteParallel": func(s *Store) error {
			_, err := s.WriteParallel(func(y func(Object, error) bool) { y(o, nil) }, WriteOpts{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := copyDir()
			s := openStore(t, dir) // syncs
			if err := write(s); err != nil {
				t.Fatal(err)
			}
			// Finishing the seal is what fsyncs that file, and renames it.
			if _, err := os.Stat(filepath.Join(dir, segmentName(1, activeSuffix))); err == nil && !ownCopy(s, o.Key) {
				t.Fatal("acknowledged, and the only copy is in a crashed seal nobody fsynced")
			}
			got, err := s.Get(o.Key)
			if err != nil || !bytes.Equal(got, o.Data) {
				t.Fatalf("Get = %d bytes, %v", len(got), err)
			}
		})
	}
	// What PutVerified finds it finishes, and relies on: no second copy.
	t.Run("PutVerified finishes the seal", func(t *testing.T) {
		dir := copyDir()
		s := openStore(t, dir)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, segmentName(1, sealedSuffix))); err != nil {
			t.Errorf("the seal is not finished: %v", err)
		}
		if ownCopy(s, o.Key) {
			t.Error("wrote a second copy of what it could finish and rely on")
		}
	})
	// A store that does not sync promises nothing, and need not write a second copy.
	t.Run("a store that does not sync", func(t *testing.T) {
		s := openStore(t, copyDir(), WithSync(false))
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if ownCopy(s, o.Key) {
			t.Error("wrote a second copy, with nothing to gain")
		}
	})
}

// A failed fsync at adoption: the store keeps nothing of the segment and is out of
// action, and the segment is free for the next one.
func TestAFailedAdoptionFsyncPoisonsTheStore(t *testing.T) {
	objs := testObjects(t, 2)
	dir := killedWriter(t, objs)
	s := openStore(t, dir)
	boom := errors.New("boom")
	s.beforeFsync = func(*os.File) error { return boom }
	if err := s.Put(objs[0].Key, objs[0].Data); !errors.Is(err, boom) {
		t.Fatalf("Put = %v, want the fsync's error", err)
	}
	if s.active != nil {
		t.Error("the store took the segment anyway")
	}
	// The store stays out of action for writes, as after any failed fsync: the
	// disk may have dropped what it failed to write, so a retry proves nothing.
	s.beforeFsync = nil
	if err := s.Put(objs[1].Key, objs[1].Data); !errors.Is(err, boom) {
		t.Errorf("Put after the failure = %v", err)
	}
	// And it let go of the segment's lock: the next store adopts it rather than make
	// one of its own.
	other := openStore(t, dir)
	if err := other.Put(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(other.active.path), segmentName(1, activeSuffix); got != want {
		t.Errorf("the next store wrote to %s, not %s: the failed one kept its lock", got, want)
	}
}

// A crashed seal somebody else has hold of is being finished by them, and may not be
// fsynced yet. PutVerified writes its own copy rather than rely on it.
func TestACrashedSealHeldBySomebodyIsNotReliedOn(t *testing.T) {
	objs := testObjects(t, 2)
	src := t.TempDir()
	w, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, o := range objs {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	path := filepath.Join(src, segmentName(1, activeSuffix))
	// w still holds the file's lock, as a writer in the middle of finishing would.
	s := openStore(t, src)
	if err := s.PutVerified(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if !ownCopy(s, objs[0].Key) {
		t.Fatal("acknowledged against a crashed seal somebody else holds")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("it finished a seal somebody else holds: %v", err)
	}
}

// Nor does a repair rewrite one: it is somebody else's to finish.
func TestARepairLeavesACrashedSealSomebodyHolds(t *testing.T) {
	objs := testObjects(t, 2)
	src := t.TempDir()
	w, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, o := range objs {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	// Its copy is damaged, which a repair would fix if it were this store's to rewrite.
	path := filepath.Join(src, segmentName(1, activeSuffix))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before[int64(len(magicHeader))+amberpack.RecHeaderSize] ^= 0x40
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}

	// A damaged copy of the same object in this store's own segment takes PutVerified
	// down its repair path, with the crashed seal in view.
	s := openStore(t, src)
	if err := s.Put(objs[0].Key, []byte("wrong")); err != nil {
		t.Fatal(err)
	}
	if !ownCopy(s, objs[0].Key) {
		t.Fatal("no copy of its own to repair")
	}
	// It answers for every copy, and cannot reach that one: saying nothing would be a lie.
	if err := s.PutVerified(objs[0].Key, objs[0].Data); err == nil || !strings.Contains(err.Error(), "another store holds") {
		t.Fatalf("PutVerified = %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("a repair rewrote a crashed seal somebody holds (%v)", err)
	}
	// Once its holder has let go, the same call finishes the seal and repairs it.
	w.Close()
	if err := s.PutVerified(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if !segExists(t, src, 1) {
		t.Fatal("nobody finished the seal")
	}
	if got, err := s.GetVerified(objs[0].Key); err != nil || !bytes.Equal(got, objs[0].Data) {
		t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
	}
}

// A good copy elsewhere must not hide it: lookups read the higher id first.
func TestPutVerifiedDoesNotAcknowledgeBesideABadHeldCrashedSeal(t *testing.T) {
	objs := testObjects(t, 2)
	dir := t.TempDir()
	sealedFile(t, dir, 1, objs[0])
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, o := range objs {
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.append(o.Key, rec, true); err != nil {
			t.Fatal(err)
		}
	}
	path, off := w.active.path, w.active.index[objs[0].Key].off
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	flipByte(t, path, off+amberpack.RecHeaderSize)
	s := openStore(t, dir)
	if err := s.PutVerified(objs[0].Key, objs[0].Data); err == nil {
		got, verr := s.GetVerified(objs[0].Key)
		t.Fatalf("PutVerified acknowledged, and GetVerified = %d bytes, %v", len(got), verr)
	}
}

// The exported form of the same check: "a copy somewhere else" must be one that can stand
// in for this one.
func TestHasOutsideDoesNotRelyOnACrashedSeal(t *testing.T) {
	objs := testObjects(t, 2)
	dir, _ := crashedSealIn(t, objs[:1])
	sealedFile(t, dir, 7, objs[0], objs[1])
	s := openStore(t, dir)
	if has, err := s.HasOutside(7, objs[0].Key); err != nil || has {
		t.Fatalf("HasOutside = %v, %v; the only copy outside segment 7 is in a crashed seal nobody has fsynced", has, err)
	}
}
