// The mark-and-sweep GC surface: Liveness (the dry run) and Compact (the
// sweep), driven by the collector in package gc against a MarkSet built
// from the references' closures. Ported from Mic92's bitmap GC
// (draganm/amber-store#9); see architecture/mark-sweep-gc.md and
// specs/gc.qnt.

package packstore

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"iter"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

type copyRec struct {
	g   *sealedSegment
	e   indexEntry
	rec []byte
}

// SegmentLiveness is one segment's liveness breakdown. Byte counts cover
// record bytes only, not footer overhead.
type SegmentLiveness struct {
	ID        uint64
	Sealed    bool
	LiveKeys  int
	DeadKeys  int
	LiveBytes uint64
	DeadBytes uint64
}

// CompactOpts tune one Compact pass.
type CompactOpts struct {
	// MinDeadRatio is the selection line: a sealed segment whose dead
	// bytes reach this fraction of its record bytes is rewritten.
	MinDeadRatio float64
	// Horizon, when nonzero, excludes segments sealed at or after it —
	// the gc grace period. The zero value means every sealed segment is
	// eligible.
	Horizon time.Time
	// Pace, when non-nil, is called with each copied record's size; the
	// collector uses it to cap copy bandwidth.
	Pace func(n int)
	// MaxCopyBytes, when non-nil, caps the live record bytes this pass
	// copies. Victim selection skips a segment whose live bytes exceed what
	// is left of the cap and goes on to smaller ones, so the pass never
	// copies more. Record bytes only: footers and allocation overhead sit
	// outside it. At a cap of 0 fully dead segments are still reclaimed,
	// since those need no copy, and nothing is sealed, since a footer would
	// grow the store. Nil, the zero value, means no cap.
	MaxCopyBytes *uint64
	// MarkVerified says that live comes from a walk that verified every object it
	// read (GetVerified), so that a bad record found here cannot have hidden anything
	// from it. The pass then leaves that record's segment in place and goes on with
	// the others. Without it, any bad record stops the pass with nothing removed: a
	// walk may have read a bad directory as something smaller, and the pass may meet
	// that directory or another bad record first, which says nothing about it.
	//
	// A pass reads only its victims, so even a stop promises nothing about a bad
	// record in a segment that is not one. A caller that needs that promise has to
	// verify what it marks.
	MarkVerified bool
}

// copyBudget is the live record bytes the pass may copy: more than any
// segment holds when there is no cap.
func (o CompactOpts) copyBudget() uint64 {
	if o.MaxCopyBytes == nil {
		return math.MaxUint64
	}
	return *o.MaxCopyBytes
}

type CompactStats struct {
	SegmentsScanned   int // sealed segments considered
	SegmentsCompacted int
	Victims           []uint64         // ids of the segments removed, ascending
	Skipped           []SkippedSegment // victims left in place, ascending by id
	RecordsCopied     int
	BytesCopied       uint64
	BytesFreed        uint64 // removed file sizes, footers included
}

// SkippedSegment is a victim that a pass left in place, because a live record in
// it failed verification. The first such record found is the one named; there may
// be more.
type SkippedSegment struct {
	ID     uint64
	Path   string
	Key    key.Key // the record's key
	Offset uint64  // the record's offset in the segment
	Err    error   // wraps ErrCorrupt, and ErrVerify when the payload is not what the key says
}

// SkippedError is what Compact returns, beside its stats, when it left victims in
// place, which it does only for a caller with a verified mark (CompactOpts.MarkVerified).
// The rest of the pass was carried out: the stats say what was removed. A segment left
// behind reports again whenever it is a victim again, until the record is repaired with
// PutVerified or nothing live refers to it.
type SkippedError struct {
	Skipped []SkippedSegment
}

func (e *SkippedError) Error() string {
	noun := "segments"
	if len(e.Skipped) == 1 {
		noun = "segment"
	}
	causes := make([]string, len(e.Skipped))
	for i, sk := range e.Skipped {
		causes[i] = sk.Err.Error()
	}
	return fmt.Sprintf("packstore: compaction left %d %s in place (repair the record with PutVerified, or drop the references that reach it): %s",
		len(e.Skipped), noun, strings.Join(causes, "; "))
}

