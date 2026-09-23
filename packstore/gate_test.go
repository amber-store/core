package packstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Two stores on one directory stand in for two processes: gc.lock is a
// flock, which belongs to the open file.

const (
	gateWait  = 10 * time.Second       // a wait that must end
	gateQuiet = 150 * time.Millisecond // how long "still waiting" is watched
)

// async runs f and returns a channel that is closed when it has returned.
func async(f func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	return done
}

func stillWaiting(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s did not wait", what)
	case <-time.After(gateQuiet):
	}
}

func finishes(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(gateWait):
		t.Fatalf("%s is still waiting", what)
	}
}

func beginWrite(t *testing.T, s *Store) func() {
	t.Helper()
	done, err := s.BeginWrite()
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func beginSweep(t *testing.T, s *Store) func() {
	t.Helper()
	done, err := s.BeginSweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func twoStores(t *testing.T) (a, b *Store) {
	t.Helper()
	dir := t.TempDir()
	return openStore(t, dir, WithSync(false)), openStore(t, dir, WithSync(false))
}

func TestWriteSpanBlocksAForeignSweep(t *testing.T) {
	a, b := twoStores(t)
	endSpan := beginWrite(t, a)
	var endSweep func()
	var err error
	swept := async(func() { endSweep, err = b.BeginSweep(context.Background()) })
	stillWaiting(t, swept, "a sweep, while another store is in a write span,")
	endSpan()
	finishes(t, swept, "the sweep, after the write span ended,")
	if err != nil {
		t.Fatal(err)
	}
	endSweep()
}

func TestSweepBlocksForeignWrites(t *testing.T) {
	a, b := twoStores(t)
	objs := testObjects(t, 3)
	endSweep := beginSweep(t, a)

	var endSpan func()
	var spanErr, putErr, batchErr error
	span := async(func() { endSpan, spanErr = b.BeginWrite() })
	put := async(func() { putErr = b.Put(objs[0].Key, objs[0].Data) })
	batch := async(func() { batchErr = b.WriteBatch(objSeq(objs[1:], -1)) })
	stillWaiting(t, span, "a write span, while another store sweeps,")
	stillWaiting(t, put, "a Put, while another store sweeps,")
	stillWaiting(t, batch, "a WriteBatch, while another store sweeps,")
	endSweep()
	finishes(t, span, "the write span, after the sweep ended,")
	finishes(t, put, "the Put, after the sweep ended,")
	finishes(t, batch, "the WriteBatch, after the sweep ended,")
	if err := errors.Join(spanErr, putErr, batchErr); err != nil {
		t.Fatal(err)
	}
	endSpan()
	wantObjects(t, a, objs)
}

// Writers in the sweeping store's own process are not held up by the file
// lock: the write barrier and the collector's reference lock deal with them.
func TestLocalWritesRunUnderALocalSweepLock(t *testing.T) {
	a, _ := twoStores(t)
	o := testObjects(t, 1)[0]
	endSweep := beginSweep(t, a)
	var err error
	finishes(t, async(func() { err = a.Put(o.Key, o.Data) }), "a Put in the store that holds the sweep lock")
	if err != nil {
		t.Fatal(err)
	}
	finishes(t, async(endSweep), "releasing the sweep lock")
}

func TestNestedWriteSpansCount(t *testing.T) {
	a, b := twoStores(t)
	outer, inner := beginWrite(t, a), beginWrite(t, a)
	var endSweep func()
	var err error
	swept := async(func() { endSweep, err = b.BeginSweep(context.Background()) })
	inner()
	stillWaiting(t, swept, "a sweep, with one of two nested spans still open,")
	outer()
	finishes(t, swept, "the sweep, after both spans ended,")
	if err != nil {
		t.Fatal(err)
	}
	endSweep()
}

func TestBeginSweepHonoursContext(t *testing.T) {
	a, b := twoStores(t)
	endSpan := beginWrite(t, a)
	defer endSpan()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := b.BeginSweep(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BeginSweep with an expiring context = %v", err)
	}
	// Giving up must not leave the store's own writers held.
	o := testObjects(t, 1)[0]
	var err error
	finishes(t, async(func() { err = b.Put(o.Key, o.Data) }), "a Put after a sweep gave up")
	if err != nil {
		t.Fatal(err)
	}
}

func TestGenerationMovesOnSweepAndWipe(t *testing.T) {
	a, _ := twoStores(t)
	generation := func() uint64 {
		t.Helper()
		g, err := a.gate.generation()
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	start := generation()
	beginSweep(t, a)()
	if got := generation(); got != start+1 {
		t.Fatalf("generation after a sweep = %d, want %d", got, start+1)
	}
	if err := a.Wipe(); err != nil {
		t.Fatal(err)
	}
	if got := generation(); got != start+2 {
		t.Fatalf("generation after a wipe = %d, want %d", got, start+2)
	}
}

// A store that mapped a segment keeps the mapping when another store reaps
// it. Its duplicate check would then find an object that is gone, skip the
// write, and lose it. The generation makes it look again first. With the
// check disabled the object is lost, which shows that this test bites.
func TestStaleWriterRefreshesBeforeDedup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ignore bool
	}{{"generation honoured", false}, {"generation ignored", true}} {
		t.Run(tc.name, func(t *testing.T) {
			a, objs := compactStore(t) // objs[0..1] and objs[2..3] sealed, objs[4] active
			b := openStore(t, a.dir, WithSync(false))
			b.gate.neverRefresh = tc.ignore
			doomed := objs[1]
			if has, err := b.Has(doomed.Key); err != nil || !has {
				t.Fatalf("Has = %v, %v", has, err)
			}
			if _, err := a.Compact(liveSet(objs, 0, 2, 3, 4), CompactOpts{MinDeadRatio: 0.1}); err != nil {
				t.Fatal(err)
			}
			// As an ingest of a tree that still contains the object would.
			if err := b.Put(doomed.Key, doomed.Data); err != nil {
				t.Fatal(err)
			}
			_, err := openStore(t, a.dir).Get(doomed.Key)
			switch {
			case tc.ignore && !errors.Is(err, ErrNotFound):
				t.Fatalf("Get = %v: without the generation check the object should be lost, or this test proves nothing", err)
			case !tc.ignore && err != nil:
				t.Fatalf("the object was skipped as a duplicate of a record that had been reaped: %v", err)
			}
		})
	}
}

func TestWipeWaitsForAForeignWriteSpan(t *testing.T) {
	a, b := twoStores(t)
	o := testObjects(t, 1)[0]
	if err := b.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil { // leaves an idle segment for the wipe to take
		t.Fatal(err)
	}
	endSpan := beginWrite(t, a)
	c := openStore(t, a.dir, WithSync(false))
	var err error
	wiped := async(func() { err = c.Wipe() })
	stillWaiting(t, wiped, "a Wipe, while another store is in a write span,")
	endSpan()
	finishes(t, wiped, "the Wipe, after the write span ended,")
	if err != nil {
		t.Fatal(err)
	}
	// Asked of a store that opens now: a's view, brought up to date by its
	// span, still reads the wiped segment through the file it holds open,
	// as any view reads a reaped one until it next looks at the directory.
	if has, _ := openStore(t, a.dir).Has(o.Key); has {
		t.Fatal("the wiped object is still there")
	}
}

// Compact also waits for this store's own writes that bypass the collector:
// their duplicate check must not race the removal of a segment.
func TestCompactWaitsForLocalWriteSpans(t *testing.T) {
	s, objs := compactStore(t)
	endSpan := beginWrite(t, s)
	var err error
	compacted := async(func() { _, err = s.Compact(liveSet(objs, 0, 2, 4), CompactOpts{MinDeadRatio: 0.1}) })
	stillWaiting(t, compacted, "Compact, while a write span of its own store is open,")
	endSpan()
	finishes(t, compacted, "Compact, after the write span ended,")
	if err != nil {
		t.Fatal(err)
	}
}

// A store that sweeps again and again must not starve a writer in another
// process, which polls for the lock: between two sweeps the lock is free for
// microseconds. Without the yield the writer below gets in only by luck.
func TestBackToBackSweepsYieldToAWaitingWriter(t *testing.T) {
	a, b := twoStores(t)
	o := testObjects(t, 1)[0]
	endFirst := beginSweep(t, a)
	var putErr error
	put := async(func() { putErr = b.Put(o.Key, o.Data) })
	stillWaiting(t, put, "a Put, while another store sweeps,")
	endFirst()

	sweeps := 0
	for ; sweeps < 200; sweeps++ {
		select {
		case <-put:
		default:
			beginSweep(t, a)()
			continue
		}
		break
	}
	finishes(t, put, "the Put")
	if putErr != nil {
		t.Fatal(putErr)
	}
	if sweeps > 20 {
		t.Fatalf("the writer got in only after %d further sweeps", sweeps)
	}
}

// Stores that sweep in turn must each move the generation past what the file
// holds, not past what they remember from their own last look at it: a store
// that remembered an older value would write the current one again, and a
// writer that had seen it already would not look at the directory again.
func TestEverySweepMovesTheGeneration(t *testing.T) {
	a, objs := compactStore(t)                // objs[0..1] and objs[2..3] sealed, objs[4] active
	b := openStore(t, a.dir, WithSync(false)) // opens before any sweep
	beginSweep(t, a)()
	w := openStore(t, a.dir, WithSync(false)) // opens after a's sweep
	first := blobObj(t, []byte("w's first write brings its view, and the generation it holds, up to date"))
	if err := w.Put(first.Key, first.Data); err != nil {
		t.Fatal(err)
	}
	doomed := objs[1]
	if has, err := w.Has(doomed.Key); err != nil || !has {
		t.Fatalf("Has = %v, %v", has, err)
	}
	before, err := w.gate.generation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Compact(liveSet(objs, 0, 2, 3, 4), CompactOpts{MinDeadRatio: 0.1}); err != nil {
		t.Fatal(err)
	}
	after, err := w.gate.generation()
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Errorf("generation %d after a second store swept, %d before: every sweep must move it", after, before)
	}
	if err := w.Put(doomed.Key, doomed.Data); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t, a.dir).Get(doomed.Key); err != nil {
		t.Fatalf("the object was skipped as a duplicate of a record that had been reaped: %v", err)
	}
}

