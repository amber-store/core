package packstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amber-store/core/amberpack"
)

// sealedStore builds a store with sealed segments and returns its dir.
func sealedStore(t *testing.T, objs []Object) string {
	t.Helper()
	dir := t.TempDir()
	s := openStore(t, dir, WithSegmentSize(8<<10))
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerifyCleanStore(t *testing.T) {
	dir := sealedStore(t, testObjects(t, 100))
	s := openStore(t, dir)
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDetectsBodyCorruption(t *testing.T) {
	dir := sealedStore(t, testObjects(t, 100))

	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) == 0 {
		t.Fatal("no sealed segments")
	}
	b, err := os.ReadFile(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	// Flip one payload byte inside the body, far from the footer: Open's
	// footer CRC does not cover the body, so this must surface in Verify.
	b[100] ^= 0x01
	if err := os.WriteFile(segs[0], b, 0o644); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, dir) // Open succeeds: footer is intact
	if err := s.Verify(context.Background()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify = %v, want ErrCorrupt", err)
	}
}

func TestVerifyDetectsWrongIndexEntry(t *testing.T) {
	// Craft a segment whose footer is internally consistent (valid CRC) but
	// whose index lies about an offset — the writer-bug class that only a
	// body/index cross-check can catch.
	objs := testObjects(t, 20)
	var body []byte
	body = append(body, magicHeader...)
	var entries []indexEntry
	for _, o := range objs {
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, indexEntry{k: o.Key, off: uint64(len(body)), slen: uint32(len(rec) - amberpack.RecHeaderSize)})
		body = append(body, rec...)
	}
	entries[3].off = entries[2].off // lie
	footer, err := buildFooter(int64(len(body)), entries)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "0000000000000001.seg")
	if err := os.WriteFile(path, append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, dir)
	if err := s.Verify(context.Background()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify = %v, want ErrCorrupt", err)
	}
}

func TestVerifyHonorsContext(t *testing.T) {
	dir := sealedStore(t, testObjects(t, 100))
	s := openStore(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Verify(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify = %v, want context.Canceled", err)
	}
}

func TestVerifyIgnoresActiveSegment(t *testing.T) {
	// Active-segment records are covered by reopen tail-scans, not Verify;
	// Verify only walks sealed segments. This test pins that behaviour: a
	// store with only an active segment verifies clean.
	dir := t.TempDir()
	s := openStore(t, dir)
	for _, o := range testObjects(t, 5) {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyConcurrentWithClose(t *testing.T) {
	// Close used to munmap under a running scrub: uncatchable SIGSEGV. The
	// scrub gate makes Close wait for in-flight walks.
	for round := 0; round < 5; round++ {
		dir := sealedStore(t, testObjects(t, 200))
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- s.Verify(context.Background()) }()
		time.Sleep(2 * time.Millisecond)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("Verify during Close: %v", err)
		}
	}
}

func TestVerifyDetectsKeyCountMismatch(t *testing.T) {
	// A consistent footer built over N+1 entries with only N body records:
	// Open passes (trailer/index/filter all self-consistent), the scrub's
	// keyCount cross-check must fire.
	objs := testObjects(t, 10)
	var body []byte
	body = append(body, magicHeader...)
	var entries []indexEntry
	for _, o := range objs {
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, indexEntry{k: o.Key, off: uint64(len(body)), slen: uint32(len(rec) - amberpack.RecHeaderSize)})
		body = append(body, rec...)
	}
	ghost := blobObj(t, []byte("never written to the body"))
	entries = append(entries, indexEntry{k: ghost.Key, off: entries[0].off, slen: entries[0].slen})
	footer, err := buildFooter(int64(len(body)), entries)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0000000000000001.seg"), append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir)
	if err := s.Verify(context.Background()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify = %v, want ErrCorrupt", err)
	}
}

func TestVerifyScrubHashMismatchIsCorrupt(t *testing.T) {
	// A record whose CRC is valid but whose payload doesn't hash to its key
	// (writer-bug class): scrub must classify it as ErrCorrupt (and ErrVerify).
	good := testObjects(t, 3)
	imposter := blobObj(t, []byte("imposter payload"))
	var body []byte
	body = append(body, magicHeader...)
	var entries []indexEntry
	for i, o := range good {
		data := o.Data
		if i == 1 {
			data = imposter.Data // encoded under good[1].Key: CRC fine, hash wrong
		}
		rec, err := amberpack.EncodeRecord(o.Key, data)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, indexEntry{k: o.Key, off: uint64(len(body)), slen: uint32(len(rec) - amberpack.RecHeaderSize)})
		body = append(body, rec...)
	}
	footer, err := buildFooter(int64(len(body)), entries)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0000000000000001.seg"), append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir)
	err = s.Verify(context.Background())
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("want ErrVerify too, got %v", err)
	}
}
