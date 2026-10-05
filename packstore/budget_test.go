package packstore

import (
	"errors"
	"math"
	"testing"
)

// recordPayload is the payload of one of compactStore's records. With its
// header a record's live bytes come to a little more, which the budgets
// below allow 512 bytes for.
const recordPayload uint64 = 4 << 10

// compactWith runs one pass over a compactStore with exactly the objects at
// live kept, under a copy budget, nil for none.
//
// compactStore puts five 4 KiB objects into a store that rotates at 8 KiB, so
// a compaction pass sees three sealed segments: {0,1}, {2,3}, and {4}, the
// last sealed by the pass itself.
func compactWith(t *testing.T, budget *uint64, live ...int) (CompactStats, *Store, []Object) {
	t.Helper()
	s, objs := compactStore(t)
	stats, err := s.Compact(liveSet(objs, live...), CompactOpts{MinDeadRatio: 0.4, MaxCopyBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	return stats, s, objs
}

// A store too full to copy must still free space. {2,3} is fully dead, so it
// needs no copy. {4} survives because a zero budget skips the seal.
func TestZeroCopyBudgetStillReclaimsAFullyDeadSegment(t *testing.T) {
	stats, s, objs := compactWith(t, new(uint64(0)), 0, 1, 4)
	if stats.RecordsCopied != 0 {
		t.Errorf("RecordsCopied = %d, want 0 (stats: %+v)", stats.RecordsCopied, stats)
	}
	if stats.SegmentsCompacted != 1 {
		t.Errorf("SegmentsCompacted = %d, want 1 (stats: %+v)", stats.SegmentsCompacted, stats)
	}
	for _, i := range []int{0, 1, 4} {
		if _, err := s.Get(objs[i].Key); err != nil {
			t.Errorf("live object %d lost: %v", i, err)
		}
	}
	for _, i := range []int{2, 3} {
		if _, err := s.Get(objs[i].Key); !errors.Is(err, ErrNotFound) {
			t.Errorf("dead object %d still readable: err = %v, want ErrNotFound", i, err)
		}
	}
	// {0,1} is the one sealed segment left: {4} was not sealed by the pass.
	segs, err := s.Segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Errorf("%d sealed segments left, want 1: the active segment was sealed", len(segs))
	}
}

// Both half-live segments need a 4 KiB copy each. One byte buys neither, so
// only the fully dead {4} goes.
func TestCopyBudgetUnderTheSmallestVictimCopiesNothing(t *testing.T) {
	stats, s, objs := compactWith(t, new(uint64(1)), 0, 2)
	if stats.RecordsCopied != 0 {
		t.Errorf("RecordsCopied = %d, want 0 (stats: %+v)", stats.RecordsCopied, stats)
	}
	if stats.SegmentsCompacted != 1 {
		t.Errorf("SegmentsCompacted = %d, want 1 (stats: %+v)", stats.SegmentsCompacted, stats)
	}
	for _, i := range []int{0, 1, 2, 3} {
		if _, err := s.Get(objs[i].Key); err != nil {
			t.Errorf("object %d lost to a skipped pass: %v", i, err)
		}
	}
}

// One record's worth buys one half-live segment, plus the free dead one.
func TestCopyBudgetForOneVictimTakesOne(t *testing.T) {
	stats, _, _ := compactWith(t, new(recordPayload+512), 0, 2)
	if stats.RecordsCopied != 1 {
		t.Errorf("RecordsCopied = %d, want 1 (stats: %+v)", stats.RecordsCopied, stats)
	}
	if stats.SegmentsCompacted != 2 {
		t.Errorf("SegmentsCompacted = %d, want 2 (stats: %+v)", stats.SegmentsCompacted, stats)
	}
}

func TestCopyBudgetForBothVictimsTakesBoth(t *testing.T) {
	bounded, _, _ := compactWith(t, new(2*(recordPayload+512)), 0, 2)
	unbounded, _, _ := compactWith(t, nil, 0, 2)
	if bounded.RecordsCopied != 2 {
		t.Errorf("RecordsCopied = %d, want 2 (stats: %+v)", bounded.RecordsCopied, bounded)
	}
	if bounded.SegmentsCompacted != 3 {
		t.Errorf("SegmentsCompacted = %d, want 3 (stats: %+v)", bounded.SegmentsCompacted, bounded)
	}
	if bounded.RecordsCopied != unbounded.RecordsCopied {
		t.Errorf("RecordsCopied = %d, unbounded %d", bounded.RecordsCopied, unbounded.RecordsCopied)
	}
	if bounded.SegmentsCompacted != unbounded.SegmentsCompacted {
		t.Errorf("SegmentsCompacted = %d, unbounded %d", bounded.SegmentsCompacted, unbounded.SegmentsCompacted)
	}
}

// The zero CompactOpts is what every caller from before the budget passes.
func TestDefaultCopyBudgetIsUnbounded(t *testing.T) {
	if got := (CompactOpts{}).copyBudget(); got != math.MaxUint64 {
		t.Errorf("copy budget of the zero CompactOpts = %d, want %d", got, uint64(math.MaxUint64))
	}
}