// A write inside an open span — a Put inside a BeginWrite bracket, or a second
// bracket — must not wait for a sweep of the same store that is waiting for
// the bracket to close: each would wait for the other.
func TestWritesInsideASpanPassAWaitingSweep(t *testing.T) {
	sweeps := map[string]func(*Store, []Object) error{
		"Compact": func(s *Store, objs []Object) error {
			_, err := s.Compact(liveSet(objs, 0, 2, 4), CompactOpts{MinDeadRatio: 0.1})
			return err
		},
		"Wipe": func(s *Store, _ []Object) error { return s.Wipe() },
		"BeginSweep": func(s *Store, _ []Object) error {
			done, err := s.BeginSweep(context.Background())
			if err == nil {
				done()
			}
			return err
		},
	}
	for name, sweep := range sweeps {
		t.Run(name, func(t *testing.T) {
			s, objs := compactStore(t)
			endSpan := beginWrite(t, s)
			defer endSpan() // lets everybody go when the test fails half way
			var sweepErr error
			swept := async(func() { sweepErr = sweep(s, objs) })
			stillWaiting(t, swept, name+", while a write span of its own store is open,")

			o := blobObj(t, []byte("written inside the span"))
			var putErr, nestErr error
			inside := async(func() {
				putErr = s.Put(o.Key, o.Data)
				var end func()
				if end, nestErr = s.BeginWrite(); nestErr == nil {
					end()
				}
			})
			finishes(t, inside, "a write inside the open span, with "+name+" waiting for the span,")
			if putErr != nil || nestErr != nil {
				t.Fatalf("Put = %v, BeginWrite = %v", putErr, nestErr)
			}
			endSpan()
			finishes(t, swept, name+", after the span ended,")
			if sweepErr != nil {
				t.Fatal(sweepErr)
			}
		})
	}
}

