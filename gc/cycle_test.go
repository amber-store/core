package gc

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestRunOverlapRefused(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
	if _, err := c.Run(context.Background(), 0); !errors.Is(err, ErrCycleRunning) {
		t.Fatalf("err = %v, want ErrCycleRunning", err)
	}
}
