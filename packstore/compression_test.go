package packstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

var (
	lz4Fast = amberpack.Compression{Algorithm: amberpack.LZ4}
	zstd19  = amberpack.Compression{Algorithm: amberpack.Zstd, Level: 19}
)

// codecOf returns the codec id of k's stored record.
func codecOf(t *testing.T, s *Store, k key.Key) amberpack.Algorithm {
	t.Helper()
	rec, err := s.GetRecord(k)
	if err != nil {
		t.Fatalf("GetRecord(%s): %v", k, err)
	}
	return amberpack.Algorithm(rec[33])
}

// rawStore opens a store with the given options and nothing else: unlike
// openStore it adds no compression of its own.
func rawStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), append([]Option{WithSync(false)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// distinct returns n compressible objects that differ from one another.
func distinct(t *testing.T, n int) []Object {
	t.Helper()
	objs := make([]Object, n)
	for i := range objs {
		objs[i] = blobObj(t, append(compressible(4096), byte(i), byte(i>>8)))
	}
	return objs
}

func mustGet(t *testing.T, s *Store, o Object) {
	t.Helper()
	data, err := s.Get(o.Key)
	if err != nil || !bytes.Equal(data, o.Data) {
		t.Fatalf("Get(%s): %d bytes, %v; want the %d bytes stored", o.Key, len(data), err, len(o.Data))
	}
}

func TestDefaultStoreWritesRaw(t *testing.T) {
	s := rawStore(t)
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.None {
		t.Fatalf("codec = %s, want none", c)
	}
	if n, ok, err := s.StoredSize(o.Key); err != nil || !ok || n != uint64(len(o.Data)) {
		t.Fatalf("StoredSize = %d, %v, %v; want %d", n, ok, err, len(o.Data))
	}
	mustGet(t, s, o)
}

func TestWithCompressionOnEveryWritePath(t *testing.T) {
	paths := map[string]func(*Store, []Object) error{
		"Put": func(s *Store, objs []Object) error {
			for _, o := range objs {
				if err := s.Put(o.Key, o.Data); err != nil {
					return err
				}
			}
			return nil
		},
		"PutVerified": func(s *Store, objs []Object) error {
			for _, o := range objs {
				if err := s.PutVerified(o.Key, o.Data); err != nil {
					return err
				}
			}
			return nil
		},
		"WriteBatch": func(s *Store, objs []Object) error { return s.WriteBatch(objSeq(objs, -1)) },
		"WriteParallel": func(s *Store, objs []Object) error {
			_, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4})
			return err
		},
	}
	for _, c := range []amberpack.Compression{zstdDefault, zstd19, lz4Fast, {Algorithm: amberpack.LZ4, Level: 9}} {
		for name, write := range paths {
			t.Run(c.String()+"/"+name, func(t *testing.T) {
				s := rawStore(t, WithCompression(c))
				objs := distinct(t, 6)
				if err := write(s, objs); err != nil {
					t.Fatal(err)
				}
				for _, o := range objs {
					if got := codecOf(t, s, o.Key); got != c.Algorithm {
						t.Fatalf("codec = %s, want %s", got, c.Algorithm)
					}
					mustGet(t, s, o)
				}
			})
		}
	}
}

func TestCompressionForReceivesKeyDataAndDefault(t *testing.T) {
	type call struct {
		k    key.Key
		data []byte
		def  amberpack.Compression
	}
	var mu sync.Mutex
	var calls []call
	s := rawStore(t, WithCompression(lz4Fast), WithCompressionFor(
		func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
			mu.Lock()
			calls = append(calls, call{k, bytes.Clone(data), def})
			mu.Unlock()
			return def
		}))
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].k != o.Key || !bytes.Equal(calls[0].data, o.Data) || calls[0].def != lz4Fast {
		t.Fatalf("callback saw %d calls: %+v", len(calls), calls)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.LZ4 {
		t.Fatalf("codec = %s, want lz4", c)
	}
}