// A store that opens while another is in the middle of a sweep lists a
// directory that is about to change, and no generation it could read then
// tells it so reliably. Its first write span therefore looks again. With the
// refresh disabled the object is lost, which shows that this test bites.
func TestStoreOpenedDuringASweepLooksAgainBeforeItsFirstWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		never bool
	}{{"first span refreshes", false}, {"no refresh", true}} {
		t.Run(tc.name, func(t *testing.T) {
			a, objs := compactStore(t)
			endSweep := beginSweep(t, a)
			x := openStore(t, a.dir, WithSync(false)) // in the middle of a's sweep
			x.gate.neverRefresh = tc.never
			doomed := objs[1]
			if has, err := x.Has(doomed.Key); err != nil || !has {
				t.Fatalf("Has = %v, %v", has, err)
			}
			if _, err := a.Compact(liveSet(objs, 0, 2, 3, 4), CompactOpts{MinDeadRatio: 0.1}); err != nil {
				t.Fatal(err)
			}
			endSweep()
			if err := x.Put(doomed.Key, doomed.Data); err != nil {
				t.Fatal(err)
			}
			_, err := openStore(t, a.dir).Get(doomed.Key)
			switch {
			case tc.never && !errors.Is(err, ErrNotFound):
				t.Fatalf("Get = %v: without the refresh the object should be lost, or this test proves nothing", err)
			case !tc.never && err != nil:
				t.Fatalf("the object was skipped as a duplicate of a record that had been reaped: %v", err)
			}
		})
	}
}

// The generation moves when the sweep begins, not when it ends: a sweeper
// that dies after deleting segments has still told every other store.
func TestGenerationMovesBeforeAnythingIsDeleted(t *testing.T) {
	a, _ := twoStores(t)
	start, err := a.gate.generation()
	if err != nil {
		t.Fatal(err)
	}
	endSweep := beginSweep(t, a)
	defer endSweep()
	if got, err := a.gate.generation(); err != nil || got != start+1 {
		t.Fatalf("generation inside a sweep = %d, %v; want %d", got, err, start+1)
	}
}
