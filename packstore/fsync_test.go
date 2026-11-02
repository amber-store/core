package packstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// sealedFile writes a sealed segment of raw records and returns where each
// record's payload starts, to damage it by.
func sealedFile(t *testing.T, dir string, id uint64, objs ...Object) map[key.Key]int64 {
	t.Helper()
	body := headerAt(versionBase)
	payload := map[key.Key]int64{}
	var entries []indexEntry
	for _, o := range objs {
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, indexEntry{k: o.Key, off: uint64(len(body)), slen: uint32(len(o.Data))})
		payload[o.Key] = int64(len(body)) + amberpack.RecHeaderSize
		body = append(body, rec...)
	}
	footer, err := buildFooter(int64(len(body)), entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, segmentName(id, sealedSuffix)), append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}
	return payload
}

func segExists(t *testing.T, dir string, id uint64) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, segmentName(id, sealedSuffix)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// crashedSealIn leaves a store whose writer died between a seal's footer and its rename,
// and lets go of the file. It returns the segment's path, which is still its active name.
func crashedSealIn(t *testing.T, objs []Object) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	path = w.active.path
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	w.Close()
	return dir, path
}

// A failed fsync may have dropped what it failed to write, and a retry reports success,
// so nothing may be acknowledged after one: finishing a seal is no exception.
func TestAFailedFsyncFinishingASealPoisonsTheStore(t *testing.T) {
	objs := testObjects(t, 2)
	dir, path := crashedSealIn(t, objs)
	s := openStore(t, dir)
	boom := errors.New("boom")
	s.beforeFsync = func(f *os.File) error {
		if f.Name() == path {
			return boom
		}
		return nil
	}
	if err := s.PutVerified(objs[0].Key, objs[0].Data); !errors.Is(err, boom) {
		t.Fatalf("PutVerified = %v, want the fsync's error", err)
	}
	s.beforeFsync = nil
	if err := s.PutVerified(objs[0].Key, objs[0].Data); !errors.Is(err, boom) {
		t.Errorf("PutVerified after the failure = %v: acknowledged on a segment whose fsync failed", err)
	}
}

// The same for the directory: without its fsync the rename may not last.
func TestAFailedDirectoryFsyncFinishingASealPoisonsTheStore(t *testing.T) {
	objs := testObjects(t, 2)
	dir, path := crashedSealIn(t, objs)
	s := openStore(t, dir)
	boom := errors.New("boom")
	s.beforeFsync = func(f *os.File) error {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return boom // the rename happened: this is the directory's
		}
		return nil
	}
	if err := s.PutVerified(objs[0].Key, objs[0].Data); !errors.Is(err, boom) {
		t.Fatalf("PutVerified = %v, want the fsync's error", err)
	}
	s.beforeFsync = nil
	if err := s.PutVerified(objs[0].Key, objs[0].Data); !errors.Is(err, boom) {
		t.Errorf("PutVerified after the failure = %v", err)
	}
}

// A seal is trusted from its rename on, so the body is synced before it, and the rename
// after it.
func TestFinishingASealSyncsTheBodyThenTheRename(t *testing.T) {
	objs := testObjects(t, 1)
	dir, path := crashedSealIn(t, objs)
	s := openStore(t, dir)
	var seen []string
	s.beforeFsync = func(f *os.File) error {
		_, err := os.Stat(path)
		seen = append(seen, map[bool]string{true: "before rename", false: "after rename"}[err == nil]+": "+filepath.Base(f.Name()))
		return nil
	}
	s.appendMu.Lock()
	err := s.finishCrashedSeal(s.sealed[0])
	s.appendMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"before rename: " + filepath.Base(path), "after rename: " + filepath.Base(dir)}
	if !slices.Equal(seen, want) {
		t.Errorf("fsyncs %q, want %q", seen, want)
	}
	if !segExists(t, dir, 1) {
		t.Error("the seal was not finished")
	}
}