func TestCompressionForChoosesPerObject(t *testing.T) {
	// The store's setting is lz4. The callback stores "raw…" objects raw,
	// sends "zstd…" objects to zstd 19 and accepts the setting for the rest.
	choose := func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
		switch {
		case bytes.HasPrefix(data, []byte("raw")):
			return amberpack.Compression{}
		case bytes.HasPrefix(data, []byte("zstd")):
			return zstd19
		}
		return def
	}
	s := rawStore(t, WithCompression(lz4Fast), WithCompressionFor(choose))
	for prefix, want := range map[string]amberpack.Algorithm{
		"raw":   amberpack.None,
		"zstd":  amberpack.Zstd,
		"other": amberpack.LZ4,
	} {
		o := blobObj(t, append([]byte(prefix), compressible(4096)...))
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if got := codecOf(t, s, o.Key); got != want {
			t.Errorf("%s object: codec = %s, want %s", prefix, got, want)
		}
		mustGet(t, s, o)
	}
}

func TestCompressionForIsAskedWhenTheSettingIsNone(t *testing.T) {
	var sawDef atomic.Value
	s := rawStore(t, WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			sawDef.Store(def)
			return zstdDefault
		}))
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if def, ok := sawDef.Load().(amberpack.Compression); !ok || def != (amberpack.Compression{}) {
		t.Fatalf("callback saw def = %v, want the zero Compression", sawDef.Load())
	}
	if c := codecOf(t, s, o.Key); c != amberpack.Zstd {
		t.Fatalf("codec = %s, want zstd", c)
	}
}

func TestCompressionForInvalidValueFailsTheWrite(t *testing.T) {
	var bad atomic.Bool
	bad.Store(true)
	s := rawStore(t, WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			if bad.Load() {
				return amberpack.Compression{Algorithm: amberpack.Zstd, Level: 99}
			}
			return def
		}))
	objs := distinct(t, 8)
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, amberpack.ErrInvalidCompression) {
			t.Fatalf("%s: err = %v, want ErrInvalidCompression", name, err)
		}
	}
	err := s.Put(objs[0].Key, objs[0].Data)
	check("Put", err)
	if !strings.Contains(err.Error(), objs[0].Key.String()) {
		t.Fatalf("Put error %q does not name the key", err)
	}
	check("PutVerified", s.PutVerified(objs[0].Key, objs[0].Data))
	check("WriteBatch", s.WriteBatch(objSeq(objs, -1)))
	_, err = s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4})
	check("WriteParallel", err)
	for _, o := range objs {
		if has, err := s.Has(o.Key); err != nil || has {
			t.Fatalf("Has(%s) = %v, %v after the rejected writes", o.Key, has, err)
		}
	}
	// The failure is that object's, not the store's: it keeps working.
	bad.Store(false)
	if _, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4}); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		mustGet(t, s, o)
	}
}

func TestCompressionForIsNotAskedForDedupHitsOrRecords(t *testing.T) {
	var calls atomic.Int64
	s := rawStore(t, WithCompression(zstdDefault), WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			calls.Add(1)
			return def
		}))
	o := blobObj(t, compressible(4096))
	for range 2 {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteBatch(objSeq([]Object{o, o}, -1)); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("callback ran %d times for one new object and three dedup hits", n)
	}
	// A pre-encoded record is appended as it is and keeps its codec.
	var recs []Object
	for i := range 4 {
		p := blobObj(t, append(compressible(4096), 0xEE, byte(i)))
		rec, err := amberpack.EncodeRecordWith(p.Key, p.Data, lz4Fast)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, Object{Key: p.Key, Record: rec})
	}
	if err := s.WriteBatch(objSeq(recs[:2], -1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteParallel(objSeq(recs[2:], -1), WriteOpts{Writers: 2}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("callback ran %d times; pre-encoded records must not reach it", n)
	}
	for _, r := range recs {
		if c := codecOf(t, s, r.Key); c != amberpack.LZ4 {
			t.Fatalf("pre-encoded record stored as %s, want lz4", c)
		}
	}
}

func TestPutDedupsAcrossCodecs(t *testing.T) {
	dir := t.TempDir()
	o := blobObj(t, compressible(4096))
	a, err := Open(dir, WithSync(false), WithCompression(lz4Fast))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	b, err := Open(dir, WithSync(false), WithCompression(zstdDefault), WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			calls.Add(1)
			return def
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("callback ran %d times for an object the store already holds", n)
	}
	if c := codecOf(t, b, o.Key); c != amberpack.LZ4 {
		t.Fatalf("codec = %s, want the lz4 record written first", c)
	}
}

func TestCompressionForUnderParallelWriters(t *testing.T) {
	var calls atomic.Int64
	s := rawStore(t, WithCompression(lz4Fast), WithCompressionFor(
		func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
			calls.Add(1)
			if data[len(data)-2]%2 == 0 { // distinct's low index byte
				return zstdDefault
			}
			return def
		}))
	objs := distinct(t, 200)
	stats, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 8})
	if err != nil || stats.Stored != len(objs) {
		t.Fatalf("WriteParallel: %+v, %v", stats, err)
	}
	if n := calls.Load(); n != int64(len(objs)) {
		t.Fatalf("callback ran %d times for %d objects", n, len(objs))
	}
	for i, o := range objs {
		want := amberpack.LZ4
		if i%2 == 0 {
			want = amberpack.Zstd
		}
		if got := codecOf(t, s, o.Key); got != want {
			t.Fatalf("object %d: codec = %s, want %s", i, got, want)
		}
	}
}

