// One cycle: snapshot the reference roots behind the write barrier; mark
// everything they reach into a bitmap over the packs' footer indexes,
// concurrently with ingests; sweep by rewriting every eligible pack whose
// dead ratio crosses the line (packstore.Compact) under the reference
// lock. Cycles never overlap. See specs/gc.qnt for the barrier protocol.

package gc

import (
	"context"
	"errors"
	"time"

	"github.com/amber-store/core/packstore"
)

// ErrCycleRunning reports an overlapping Run; cycles never overlap.
var ErrCycleRunning = errors.New("gc: a cycle is already running")

// CycleStats describes one cycle.
type CycleStats struct {
	Start         time.Time
	Duration      time.Duration
	MarkDuration  time.Duration // roots snapshot + mark walk
	SweepDuration time.Duration // Compact: select, copy, delete
	Threshold     float64
	Marked        int      // distinct live objects marked
	Scored        int      // sealed packs considered
	Reaped        []uint64 // victim ids, ascending
	CopiedRecords int
	CopiedBytes   int64
	FreedBytes    int64 // victim file bytes freed on disk, footers included
}

// Run marks from every reference root and sweeps the eligible packs above
// the line: garbage >= 0 forces that line, garbage < 0 uses policy — 0.5,
// or 0.1 under min-free pressure. Ingests and reads run through the mark
// (the write barrier keeps them); reference publication and the sweep
// stall each other on the reference lock.
func (c *Collector) Run(ctx context.Context, garbage float64) (CycleStats, error) {
	if !c.cycleMu.TryLock() {
		return CycleStats{}, ErrCycleRunning
	}
	defer c.cycleMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancelCycle = cancel
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.cancelCycle = nil
		c.mu.Unlock()
	}()
	stats, err := c.cycle(ctx, garbage)
	c.mu.Lock()
	c.last = &stats
	c.lastErr = err
	c.mu.Unlock()
	return stats, err
}

func (c *Collector) cycle(ctx context.Context, garbage float64) (stats CycleStats, err error) {
	stats.Start = time.Now()
	defer func() { stats.Duration = time.Since(stats.Start) }()

	threshold := garbage
	if threshold < 0 {
		threshold = c.opts.Garbage
		if freeBelow(c.dir, c.opts.MinFree) {
			threshold = minFreeGarbage
		}
	}
	stats.Threshold = threshold

	// Snapshot: barrier on, then the roots, under the reference lock — a
	// PUT in flight commits or aborts before the snapshot; every later
	// PUT greys its walked closure, every later ingest its written keys.
	c.refLock.Lock()
	c.objects.BeginBarrier()
	roots, err := c.roots()
	c.refLock.Unlock()
	if err != nil {
		c.objects.AbortBarrier()
		return stats, err
	}

	live, err := c.markLive(ctx, roots)
	c.mu.Lock()
	midMark := c.midMark
	c.mu.Unlock()
	if midMark != nil {
		midMark()
	}
	if err != nil {
		c.objects.AbortBarrier()
		return stats, err
	}
	stats.Marked = live.Marked()
	stats.MarkDuration = time.Since(stats.Start)

	// Sweep, excluding reference publication. Compact consumes the grey
	// set, seals the active segment, rewrites the victims and deletes
	// them once the copies are durable.
	c.refLock.Lock()
	defer c.refLock.Unlock()
	if err := ctx.Err(); err != nil {
		c.objects.AbortBarrier()
		return stats, err
	}
	opts := packstore.CompactOpts{
		MinDeadRatio: threshold,
		Horizon:      time.Now().Add(-c.opts.Grace),
	}
	if c.opts.Rate > 0 {
		opts.Pace = newThrottle(c.opts.Rate).pace
	}
	sweepStart := time.Now()
	cs, err := c.objects.Compact(live.Contains, opts)
	stats.SweepDuration = time.Since(sweepStart)
	stats.Scored = cs.SegmentsScanned
	stats.Reaped = cs.Victims
	stats.CopiedRecords = cs.RecordsCopied
	stats.CopiedBytes = int64(cs.BytesCopied)
	stats.FreedBytes = int64(cs.BytesFreed)
	return stats, err
}
