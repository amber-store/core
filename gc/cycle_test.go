package gc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

func TestRunReapsDeadKeepsLive(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	rootKeep, keysKeep := storeTree(t, ts.objects, "keep", 40)
	rootDead, keysDead := storeTree(t, ts.objects, "dead", 40)
	putTestRef(t, c, ts.refs, "keep", rootKeep)
	putTestRef(t, c, ts.refs, "dead", rootDead)
	rmTestRef(t, c, ts.refs, "dead", rootDead)

	backdatePacks(t, ts)
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) == 0 {
		t.Fatalf("nothing reaped: %+v", stats)
	}
	if stats.Marked == 0 {
		t.Error("Marked = 0")
	}
	for _, k := range keysKeep {
		if _, err := ts.objects.Get(k); err != nil {
			t.Fatalf("live key %s: %v", k, err)
		}
	}
	gone := 0
	for _, k := range keysDead {
		if _, err := ts.objects.Get(k); errors.Is(err, packstore.ErrNotFound) {
			gone++
		}
	}
	if gone == 0 {
		t.Error("no dead key was collected")
	}
}

func TestRunRespectsGrace(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	root, keys := storeTree(t, ts.objects, "young", 40)
	putTestRef(t, c, ts.refs, "v", root)
	rmTestRef(t, c, ts.refs, "v", root)

	// No backdate: every pack is younger than the grace period.
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) != 0 {
		t.Fatalf("reaped young packs: %+v", stats)
	}
	for _, k := range keys {
		if _, err := ts.objects.Get(k); err != nil {
			t.Fatalf("young unreferenced key %s collected: %v", k, err)
		}
	}
}

// TestRefPutDuringMark: a reference committed between the mark and the
// sweep names a pre-existing, unmarked tree; its walked closure joins the
// grey set, so the sweep keeps it while still collecting other garbage of
// the same vintage.
func TestRefPutDuringMark(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	rootLate, keysLate := storeTree(t, ts.objects, "late", 40)
	_, keysDead := storeTree(t, ts.objects, "dead", 40)
	rootKeep, _ := storeTree(t, ts.objects, "keep", 4)
	putTestRef(t, c, ts.refs, "keep", rootKeep)
	backdatePacks(t, ts)

	c.mu.Lock()
	c.midMark = func() {
		putTestRef(t, c, ts.refs, "late", rootLate)
	}
	c.mu.Unlock()
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) == 0 {
		t.Fatalf("nothing reaped: %+v", stats)
	}
	for _, k := range keysLate {
		if _, err := ts.objects.Get(k); err != nil {
			t.Fatalf("late-referenced key %s lost to the sweep: %v", k, err)
		}
	}
	gone := 0
	for _, k := range keysDead {
		if _, err := ts.objects.Get(k); errors.Is(err, packstore.ErrNotFound) {
			gone++
		}
	}
	if gone == 0 {
		t.Error("the sweep collected nothing else, so the test proves nothing")
	}
}

// TestRunWithCopyBudgetCapsTheSweep: the budget reaches the sweep. The few
// live objects share their pack with dead ones, and the packs after it hold
// nothing live. A cycle with no room to copy reaps those and leaves the
// shared pack to the next cycle, which has room.
func TestRunWithCopyBudgetCapsTheSweep(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	rootKeep, keysKeep := storeTree(t, ts.objects, "keep", 4)
	rootDead, _ := storeTree(t, ts.objects, "dead", 80)
	putTestRef(t, c, ts.refs, "keep", rootKeep)
	putTestRef(t, c, ts.refs, "dead", rootDead)
	rmTestRef(t, c, ts.refs, "dead", rootDead)
	backdatePacks(t, ts)

	keepIsLive := func(when string) {
		t.Helper()
		for _, k := range keysKeep {
			if _, err := ts.objects.Get(k); err != nil {
				t.Fatalf("live key %s after %s: %v", k, when, err)
			}
		}
	}

	capped, err := c.RunWithCopyBudget(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Reaped) == 0 {
		t.Fatalf("a zero budget reaped no fully dead pack: %+v", capped)
	}
	if capped.CopiedRecords != 0 || capped.CopiedBytes != 0 {
		t.Fatalf("a zero budget copied %d records, %d bytes: %+v", capped.CopiedRecords, capped.CopiedBytes, capped)
	}
	keepIsLive("the capped cycle")

	rest, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if rest.CopiedRecords == 0 {
		t.Fatalf("the capped cycle left nothing that needed a copy, so the test proves nothing: %+v", rest)
	}
	keepIsLive("the uncapped cycle")
}

func TestRunOverlapRefused(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	if _, err := c.Run(context.Background(), 0); !errors.Is(err, ErrCycleRunning) {
		t.Fatalf("err = %v, want ErrCycleRunning", err)
	}
}