// Unwrap gives errors.Is and errors.As each segment's cause.
func (e *SkippedError) Unwrap() []error {
	errs := make([]error, len(e.Skipped))
	for i, sk := range e.Skipped {
		errs[i] = sk.Err
	}
	return errs
}

func (fv *footerView) allEntries() iter.Seq[indexEntry] {
	return func(yield func(indexEntry) bool) {
		for i := 0; i < int(fv.keyCount); i++ {
			row := fv.entries[i*indexEntrySize:]
			var e indexEntry
			copy(e.k[:], row[:32])
			e.off = binary.BigEndian.Uint64(row[32:])
			e.slen = binary.BigEndian.Uint32(row[40:])
			if !yield(e) {
				return
			}
		}
	}
}

func (info *SegmentLiveness) add(isLive bool, slen uint32) {
	n := uint64(amberpack.RecHeaderSize) + uint64(slen)
	if isLive {
		info.LiveKeys++
		info.LiveBytes += n
	} else {
		info.DeadKeys++
		info.DeadBytes += n
	}
}

func (g *sealedSegment) liveness(live func(key.Key) bool) SegmentLiveness {
	info := SegmentLiveness{ID: g.id, Sealed: true}
	for e := range g.fv.allEntries() {
		info.add(live(e.k), e.slen)
	}
	return info
}

// Liveness classifies every record by live(k) and reports per segment,
// ascending by id with the active segment last: the GC dry run.
func (s *Store) Liveness(live func(key.Key) bool) ([]SegmentLiveness, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	var report []SegmentLiveness
	for _, g := range s.sealed {
		report = append(report, g.liveness(live))
	}
	if a := s.active; a != nil {
		info := SegmentLiveness{ID: a.id}
		for k, loc := range a.index {
			info.add(live(k), loc.slen)
		}
		report = append(report, info)
	}
	for _, fa := range s.foreign { // other writers' active segments: never victims either
		info := SegmentLiveness{ID: fa.id}
		for k, loc := range fa.scan.index {
			info.add(live(k), loc.slen)
		}
		report = append(report, info)
	}
	return report, nil
}

// Compact seals the active segment, rewrites eligible sealed segments
// whose dead ratio reaches opts.MinDeadRatio (re-verifying live records
// while copying), and deletes victims only after the copies are durable.
// The caller must guarantee no ingest or reference publication overlaps
// (specs/gc.qnt). A grey set captured since BeginBarrier is consumed and
// kept alongside live. Under opts.MaxCopyBytes a segment the cap does not
// cover is left for a later pass, and at a cap of 0 the active segment is
// not sealed either.
//
// A live record that fails verification stops the pass with nothing removed, unless
// the caller's mark verified what it read (CompactOpts.MarkVerified). Then its segment
// stays where it is, the pass goes on with the others, and it returns a *SkippedError
// beside stats that say what it did. Any other error means nothing was removed, unless
// it came from removing the victims themselves.
func (s *Store) Compact(live func(key.Key) bool, opts CompactOpts) (CompactStats, error) {
	// Other stores' writers wait for the whole pass and look at the directory
	// again afterwards (gate.go); inside a collector's BeginSweep the gate is
	// held already. This store's own writes wait too: a duplicate check must
	// not race the removal of a segment, and those that bypass the collector
	// are held by nothing else.
	end, err := s.gate.beginExclusive(context.Background())
	if err != nil {
		return CompactStats{}, err
	}
	defer end()
	s.gate.pauseLocal()
	defer s.gate.resumeLocal()
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	if grey := s.takeGrey(); len(grey) > 0 {
		marked := live
		live = func(k key.Key) bool {
			_, ok := grey[k]
			return ok || marked(k)
		}
	}

	s.mu.RLock()
	closed, failed := s.closed, s.failed
	s.mu.RUnlock()
	var stats CompactStats
	if closed {
		return stats, ErrClosed
	}
	if failed != nil {
		return stats, failed
	}
	// Sealing writes a footer, which grows the store. A zero budget asks for
	// no growth, and must still reach the dead segments it can reclaim
	// without copying. sealIdleLocked is skipped with it: it lets go of this
	// store's segment on the premise that the seal emptied it, then adopts
	// and seals whatever it finds, this one included.
	if opts.copyBudget() > 0 {
		if err := s.sealActiveLocked(); err != nil {
			return stats, err
		}
		if err := s.sealIdleLocked(); err != nil {
			return stats, err
		}
	}

	victims, err := s.selectVictims(live, opts, &stats)
	if err != nil {
		return stats, err
	}
	if len(victims) == 0 {
		return stats, nil
	}
	s.copying = true
	skipped, err := s.copyLive(victims, live, opts, &stats)
	s.copying = false
	if err != nil {
		return stats, err
	}
	if s.cfg.sync && s.active != nil {
		if err := s.fsync(s.active.f); err != nil {
			s.setFailed(err)
			return stats, err
		}
		s.active.sc.synced(s.active.size)
	}
	stats.Skipped = skipped
	s.kept = make(map[uint64]bool, len(skipped))
	for _, sk := range skipped {
		s.kept[sk.ID] = true
	}
	victims = slices.DeleteFunc(victims, func(g *sealedSegment) bool { return s.kept[g.id] })
	if err := s.removeVictims(victims, &stats); err != nil {
		return stats, err
	}
	if len(skipped) > 0 {
		return stats, &SkippedError{Skipped: skipped}
	}
	return stats, nil
}

