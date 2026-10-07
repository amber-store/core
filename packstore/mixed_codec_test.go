package packstore

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// TestMixedCodecStore fills one store through five handles, each with its own
// compression, and then reads it through a handle with the default: after a
// plain reopen, after losing every sidecar, and after a compaction.
func TestMixedCodecStore(t *testing.T) {
	dir := t.TempDir()
	settings := []amberpack.Compression{
		{}, zstdDefault, lz4Fast, zstd19, {Algorithm: amberpack.LZ4, Level: 9},
	}
	var objs []Object
	for si, c := range settings {
		s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false), WithCompression(c))
		if err != nil {
			t.Fatal(err)
		}
		for i := range 12 {
			data := compressible(3000)
			if i%3 == 0 {
				data = incompressible(3000)
			}
			o := blobObj(t, append(data, byte(si), byte(i)))
			if err := s.Put(o.Key, o.Data); err != nil {
				t.Fatal(err)
			}
			objs = append(objs, o)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	reopen := func() *Store {
		t.Helper()
		s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// readAll checks every object and returns each one's codec.
	readAll := func(s *Store) map[key.Key]amberpack.Algorithm {
		t.Helper()
		codecs := make(map[key.Key]amberpack.Algorithm, len(objs))
		for _, o := range objs {
			data, err := s.Get(o.Key)
			if err != nil || !bytes.Equal(data, o.Data) {
				t.Fatalf("Get(%s): %d bytes, %v", o.Key, len(data), err)
			}
			codecs[o.Key] = codecOf(t, s, o.Key)
		}
		return codecs
	}

	s := reopen()
	before := readAll(s)
	count := map[amberpack.Algorithm]int{}
	for _, c := range before {
		count[c]++
	}
	for _, a := range []amberpack.Algorithm{amberpack.None, amberpack.Zstd, amberpack.LZ4} {
		if count[a] == 0 {
			t.Fatalf("the store holds no %s record: %v", a, count)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Without their sidecars the active segments are indexed by scanning them.
	sidecars, err := filepath.Glob(filepath.Join(dir, "*"+sidecarSuffix))
	if err != nil || len(sidecars) == 0 {
		t.Fatalf("sidecars = %v, %v; want at least one", sidecars, err)
	}
	for _, p := range sidecars {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	s = reopen()
	defer s.Close()
	if got := readAll(s); !maps.Equal(got, before) {
		t.Fatal("codecs changed across a reopen without sidecars")
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Compaction copies the live records as they are.
	live := func(k key.Key) bool { return k[0]%2 == 0 }
	if _, err := s.Compact(live, CompactOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if !live(o.Key) {
			continue
		}
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("after compaction, Get(%s): %d bytes, %v", o.Key, len(data), err)
		}
		if got := codecOf(t, s, o.Key); got != before[o.Key] {
			t.Fatalf("compaction changed %s from %s to %s", o.Key, before[o.Key], got)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}