func TestOpenRejectsInvalidCompression(t *testing.T) {
	for _, c := range []amberpack.Compression{
		{Algorithm: amberpack.Zstd, Level: 23},
		{Algorithm: amberpack.LZ4, Level: -1},
		{Algorithm: amberpack.None, Level: 1},
		{Algorithm: amberpack.Algorithm(9)},
	} {
		dir := filepath.Join(t.TempDir(), "store")
		s, err := Open(dir, WithCompression(c))
		if !errors.Is(err, amberpack.ErrInvalidCompression) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("%+v: err = %v, want ErrInvalidCompression", c, err)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("%+v: Open created %s before rejecting the option", c, dir)
		}
	}
}

func TestCompressionOptionsLastOneWins(t *testing.T) {
	never := func(key.Key, []byte, amberpack.Compression) amberpack.Compression { return zstd19 }
	s := rawStore(t,
		WithCompression(zstdDefault), WithCompressionFor(never),
		WithCompression(lz4Fast), WithCompressionFor(nil))
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.LZ4 {
		t.Fatalf("codec = %s, want lz4: the later options replace the earlier ones", c)
	}
}

func TestTinyObjectsRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 2, 12, 13, 64}
	for _, c := range []amberpack.Compression{zstdDefault, lz4Fast, {Algorithm: amberpack.LZ4, Level: 9}} {
		var seen []int
		s := rawStore(t, WithCompression(c), WithCompressionFor(
			func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
				seen = append(seen, len(data))
				return def
			}))
		for _, n := range sizes {
			o := blobObj(t, bytes.Repeat([]byte{'a'}, n))
			if err := s.Put(o.Key, o.Data); err != nil {
				t.Fatalf("%s, %d bytes: %v", c, n, err)
			}
			mustGet(t, s, o)
		}
		if !slices.Equal(seen, sizes) {
			t.Fatalf("%s: callback saw sizes %v, want %v", c, seen, sizes)
		}
		// Verify re-parses every record: the length invariants hold.
		if err := s.Verify(context.Background()); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
}