// #29. One live record that fails verification costs its own pack and nothing else:
// the cycle frees what it can, says what it left, and a repair clears it.
func TestOneBadLiveRecordDoesNotStopCollection(t *testing.T) {
	for _, how := range []string{"plain Put of the wrong bytes", "bit rot in a sealed pack"} {
		t.Run(how, func(t *testing.T) {
			ts := newTestStore(t, 2<<10)
			c := ts.openCollector(t, Options{Grace: time.Hour})
			good, err := fstree.EncodeBlob([]byte("what the key says"))
			if err != nil {
				t.Fatal(err)
			}
			data := good.Bytes
			if how == "plain Put of the wrong bytes" {
				data = []byte("something else")
			}
			if err := ts.objects.Put(good.Key, data); err != nil {
				t.Fatal(err)
			}
			root, err := fstree.EncodeFileNode([]key.Key{good.Key})
			if err != nil {
				t.Fatal(err)
			}
			if err := ts.objects.Put(root.Key, root.Bytes); err != nil {
				t.Fatal(err)
			}
			var dead []key.Key
			for i := range 40 {
				o, err := fstree.EncodeBlob([]byte(fmt.Sprintf("%0400d", i)))
				if err != nil {
					t.Fatal(err)
				}
				if err := ts.objects.Put(o.Key, o.Bytes); err != nil {
					t.Fatal(err)
				}
				dead = append(dead, o.Key)
			}
			putTestRef(t, c, ts.refs, "main", root.Key)
			if how == "bit rot in a sealed pack" {
				// The blob is the first record of the first pack.
				p := filepath.Join(ts.dir, "packstore", "0000000000000001.seg")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b[8+amberpack.RecHeaderSize] ^= 0x40
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			backdatePacks(t, ts)
			stats, err := c.Run(context.Background(), 0)
			if !errors.Is(err, packstore.ErrCorrupt) {
				t.Fatalf("Run = %v, want ErrCorrupt: a bad record must stay loud", err)
			}
			if len(stats.Skipped) != 1 || stats.Skipped[0].Key != good.Key || stats.Skipped[0].ID != 1 {
				t.Fatalf("Skipped = %+v", stats.Skipped)
			}
			if slices.Contains(stats.Reaped, 1) {
				t.Errorf("Reaped = %v lists the pack it kept", stats.Reaped)
			}
			if len(stats.Reaped) == 0 || stats.FreedBytes == 0 {
				t.Errorf("nothing freed: %+v", stats)
			}
			gone := countGone(t, ts.objects, dead)
			if gone == 0 {
				t.Fatal("collection stopped")
			}
			st, err := c.Status(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(st.LastError, good.Key.String()) {
				t.Errorf("Status.LastError = %q, want it to name the object", st.LastError)
			}

			// The repair clears it: what the kept pack held of the garbage goes too.
			if err := ts.objects.PutVerified(good.Key, good.Bytes); err != nil {
				t.Fatal(err)
			}
			backdatePacks(t, ts)
			stats, err = c.Run(context.Background(), 0)
			if err != nil || len(stats.Skipped) != 0 {
				t.Fatalf("after the repair: %v, skipped %+v", err, stats.Skipped)
			}
			if !slices.Contains(stats.Reaped, 1) {
				t.Errorf("after the repair: Reaped = %v", stats.Reaped)
			}
			if got := countGone(t, ts.objects, dead); got != len(dead) {
				t.Errorf("after the repair: %d of %d dead objects collected", got, len(dead))
			}
			if got, err := ts.objects.Get(good.Key); err != nil || !bytes.Equal(got, good.Bytes) {
				t.Errorf("the object a reference keeps: %q, %v", got, err)
			}
		})
	}
}

// An interior object has to be read to know what it reaches. A bad one stops the mark
// even if it still decodes: it walks as a smaller directory, and whatever it no longer
// names would be swept. Nothing can be freed without knowing what is live.
func TestAMarkVerifiesWhatItReads(t *testing.T) {
	ent := func(name string, k key.Key) fstree.Entry {
		return fstree.Entry{Name: []byte(name), Mode: 0o100644, ContentKey: k[:]}
	}
	blob := func(s string) fstree.Object {
		o, err := fstree.EncodeBlob([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	for name, deadInFirstPack := range map[string]bool{
		"its pack would be swept":      true,
		"its pack is below every line": false,
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestStore(t, 2<<10)
			c := ts.openCollector(t, Options{Grace: time.Hour})
			put := func(k key.Key, d []byte) {
				t.Helper()
				if err := ts.objects.Put(k, d); err != nil {
					t.Fatal(err)
				}
			}
			fill := func(tag string, packs int) { // garbage until that many packs are sealed
				t.Helper()
				for i := 0; ; i++ {
					o := blob(fmt.Sprintf("%s-%0400d", tag, i))
					put(o.Key, o.Bytes)
					if segs, _ := ts.objects.Segments(); len(segs) >= packs {
						return
					}
				}
			}
			a, b := blob("file a"), blob("file b")
			entries := []fstree.Entry{ent("a", a.Key), ent("b", b.Key)}
			var filler []fstree.Object
			if !deadInFirstPack {
				for i := range 8 { // live, to fill the pack with nothing dead
					o := blob(fmt.Sprintf("live-%0400d", i))
					filler = append(filler, o)
					entries = append(entries, ent(fmt.Sprintf("z%d", i), o.Key))
				}
			}
			root, err := fstree.EncodeDirLeaf(entries)
			if err != nil {
				t.Fatal(err)
			}
			put(root.Key, root.Bytes)
			put(a.Key, a.Bytes)
			for _, o := range filler {
				put(o.Key, o.Bytes)
			}
			if deadInFirstPack {
				fill("one", 1)
			}
			put(b.Key, b.Bytes)
			fill("two", 2)
			putTestRef(t, c, ts.refs, "main", root.Key)

			// One bit: map key 5 (ContentKey) becomes 13, which the decoder ignores, and
			// entry "b" links nothing.
			p := filepath.Join(ts.dir, "packstore", "0000000000000001.seg")
			f, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			at := 8 + amberpack.RecHeaderSize
			i := bytes.Index(f[at:], append([]byte{0x05, 0x58, 0x20}, b.Key[:]...))
			if i < 0 {
				t.Fatal("content key not found")
			}
			f[at+i] ^= 0x08
			if err := os.WriteFile(p, f, 0o644); err != nil {
				t.Fatal(err)
			}

			backdatePacks(t, ts)
			stats, err := c.Run(context.Background(), 0)
			if !errors.Is(err, packstore.ErrVerify) {
				t.Fatalf("Run = %v, want ErrVerify", err)
			}
			if len(stats.Reaped) != 0 {
				t.Errorf("Reaped = %v: something was freed without knowing what is live", stats.Reaped)
			}
			// What the error says to do: the reference is whole.
			if err := ts.objects.PutVerified(root.Key, root.Bytes); err != nil {
				t.Fatal(err)
			}
			if _, err := fstree.CheckComplete(root.Key, ts.objects.GetVerified, ts.objects.Has, 1); err != nil {
				t.Fatalf("the accepted reference dangles: %v", err)
			}
			backdatePacks(t, ts)
			if _, err := c.Run(context.Background(), 0); err != nil {
				t.Fatalf("after the repair: %v", err)
			}
			if _, err := fstree.CheckComplete(root.Key, ts.objects.GetVerified, ts.objects.Has, 1); err != nil {
				t.Fatalf("after a cycle, the accepted reference dangles: %v", err)
			}
		})
	}
}

// One that does not decode at all stops it the same way.
func TestABadInteriorObjectStopsTheMark(t *testing.T) {
	ts := newTestStore(t, 2<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	blob, err := fstree.EncodeBlob([]byte("a file"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.objects.Put(blob.Key, blob.Bytes); err != nil {
		t.Fatal(err)
	}
	root, err := fstree.EncodeFileNode([]key.Key{blob.Key})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.objects.Put(root.Key, root.Bytes); err != nil {
		t.Fatal(err)
	}
	putTestRef(t, c, ts.refs, "main", root.Key)
	var dead []key.Key
	for i := range 40 {
		o, err := fstree.EncodeBlob([]byte(fmt.Sprintf("%0400d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := ts.objects.Put(o.Key, o.Bytes); err != nil {
			t.Fatal(err)
		}
		dead = append(dead, o.Key)
	}
	// The root's record, the second of the first pack: its payload no longer decodes.
	p := filepath.Join(ts.dir, "packstore", "0000000000000001.seg")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	off := 8 + amberpack.RecHeaderSize + len(blob.Bytes) + amberpack.RecHeaderSize
	b[off] = 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	backdatePacks(t, ts)
	stats, err := c.Run(context.Background(), 0)
	if err == nil {
		t.Fatal("Run passed a tree it could not read")
	}
	if len(stats.Reaped) != 0 || countGone(t, ts.objects, dead) != 0 {
		t.Errorf("something was freed without knowing what is live: %+v", stats)
	}
}

// A reference is accepted on what verified: a directory whose bytes are not what its
// key says may name less than it should, and the reference would dangle from the start.
func TestPrepareRefRefusesABadInteriorObject(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	a, err := fstree.EncodeBlob([]byte("file a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fstree.EncodeBlob([]byte("file b, never stored"))
	if err != nil {
		t.Fatal(err)
	}
	ent := func(name string, k key.Key) fstree.Entry {
		return fstree.Entry{Name: []byte(name), Mode: 0o100644, ContentKey: k[:]}
	}
	root, err := fstree.EncodeDirLeaf([]fstree.Entry{ent("a", a.Key), ent("b", b.Key)})
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := fstree.EncodeDirLeaf([]fstree.Entry{ent("a", a.Key)})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.objects.Put(a.Key, a.Bytes); err != nil {
		t.Fatal(err)
	}
	if err := ts.objects.Put(root.Key, wrong.Bytes); err != nil { // plain Put verifies nothing
		t.Fatal(err)
	}
	if _, _, err := c.PrepareRef(root.Key); !errors.Is(err, packstore.ErrVerify) {
		t.Fatalf("PrepareRef = %v, want ErrVerify", err)
	}
}