func (s *Store) selectVictims(live func(key.Key) bool, opts CompactOpts, stats *CompactStats) ([]*sealedSegment, error) {
	s.mu.RLock()
	segs := make([]*sealedSegment, len(s.sealed))
	copy(segs, s.sealed)
	s.mu.RUnlock()
	stats.SegmentsScanned = len(segs)

	copyLeft := opts.copyBudget()
	var victims []*sealedSegment
	consider := func(g *sealedSegment) error {
		if !opts.Horizon.IsZero() {
			st, err := g.stat()
			if err != nil {
				return err
			}
			if !st.ModTime().Before(opts.Horizon) {
				return nil
			}
		}
		info := g.liveness(live)
		total := info.LiveBytes + info.DeadBytes
		if info.DeadKeys > 0 && float64(info.DeadBytes) >= opts.MinDeadRatio*float64(total) {
			// More to copy than is left of the budget: skipped. A smaller
			// segment after it is still considered.
			if info.LiveBytes > copyLeft {
				return nil
			}
			copyLeft -= info.LiveBytes
			victims = append(victims, g)
		}
		return nil
	}
	// What the last pass left in place goes last: it is charged its whole live size
	// though it will copy next to nothing, and would starve the rest for good.
	for _, kept := range []bool{false, true} {
		for _, g := range segs {
			if s.kept[g.id] != kept {
				continue
			}
			if err := consider(g); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(victims, func(a, b *sealedSegment) int { return cmp.Compare(a.id, b.id) })
	return victims, nil
}

// goodSurvivor reports whether k has a copy outside the victims that a sweep may rely
// on instead of the victim's, which means one that verifies: otherwise the victim's may
// be the only good one left. This store's own active segment is taken on trust, since it
// holds only what this pass copied and so verified: the pass sealed whatever was there
// before, and takes no segment from anybody while it copies (ensureActiveLocked).
//
// The verifying is left for outside the lock, and the callers are the pass's verifiers:
// duplicates are not always rare, what with concurrent ingests and packs left in place.
func (s *Store) goodSurvivor(k key.Key, victimID map[uint64]bool) bool {
	s.mu.RLock()
	if s.active != nil {
		if _, ok := s.active.index[k]; ok {
			s.mu.RUnlock()
			return true
		}
	}
	foreign := s.foreignCopiesLocked(k)
	var holders []*sealedSegment
	for _, g := range s.sealed {
		if !victimID[g.id] && g.has(k) && (!s.cfg.sync || !g.crashedSeal()) {
			holders = append(holders, g)
		}
	}
	if len(foreign) == 0 && len(holders) == 0 {
		s.mu.RUnlock()
		return false
	}
	s.beginScrub() // so that nothing in holders is unmapped
	s.mu.RUnlock()
	defer s.endScrub()
	for _, c := range foreign {
		if (!s.cfg.sync || c.durable) && c.raw != nil && validRepairRecord(k, c.raw) {
			return true
		}
	}
	for _, g := range holders {
		off, slen, ok := g.fv.lookup(k)
		if !ok {
			continue
		}
		if raw, err := repairRecord(g, indexEntry{k: k, off: off, slen: slen}); err == nil && validRepairRecord(k, raw) {
			return true
		}
	}
	return false
}

// foreignCopy is a copy of a record in another store's active segment. raw is nil when
// the segment does not hold what the view says it does.
type foreignCopy struct {
	fa      *foreignActive // its id and path only: the rest is under mu
	raw     []byte
	durable bool // its owner has synced it
}

// foreignCopiesLocked reads k's copies in other stores' active segments, to be verified
// outside mu, which readers and the view's changes share. The caller holds mu.
func (s *Store) foreignCopiesLocked(k key.Key) []foreignCopy {
	var out []foreignCopy
	for _, fa := range s.foreign {
		loc, ok := fa.scan.index[k]
		if !ok {
			continue
		}
		raw, err := fa.readRecord(k, loc)
		if err != nil {
			raw = nil
		}
		out = append(out, foreignCopy{fa: fa, raw: raw, durable: loc.off+amberpack.RecHeaderSize+int64(loc.slen) <= fa.scan.durable})
	}
	return out
}

// copyLive appends every live victim record that has no good copy in a surviving
// segment. Verification (decompress + re-hash) dominates, so workers verify in parallel,
// the copies a record could rely on and the record itself, while the calling goroutine
// appends whatever is ready.
//
// A record that fails verification is not copied, and stops the pass with nothing to
// remove, unless the caller's mark verified what it read (CompactOpts.MarkVerified). Then
// its segment is reported and goes no further: it stays where it is, so what it holds
// needs no copy. The pass goes on with the others, whose removal is safe by the same rule
// as ever: a live key has a copy that verified. A segment left in place is no survivor
// here, so a key with a bad copy in one victim and a good copy in another gets the good
// one copied. An append that fails stops the pass as well: the copies are not all there.
func (s *Store) copyLive(victims []*sealedSegment, live func(key.Key) bool, opts CompactOpts, stats *CompactStats) ([]SkippedSegment, error) {
	victimID := make(map[uint64]bool, len(victims))
	for _, g := range victims {
		victimID[g.id] = true
	}

	// Read-only once built, so that the walk asks without a lock.
	bad := make(map[uint64]*atomic.Bool, len(victims))
	for _, g := range victims {
		bad[g.id] = new(atomic.Bool)
	}
	var (
		skipMu  sync.Mutex
		skipped []SkippedSegment
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopped error // under skipMu: what stops the pass
	skip := func(g *sealedSegment, e indexEntry, err error) {
		if !opts.MarkVerified {
			skipMu.Lock()
			defer skipMu.Unlock()
			if stopped == nil {
				stopped = err
			}
			cancel()
			return
		}
		if bad[g.id].Swap(true) {
			return // the first one found is reported
		}
		skipMu.Lock()
		defer skipMu.Unlock()
		skipped = append(skipped, SkippedSegment{ID: g.id, Path: g.file(), Key: e.k, Offset: e.off, Err: err})
	}

	cands := make(chan copyRec, 4*runtime.GOMAXPROCS(0))
	verified := make(chan copyRec, 4*runtime.GOMAXPROCS(0))
	var pipe sync.WaitGroup
	pipe.Go(func() {
		defer close(cands)
		for _, g := range victims {
			for e := range g.fv.allEntries() {
				if bad[g.id].Load() {
					break // it stays where it is, with all it holds
				}
				if !live(e.k) {
					continue
				}
				select {
				case cands <- copyRec{g: g, e: e}:
				case <-ctx.Done():
					return
				}
			}
		}
	})
	for range runtime.GOMAXPROCS(0) {
		pipe.Go(func() {
			for c := range cands {
				if ctx.Err() != nil || bad[c.g.id].Load() {
					continue
				}
				if s.goodSurvivor(c.e.k, victimID) {
					continue
				}
				bodyLen := uint64(c.g.fv.bodyLen)
				if c.e.off < uint64(len(magicHeader)) || c.e.off > bodyLen ||
					uint64(amberpack.RecHeaderSize)+uint64(c.e.slen) > bodyLen-c.e.off {
					skip(c.g, c.e, fmt.Errorf("%w: %s: index entry out of bounds, key %s", ErrCorrupt, c.g.path, c.e.k))
					continue
				}
				c.rec = c.g.mm[c.e.off : c.e.off+uint64(amberpack.RecHeaderSize)+uint64(c.e.slen)]
				if err := verifyRecord(c.g, c.e, c.rec); err != nil {
					skip(c.g, c.e, err)
					continue
				}
				select {
				case verified <- c:
				case <-ctx.Done():
				}
			}
		})
	}
	go func() {
		pipe.Wait()
		close(verified)
	}()

	var appendErr error
	for c := range verified {
		if appendErr != nil {
			continue // drain
		}
		if s.active != nil {
			if _, ok := s.active.index[c.e.k]; ok {
				continue // the same key from another victim, already copied
			}
		}
		if appendErr = s.appendLocked(c.e.k, c.rec, false); appendErr != nil {
			cancel()
			continue
		}
		stats.RecordsCopied++
		stats.BytesCopied += uint64(len(c.rec))
		if opts.Pace != nil {
			opts.Pace(len(c.rec))
		}
	}
	if appendErr != nil {
		return nil, appendErr
	}
	if stopped != nil {
		return nil, stopped
	}
	slices.SortFunc(skipped, func(a, b SkippedSegment) int { return cmp.Compare(a.ID, b.ID) })
	return skipped, nil
}

func (s *Store) removeVictims(victims []*sealedSegment, stats *CompactStats) error {
	if len(victims) == 0 {
		return nil // every victim was left in place: nothing to detach or sync
	}
	victimID := make(map[uint64]bool, len(victims))
	for _, g := range victims {
		victimID[g.id] = true
	}
	// A lookup that finds nothing lists the directory (view.go). Between the
	// victims leaving the view and leaving the directory, such a listing
	// would map them again, and the duplicate check would go on finding what
	// is gone. No listing runs in between: refreshMu is held. Unlinking does
	// not have to wait for scrubs, only unmapping does — and must not wait
	// here: a scrub's callback may be waiting for refreshMu.
	s.refreshMu.Lock()
	s.mu.Lock()
	// Fresh slice: never mutate an array a concurrent reader may still
	// hold (same discipline as Remove).
	kept := make([]*sealedSegment, 0, len(s.sealed)-len(victims))
	for _, g := range s.sealed {
		if !victimID[g.id] {
			kept = append(kept, g)
		}
	}
	s.sealed = kept
	s.mu.Unlock()
	if s.afterDetach != nil {
		s.afterDetach()
	}
	var firstErr error
	for _, g := range victims {
		if err := os.Remove(g.file()); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stats.Victims = append(stats.Victims, g.id)
		stats.BytesFreed += uint64(len(g.mm))
	}
	if s.cfg.sync {
		if err := s.fsync(s.dirF); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.refreshMu.Unlock()

	s.waitScrubs() // in-flight scrub walks may still read the victims' mmaps
	for _, g := range victims {
		if err := g.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	stats.SegmentsCompacted = len(stats.Victims)
	return firstErr
}

// verifyRecord checks rec against its index entry: framing CRC, key match,
// and the payload rehashed against the key.
func verifyRecord(g *sealedSegment, e indexEntry, rec []byte) error {
	if err := checkRecord(e.k, rec); err != nil {
		return fmt.Errorf("%w: %s: record at offset %d, key %s: %w", ErrCorrupt, g.path, e.off, e.k, err)
	}
	return nil
}

// checkRecord is the full record scrub: framing CRC, key match, and the
// payload rehashed against the key.
func checkRecord(k key.Key, rec []byte) error {
	parsed, err := amberpack.ParseRecord(rec)
	if err != nil {
		return err
	}
	if parsed.Key != k {
		return fmt.Errorf("record keyed %s, expected %s", parsed.Key, k)
	}
	payload, err := amberpack.DecodePayload(parsed.Flags, parsed.Ulen, rec[amberpack.RecHeaderSize:])
	if err != nil {
		return err
	}
	return verifyObject(Object{Key: k, Data: payload})
}
