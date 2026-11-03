package packstore

import (
	"context"
	"os"
	"testing"

	"github.com/amber-store/core/amberpack"
)

// The payload of a record is whatever somebody stored. These bytes end the segment, and
// read as a whole footer that fits exactly where they lie.
func objectEndingInAFooter(t *testing.T, s *Store) Object {
	t.Helper()
	s.appendMu.Lock()
	err := s.ensureActiveLocked()
	start := s.active.size + amberpack.RecHeaderSize
	s.appendMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	filler := []byte("x")
	decoy := blobObj(t, []byte("decoy"))
	footer, err := buildFooter(start+int64(len(filler)), []indexEntry{{k: decoy.Key, off: uint64(len(magicHeader))}})
	if err != nil {
		t.Fatal(err)
	}
	return blobObj(t, append(filler, footer...))
}

func TestARecordThatEndsInAFooterIsNoSeal(t *testing.T) {
	setup := func(t *testing.T) (dir string, w *Store, all []Object) {
		dir = t.TempDir()
		w = openStore(t, dir, WithCompression(amberpack.Compression{})) // a compressed payload would not end in these bytes
		all = distinct(t, 3)
		for _, o := range all {
			if err := w.Put(o.Key, o.Data); err != nil {
				t.Fatal(err)
			}
		}
		evil := objectEndingInAFooter(t, w)
		if err := w.Put(evil.Key, evil.Data); err != nil {
			t.Fatal(err)
		}
		return dir, w, append(all, evil)
	}
	// Every object reads, and the next writer appends behind them instead of finishing a
	// seal: renaming the segment would make the decoy index the only one it has.
	check := func(t *testing.T, dir string, all []Object) {
		t.Helper()
		r := openStore(t, dir)
		wantObjects(t, r, all)
		n := openStore(t, dir)
		next := blobObj(t, []byte("the next writer"))
		if err := n.Put(next.Key, next.Data); err != nil {
			t.Fatal(err)
		}
		if segExists(t, dir, 1) {
			t.Fatal("the next writer finished a seal nobody started")
		}
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		a := openStore(t, dir)
		wantObjects(t, a, append(all, next))
		if err := a.Verify(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("after a clean close", func(t *testing.T) {
		dir, w, all := setup(t)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		check(t, dir, all)
	})
	t.Run("without a sidecar", func(t *testing.T) {
		dir, w, all := setup(t)
		p := w.active.path
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p + sidecarSuffix); err != nil {
			t.Fatal(err)
		}
		check(t, dir, all)
	})
	t.Run("seen by another store while its writer is alive", func(t *testing.T) {
		dir, _, all := setup(t)
		wantObjects(t, openStore(t, dir), all)
	})
	t.Run("a seal behind it is still a seal", func(t *testing.T) {
		dir, w, all := setup(t)
		w.afterFooter = func() error { return os.ErrInvalid } // killed between the footer and the rename
		w.appendMu.Lock()
		err := w.sealActiveLocked()
		w.appendMu.Unlock()
		if err == nil {
			t.Fatal("the seal went through")
		}
		w.Close()
		n := openStore(t, dir)
		next := blobObj(t, []byte("the next writer"))
		if err := n.Put(next.Key, next.Data); err != nil {
			t.Fatal(err)
		}
		if !segExists(t, dir, 1) {
			t.Fatal("nobody finished the seal")
		}
		wantObjects(t, n, append(all, next))
	})
}