func TestRepairReplacementUsesTheHandlesCompression(t *testing.T) {
	var useLZ4 atomic.Bool
	s := rawStore(t, WithCompressionFor(
		func(key.Key, []byte, amberpack.Compression) amberpack.Compression {
			if useLZ4.Load() {
				return lz4Fast
			}
			return zstdDefault
		}))
	o := blobObj(t, compressible(8192))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.Zstd {
		t.Fatalf("first record stored as %s, want zstd", c)
	}
	off := s.active.index[o.Key].off
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	damageRepairRecord(t, s.sealed[0].path, off+amberpack.RecHeaderSize)
	useLZ4.Store(true)
	if err := s.PutVerified(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.LZ4 {
		t.Fatalf("replacement stored as %s, want lz4", c)
	}
	mustGet(t, s, o)
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestCompressionForInvalidValueMidRun has the callback reject one object in
// the middle of a run: what was written before it stays, the rejected object
// is absent, and the store keeps working.
func TestCompressionForInvalidValueMidRun(t *testing.T) {
	const reject = 20
	var bad atomic.Bool
	choose := func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
		if bad.Load() && data[len(data)-2] == reject { // distinct's low index byte
			return amberpack.Compression{Algorithm: amberpack.LZ4, Level: 99}
		}
		return def
	}
	objs := distinct(t, 40)

	t.Run("WriteBatch", func(t *testing.T) {
		bad.Store(true)
		s := rawStore(t, WithCompression(zstdDefault), WithCompressionFor(choose))
		err := s.WriteBatch(objSeq(objs, -1))
		if !errors.Is(err, amberpack.ErrInvalidCompression) || !strings.Contains(err.Error(), objs[reject].Key.String()) {
			t.Fatalf("err = %v, want ErrInvalidCompression naming object %d", err, reject)
		}
		for i, o := range objs {
			has, err := s.Has(o.Key)
			if err != nil || has != (i < reject) {
				t.Fatalf("object %d: Has = %v, %v; a batch keeps exactly the objects before the rejected one", i, has, err)
			}
		}
		for _, o := range objs[:reject] {
			mustGet(t, s, o)
		}
	})

	t.Run("WriteParallel", func(t *testing.T) {
		bad.Store(true)
		s := rawStore(t, WithCompression(zstdDefault), WithCompressionFor(choose))
		stats, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4})
		if !errors.Is(err, amberpack.ErrInvalidCompression) {
			t.Fatalf("err = %v, want ErrInvalidCompression", err)
		}
		stored := 0
		for i, o := range objs {
			has, err := s.Has(o.Key)
			if err != nil {
				t.Fatal(err)
			}
			if i == reject && has {
				t.Fatal("the rejected object was stored")
			}
			if has {
				stored++
				mustGet(t, s, o)
			}
		}
		if stored != stats.Stored {
			t.Fatalf("the stats say %d objects stored, the store holds %d", stats.Stored, stored)
		}
		if err := s.Verify(context.Background()); err != nil {
			t.Fatal(err)
		}
		// The store is not poisoned: the same run goes through once the
		// callback behaves.
		bad.Store(false)
		if _, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4}); err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			mustGet(t, s, o)
		}
	})
}

// listDir returns "name size" for every entry of dir, in name order.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %d", e.Name(), info.Size()))
	}
	return out
}

// TestRejectedRepairLeavesTheStoreUntouched damages a sealed record and asks
// for a repair whose replacement the callback makes impossible to encode:
// the call fails before it changes anything on disk, the active segment
// stays active, and the repair goes through once the callback behaves.
func TestRejectedRepairLeavesTheStoreUntouched(t *testing.T) {
	var bad atomic.Bool
	s := rawStore(t, WithCompression(zstdDefault), WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			if bad.Load() {
				return amberpack.Compression{Algorithm: amberpack.Zstd, Level: 99}
			}
			return def
		}))
	o := blobObj(t, compressible(8192))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	off := s.active.index[o.Key].off
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	damageRepairRecord(t, s.sealed[0].path, off+amberpack.RecHeaderSize)
	later := blobObj(t, []byte("a later object, in a new active segment"))
	if err := s.Put(later.Key, later.Data); err != nil {
		t.Fatal(err)
	}

	before := listDir(t, s.dir)
	bad.Store(true)
	if err := s.PutVerified(o.Key, o.Data); !errors.Is(err, amberpack.ErrInvalidCompression) {
		t.Fatalf("PutVerified: err = %v, want ErrInvalidCompression", err)
	}
	if after := listDir(t, s.dir); !slices.Equal(after, before) {
		t.Fatalf("a rejected repair changed the store directory:\nbefore %v\nafter  %v", before, after)
	}

	bad.Store(false)
	if err := s.PutVerified(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	mustGet(t, s, o)
	mustGet(t, s, later)
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}
