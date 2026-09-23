package packstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// gateFile is the cross-process form of the collector's reference lock, and
// carries the view generation in its first 8 bytes (big-endian).
const gateFile = "gc.lock"

// Waits for the file lock poll, backing off to maxGatePoll. A store that
// just swept leaves the lock alone for sweepYield, two polls' worth, before
// it sweeps again: it would otherwise take the lock back within microseconds
// of dropping it, and a writer polling from another process could wait as
// long as the sweeps keep coming.
const (
	maxGatePoll = 50 * time.Millisecond
	sweepYield  = 2 * maxGatePoll
)

// A GC cycle must not lose an object that a writer just relied on: written,
// or skipped as a duplicate of a record the cycle is about to reap. Within
// one process the write barrier and the collector's reference lock see to
// that (specs/gc.qnt, policy "barrier"). Across processes the gate applies
// the simpler safe policy, "quiesce", with one flock:
//
//   - shared, while any write span of this store is in flight: every
//     exported write, and the spans callers bracket with BeginWrite (a
//     completeness walk followed by a reference put);
//   - exclusive, for a whole GC cycle, and for anything else that deletes
//     segments (Compact, Wipe, Remove).
//
// So while one store sweeps, writers in other processes wait; readers never
// do. Writers of the sweeping store itself do not touch the file lock while
// their store holds it exclusively: the barrier and the reference lock deal
// with them, as before.
//
// flock cannot convert between shared and exclusive atomically — the lock is
// dropped in between, and another process may take it — so the gate never
// converts: exclusive is taken and dropped only with no local span in flight.
//
// Whoever held the lock exclusively bumps the generation before letting go.
// A store that takes the shared lock and finds the generation moved looks at
// the directory again before it does anything else: it may still have a
// reaped segment mapped, and its duplicate check would find objects there
// that are gone.
type gate struct {
	f       *os.File
	refresh func() error // brings the store's view up to date
	done    chan struct{}

	mu        sync.Mutex
	cond      *sync.Cond
	shared    int       // local write spans in flight
	flocked   bool      // the file lock is held shared on their behalf
	exclusive int       // depth: this store holds the file lock exclusively
	busy      bool      // one goroutine is changing the file lock's state; the others wait
	blocked   bool      // new local spans wait: a sweep is coming, going, or at work
	gen       uint64    // the generation this store's view is good for
	sweptAt   time.Time // when this store last dropped the exclusive lock
	closed    bool

	ignoreGeneration bool // test hook: reproduces the loss the generation prevents
}

func openGate(dir string, refresh func() error) (*gate, error) {
	f, err := os.OpenFile(filepath.Join(dir, gateFile), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("packstore: %s: %w", gateFile, err)
	}
	g := &gate{f: f, refresh: refresh, done: make(chan struct{})}
	g.cond = sync.NewCond(&g.mu)
	// Before the first look at the directory: a sweep that falls in between
	// then shows as a generation the view has yet to catch up with.
	if g.gen, err = g.generation(); err != nil {
		f.Close()
		return nil, err
	}
	return g, nil
}