// Adoption's fsync is what lets a store acknowledge a tail it did not write.
func TestAdoptingAnUnsyncedTailFsyncsTheSegment(t *testing.T) {
	objs := testObjects(t, 2)
	dir := killedWriter(t, objs)
	s := openStore(t, dir)
	var seen []string
	s.beforeFsync = func(f *os.File) error {
		seen = append(seen, filepath.Base(f.Name()))
		return nil
	}
	if err := s.Put(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(seen, segmentName(1, activeSuffix)) {
		t.Errorf("fsyncs %q: a Put was acknowledged on an unsynced record", seen)
	}
}

// Finishing a seal looks first: the file may be gone, or no seal after all.
func TestFinishCrashedSealTakesNothingOnTrust(t *testing.T) {
	objs := testObjects(t, 1)
	t.Run("gone", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		g := &sealedSegment{id: 7, path: filepath.Join(dir, segmentName(7, activeSuffix))}
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		if err := s.finishCrashedSeal(g); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("not a seal", func(t *testing.T) {
		dir := t.TempDir()
		w, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Put(objs[0].Key, objs[0].Data); err != nil {
			t.Fatal(err)
		}
		path := w.active.path
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		s := openStore(t, dir)
		g := &sealedSegment{id: 1, path: path}
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		if err := s.finishCrashedSeal(g); err != nil {
			t.Fatal(err)
		}
		if segExists(t, dir, 1) {
			t.Error("a segment with no footer was renamed to a sealed name")
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the active segment: %v", err)
		}
	})
	t.Run("held", func(t *testing.T) {
		dir, path := crashedSealIn(t, objs)
		s := openStore(t, dir)
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if ok, err := tryLock(f); err != nil || !ok {
			t.Fatalf("tryLock: %v, %v", ok, err)
		}
		s.appendMu.Lock()
		defer s.appendMu.Unlock()
		if err := s.finishCrashedSeal(s.sealed[0]); err != nil {
			t.Fatal(err)
		}
		if segExists(t, dir, 1) {
			t.Error("a seal somebody holds was finished under them")
		}
	})
}

// A closing store fsyncs its segment again. After a failed fsync that one reports success
// without having written anything, and a synced mark would then promise what is lost.
func TestAPoisonedStoreLeavesNoSyncedMarkBehind(t *testing.T) {
	objs := testObjects(t, 1)
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutVerifiedDeferred(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	before := durableMark(t, s)
	path := s.active.path
	boom := errors.New("boom")
	s.beforeFsync = func(*os.File) error { return boom }
	if err := s.Sync(); !errors.Is(err, boom) {
		t.Fatalf("Sync = %v", err)
	}
	s.beforeFsync = nil
	s.Close()
	side, err := os.ReadFile(path + sidecarSuffix)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := readSidecar(side, true)
	for _, r := range recs {
		if r.kind == sidecarSynced && int64(r.off) > before {
			t.Fatalf("a synced mark at %d after a failed fsync, was %d", r.off, before)
		}
	}
}

// A view can hold a crashed seal that another store has since finished.
func TestSegmentsFollowsASealAnotherStoreFinished(t *testing.T) {
	objs := testObjects(t, 2)
	dir, _ := crashedSealIn(t, objs)
	s := openStore(t, dir) // maps it under its active name
	other := openStore(t, dir)
	if err := other.PutVerified(objs[0].Key, objs[0].Data); err != nil { // finishes the seal
		t.Fatal(err)
	}
	if !segExists(t, dir, 1) {
		t.Fatal("the seal was not finished")
	}
	segs, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if len(segs) != 1 || segs[0].ID != 1 {
		t.Errorf("Segments = %+v", segs)
	}
}

// Once the view knows the seal is finished, it is a sealed segment like any other: a
// write relies on it rather than make another copy.
func TestAFinishedSealCountsOnceTheViewKnows(t *testing.T) {
	objs := testObjects(t, 2)
	dir, _ := crashedSealIn(t, objs)
	s := openStore(t, dir)
	other := openStore(t, dir)
	if err := other.PutVerified(objs[0].Key, objs[0].Data); err != nil { // finishes the seal
		t.Fatal(err)
	}
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(objs[1].Key, objs[1].Data); err != nil {
		t.Fatal(err)
	}
	if ownCopy(s, objs[1].Key) {
		t.Error("a second copy, beside a sealed segment that holds it durably")
	}
}

// A crashed seal's name moves under a view. Run with -race: a refresh learns of it while
// repairs and duplicate checks ask.
func TestACrashedSealsNameIsReadSafelyDuringARefresh(t *testing.T) {
	objs := testObjects(t, 4)
	dir := t.TempDir()
	sealedFile(t, dir, 1, objs...)
	s := openStore(t, dir)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				if err := s.Refresh(); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	for range 200 {
		if err := s.PutVerified(objs[0].Key, objs[0].Data); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	<-done
}

// The ordinary seal: its footer's fsync is what every record in the segment now rests on.
func TestAFailedFsyncSealingPoisonsTheStore(t *testing.T) {
	objs := testObjects(t, 2)
	s := openStore(t, t.TempDir())
	if err := s.Put(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	s.beforeFsync = func(*os.File) error { return errors.New("EIO") }
	s.appendMu.Lock()
	err := s.sealActiveLocked()
	s.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	s.beforeFsync = nil
	if err := s.Put(objs[1].Key, objs[1].Data); err == nil || !strings.Contains(err.Error(), "EIO") {
		t.Fatalf("Put after a failed seal = %v", err)
	}
}
