package gc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

func obj(t *testing.T, s string) fstree.Object {
	t.Helper()
	o, err := fstree.EncodeBlob([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func dirOf(t *testing.T, names map[string]key.Key) fstree.Object {
	t.Helper()
	var ents []fstree.Entry
	for _, n := range []string{"a", "c"} {
		k := names[n]
		ents = append(ents, fstree.Entry{Name: []byte(n), Mode: 0o100644, ContentKey: k[:]})
	}
	o, err := fstree.EncodeDirLeaf(ents)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func putAll(t *testing.T, s *packstore.Store, objs ...fstree.Object) {
	t.Helper()
	for _, o := range objs {
		if err := s.Put(o.Key, o.Bytes); err != nil {
			t.Fatal(err)
		}
	}
}

// fillPacks puts garbage until n packs are sealed.
func fillPacks(t *testing.T, s *packstore.Store, tag string, n int) {
	t.Helper()
	for i := 0; ; i++ {
		putAll(t, s, obj(t, fmt.Sprintf("%s-%0400d", tag, i)))
		if segs, _ := s.Segments(); len(segs) >= n {
			return
		}
	}
}

// hideChild flips the bit that makes a directory's entry link nothing: map key 5
// (ContentKey) becomes 13, which the decoder ignores. It still decodes.
func hideChild(t *testing.T, f []byte, child key.Key) {
	t.Helper()
	i := bytes.Index(f, append([]byte{0x05, 0x58, 0x20}, child[:]...))
	if i < 0 {
		t.Fatal("content key not found")
	}
	f[i] ^= 0x08
}

// unverifiedLive is a caller without MarkVerified: a walk that reads with Get.
func unverifiedLive(t *testing.T, s *packstore.Store, root key.Key) func(key.Key) bool {
	t.Helper()
	live := map[key.Key]bool{}
	for stack := []key.Key{root}; len(stack) > 0; {
		k := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if live[k] {
			continue
		}
		live[k] = true
		data, err := s.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		kids, err := fstree.ChildKeys(k, data)
		if err != nil {
			t.Fatal(err)
		}
		stack = append(stack, kids...)
	}
	return func(k key.Key) bool { return live[k] }
}

// Two bad records in one pack, as a damaged region of a disk leaves. If a bad Blob found
// first made a pass give up on the pack, the bad directory beside it would never be looked
// at, the pass would go on, and what the directory no longer names would be swept.
func TestABadBlobDoesNotHideABadDirectoryFromAnUnverifiedPass(t *testing.T) {
	old := runtime.GOMAXPROCS(1) // deterministic: the blob's turn comes first
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
	child := obj(t, "the child, in another pack")
	var file, root fstree.Object
	for i := 0; ; i++ { // the blob sorts before the directory in the index
		file = obj(t, fmt.Sprintf("a file in the first pack %d", i))
		root = dirOf(t, map[string]key.Key{"a": file.Key, "c": child.Key})
		if bytes.Compare(file.Key[:], root.Key[:]) < 0 {
			break
		}
	}
	ts := newTestStore(t, 2<<10)
	putAll(t, ts.objects, root, file)
	fillPacks(t, ts.objects, "one", 1)
	putAll(t, ts.objects, child)
	fillPacks(t, ts.objects, "two", 2)

	p := filepath.Join(ts.dir, "packstore", "0000000000000001.seg")
	f, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	hideChild(t, f, child.Key)
	f[bytes.Index(f, file.Bytes)] ^= 0xff
	if err := os.WriteFile(p, f, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ts.objects.Refresh(); err != nil {
		t.Fatal(err)
	}
	live := unverifiedLive(t, ts.objects, root.Key)
	stats, cerr := ts.objects.Compact(live, packstore.CompactOpts{})
	if !errors.Is(cerr, packstore.ErrCorrupt) {
		t.Errorf("Compact = %v, want it stopped", cerr)
	}
	if ok, err := ts.objects.Has(child.Key); err != nil || !ok {
		t.Fatalf("Compact = %v, victims %v; the child of a live directory is gone (has %v, %v)", cerr, stats.Victims, ok, err)
	}
}

// What the error says to do clears it, also for a copy in a segment nobody holds, which
// lookups read before the good sealed one.
func TestTheRepairTheErrorNamesClearsIt(t *testing.T) {
	ts := newTestStore(t, 2<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	pdir := filepath.Join(ts.dir, "packstore")
	blob := obj(t, "a file")
	root, err := fstree.EncodeFileNode([]key.Key{blob.Key})
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, ts.objects, blob, root)
	putTestRef(t, c, ts.refs, "main", root.Key)
	fillPacks(t, ts.objects, "g", 1)
	w, err := packstore.Open(pdir) // a duplicate in another process's idle segment, gone bad
	if err != nil {
		t.Fatal(err)
	}
	rec, err := amberpack.EncodeRecord(root.Key, root.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AppendRecord(root.Key, rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(pdir, "*.seg.active"))
	last := m[len(m)-1]
	f, err := os.ReadFile(last)
	if err != nil {
		t.Fatal(err)
	}
	f[8+amberpack.RecHeaderSize] ^= 0xff
	if err := os.WriteFile(last, f, 0o644); err != nil {
		t.Fatal(err)
	}
	backdatePacks(t, ts)
	if _, err := c.Run(context.Background(), 0); !errors.Is(err, packstore.ErrVerify) {
		t.Fatalf("Run = %v, want the mark to stop on the bad copy", err)
	}
	if err := ts.objects.PutVerified(root.Key, root.Bytes); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), 0); err != nil {
		t.Fatalf("Run after the repair: %v", err)
	}
	if got, err := ts.objects.GetVerified(root.Key); err != nil || !bytes.Equal(got, root.Bytes) {
		t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
	}
}

// A reference names a tree a repair next door has just made whole: a store that has been
// open all along must not refuse it for the old copy it still has mapped.
func TestAReferenceSeesARepairAnotherStoreMade(t *testing.T) {
	ts := newTestStore(t, 2<<10)
	blob := obj(t, "a file")
	root, err := fstree.EncodeFileNode([]key.Key{blob.Key})
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, ts.objects, blob, root)
	fillPacks(t, ts.objects, "g", 1)
	daemon := openTestStoreAt(t, ts.dir, 2<<10)
	c := daemon.openCollector(t, Options{Grace: time.Hour})
	end, err := daemon.objects.BeginWrite() // its first span, which looks; the next ones will not
	if err != nil {
		t.Fatal(err)
	}
	end()
	if ok, err := daemon.objects.Has(root.Key); err != nil || !ok {
		t.Fatalf("Has: %v, %v", ok, err)
	}
	p := filepath.Join(ts.dir, "packstore", "0000000000000001.seg")
	f, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(f, root.Bytes)
	if i < 0 {
		t.Fatal("root not in the pack")
	}
	w, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte{f[i] ^ 0xff}, int64(i)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := ts.objects.PutVerified(root.Key, root.Bytes); err != nil {
		t.Fatal(err)
	}
	_, abort, err := c.PrepareRef(root.Key)
	if err != nil {
		t.Fatalf("repaired and acknowledged, and PrepareRef = %v", err)
	}
	abort()
}