func (g *gate) generation() (uint64, error) {
	var b [8]byte
	n, err := g.f.ReadAt(b[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("packstore: %s: %w", gateFile, err)
	}
	if n < len(b) {
		return 0, nil // never swept
	}
	return binary.BigEndian.Uint64(b[:]), nil
}

// poll takes the file lock in the given mode, trying without blocking so
// that the wait ends with ctx or with the store.
func (g *gate) poll(ctx context.Context, how int) error {
	for delay := time.Millisecond; ; delay = min(2*delay, maxGatePoll) {
		switch err := unix.Flock(int(g.f.Fd()), how|unix.LOCK_NB); err {
		case nil:
			return nil
		case unix.EWOULDBLOCK, unix.EINTR:
		default:
			return fmt.Errorf("packstore: %s: %w", gateFile, err)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		case <-g.done:
			return ErrClosed
		}
	}
}

func (g *gate) unlock() { unix.Flock(int(g.f.Fd()), unix.LOCK_UN) }

// waitLocked waits on the condition until ok reports true, ctx ends or the
// gate closes. The caller holds mu.
func (g *gate) waitLocked(ctx context.Context, ok func() bool) error {
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stop()
	for !ok() {
		if g.closed {
			return ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		g.cond.Wait()
	}
	if g.closed {
		return ErrClosed
	}
	return nil
}

// beginShared opens a write span.
func (g *gate) beginShared() error {
	ctx := context.Background()
	g.mu.Lock()
	if err := g.waitLocked(ctx, func() bool { return !g.busy && !g.blocked }); err != nil {
		g.mu.Unlock()
		return err
	}
	if g.exclusive > 0 || g.flocked {
		g.shared++
		g.mu.Unlock()
		return nil
	}
	g.busy = true // the first span takes the file lock; the rest wait for it here
	g.mu.Unlock()

	err := g.poll(ctx, unix.LOCK_SH)
	gen := uint64(0)
	if err == nil {
		if gen, err = g.generation(); err == nil && gen != g.gen && !g.ignoreGeneration {
			err = g.refresh() // before any span proceeds to a duplicate check
		}
		if err != nil {
			g.unlock()
		}
	}
	g.mu.Lock()
	g.busy = false
	if err == nil {
		g.flocked, g.shared, g.gen = true, 1, gen
	}
	g.cond.Broadcast()
	g.mu.Unlock()
	return err
}

func (g *gate) endShared() {
	g.mu.Lock()
	g.shared--
	if g.shared == 0 && g.flocked {
		g.unlock()
		g.flocked = false
	}
	g.cond.Broadcast()
	g.mu.Unlock()
}

// beginExclusive takes the gate for a sweep: it waits out this store's
// spans, then every other store's, and refreshes the view, which from then
// on is complete and stable. Calls nest: Compact inside a collector's cycle
// finds the gate already held. It must not be called from inside a write
// span of the same store, which it would wait for.
func (g *gate) beginExclusive(ctx context.Context) (end func(), err error) {
	g.mu.Lock()
	if err := g.waitLocked(ctx, func() bool { return !g.busy && !g.blocked }); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.exclusive > 0 {
		g.exclusive++
		g.mu.Unlock()
		return g.endExclusive, nil
	}
	g.blocked = true
	if err := g.waitLocked(ctx, func() bool { return g.shared == 0 }); err != nil {
		g.blocked = false
		g.cond.Broadcast()
		g.mu.Unlock()
		return nil, err
	}
	g.busy = true
	g.mu.Unlock()

	err = g.poll(ctx, unix.LOCK_EX)
	g.mu.Lock()
	g.busy, g.blocked = false, false
	if err == nil {
		g.exclusive = 1
	}
	g.cond.Broadcast()
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := g.refresh(); err != nil {
		g.endExclusive()
		return nil, err
	}
	return g.endExclusive, nil
}

func (g *gate) endExclusive() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exclusive > 1 {
		g.exclusive--
		return
	}
	// Local writes ran under the exclusive lock without a file lock of their
	// own. None may be in flight when it goes, or another store could sweep
	// under them; and the lock cannot be turned into a shared one.
	for g.busy || g.blocked {
		g.cond.Wait()
	}
	g.blocked = true
	for g.shared > 0 && !g.closed {
		g.cond.Wait()
	}
	var b [8]byte
	g.gen++
	binary.BigEndian.PutUint64(b[:], g.gen)
	g.f.WriteAt(b[:], 0) // not synced: it only has to outlive the processes that are running
	g.unlock()
	g.exclusive, g.blocked, g.sweptAt = 0, false, time.Now()
	g.cond.Broadcast()
}

// yield waits out what is left of sweepYield since this store's last sweep.
func (g *gate) yield(ctx context.Context) error {
	g.mu.Lock()
	wait := time.Until(g.sweptAt.Add(sweepYield))
	if g.exclusive > 0 {
		wait = 0 // nested: the lock is held, nobody is being kept waiting for it
	}
	g.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-g.done:
		return ErrClosed
	}
}

// pauseLocal keeps this store's own write spans out, waiting for the ones in
// flight, until resumeLocal. Compact runs inside the pair: a write's
// duplicate check must not race the removal of a segment, and writes that
// bypass the collector are held by nothing else.
func (g *gate) pauseLocal() {
	g.mu.Lock()
	for (g.busy || g.blocked) && !g.closed {
		g.cond.Wait()
	}
	g.blocked = true
	for g.shared > 0 && !g.closed {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

func (g *gate) resumeLocal() {
	g.mu.Lock()
	g.blocked = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gate) close() error {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		close(g.done)
	}
	g.cond.Broadcast()
	g.mu.Unlock()
	return g.f.Close() // drops whatever lock is held
}

// BeginWrite opens a write span: until done is called, no other store can
// sweep. Every exported write opens one itself; callers bracket larger spans
// that must not straddle a sweep, such as a completeness walk followed by a
// reference put. If another store has swept since this one last looked, the
// view is refreshed before BeginWrite returns. Spans nest and cost nothing
// extra while one is open.
func (s *Store) BeginWrite() (done func(), err error) {
	if err := s.gate.beginShared(); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(s.gate.endShared) }, nil
}

// BeginSweep takes the store for a GC cycle: it waits for this store's write
// spans and every other store's to end, keeps other stores' writers out
// until done is called, and refreshes the view, which is then complete and
// stable. This store's own writers go on: the write barrier and the
// collector's reference lock are what holds them where needed. done bumps
// the view generation, so that every other store looks at the directory
// again before its next write. Compact, Wipe and Remove take the gate
// themselves; inside a BeginSweep they find it held.
func (s *Store) BeginSweep(ctx context.Context) (done func(), err error) {
	if err := s.gate.yield(ctx); err != nil {
		return nil, err
	}
	end, err := s.gate.beginExclusive(ctx)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(end) }, nil
}
