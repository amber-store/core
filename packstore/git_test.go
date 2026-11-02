package packstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/gitobj"
	"github.com/amber-store/core/key"
)

func gitBlobObj(t *testing.T, content []byte) Object {
	t.Helper()
	data := gitobj.Encode(gitobj.Blob, content)
	k, err := gitobj.KeyOf(data)
	if err != nil {
		t.Fatal(err)
	}
	return Object{Key: k, Data: data}
}

// gitBlobObjs returns n distinct git blobs of a few hundred bytes.
func gitBlobObjs(t *testing.T, n int) []Object {
	t.Helper()
	objs := make([]Object, n)
	for i := range objs {
		objs[i] = gitBlobObj(t, append(incompressible(600), byte(i), byte(i>>8)))
	}
	return objs
}

// segmentGitRecords returns a segment's version byte and how many git records
// it holds.
func segmentGitRecords(t *testing.T, path string) (version byte, git int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 8 || string(b[:7]) != "AMBERSG" {
		t.Fatalf("%s: no segment header", path)
	}
	for off := 8; off < len(b) && b[off] == 0x01; {
		rec, err := amberpack.ParseRecord(b[off:])
		if err != nil {
			t.Fatalf("%s: record at offset %d: %v", path, off, err)
		}
		if rec.Key.Type() == key.GitSHA1 {
			git++
		}
		off += amberpack.RecHeaderSize + int(rec.Slen)
	}
	return b[7], git
}

// gitPerVersion walks every segment in dir and checks the gate's rule: a git
// record is in a version-4 segment and nowhere else. It returns, per version,
// the segments there are and the git records they hold.
func gitPerVersion(t *testing.T, dir string) map[byte][2]int {
	t.Helper()
	out := map[byte][2]int{}
	for _, suffix := range []string{sealedSuffix, activeSuffix} {
		paths, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			v, git := segmentGitRecords(t, p)
			if git > 0 && v != 4 {
				t.Fatalf("%s is a version-%d segment and holds %d git records", filepath.Base(p), v, git)
			}
			c := out[v]
			c[0]++
			c[1] += git
			out[v] = c
		}
	}
	return out
}

func TestFirstGitRecordMovesTheHandleToVersion4(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	plain := distinct(t, 2)
	git := gitBlobObjs(t, 2)
	put := func(o Object) {
		t.Helper()
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	put(plain[0])
	if got := gitPerVersion(t, dir); len(got) != 1 || got[2][0] != 1 {
		t.Fatalf("one plain record: %v", got)
	}

	put(git[0])
	got := gitPerVersion(t, dir)
	if got[2] != [2]int{1, 0} || got[4] != [2]int{1, 1} {
		t.Fatalf("the first git record: %v", got)
	}
	if sealed, _ := filepath.Glob(filepath.Join(dir, "*"+sealedSuffix)); len(sealed) != 1 {
		t.Fatalf("sealed segments = %v, want the version-2 one the handle left", sealed)
	}

	// A plain record goes on in the version-4 segment, and the handle's next
	// segment is at version 4 too: one early seal, not one per segment.
	put(plain[1])
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	put(git[1])
	got = gitPerVersion(t, dir)
	if got[2] != [2]int{1, 0} || got[4] != [2]int{2, 2} {
		t.Fatalf("the next segment: %v", got)
	}

	for _, o := range append(append([]Object{}, plain...), git...) {
		mustGet(t, s, o)
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// lz4 needs version 3 and a git record version 4: a version-4 segment holds
// both, and a version-3 one is left like a version-2 one.
func TestGitRecordLeavesAVersion3Segment(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithSync(false), WithCompression(lz4Fast))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	plain := blobObj(t, compressible(4096))
	git := gitBlobObj(t, compressible(4096))
	if err := s.Put(plain.Key, plain.Data); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(git.Key, git.Data); err != nil {
		t.Fatal(err)
	}
	got := gitPerVersion(t, dir)
	if got[3] != [2]int{1, 0} || got[4] != [2]int{1, 1} {
		t.Fatalf("segments: %v", got)
	}
	if c := codecOf(t, s, git.Key); c != amberpack.LZ4 {
		t.Fatalf("a git record stored as %s, want the handle's lz4", c)
	}
	// And lz4 records are at home in version 4.
	later := blobObj(t, append(compressible(4096), 1))
	if err := s.Put(later.Key, later.Data); err != nil {
		t.Fatal(err)
	}
	mustGet(t, s, plain)
	mustGet(t, s, git)
	mustGet(t, s, later)
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGitRecordsThroughEveryWritePath(t *testing.T) {
	for _, c := range []struct {
		name  string
		write func(t *testing.T, s *Store, objs []Object)
	}{
		{"Put", func(t *testing.T, s *Store, objs []Object) {
			putAll(t, s, objs)
		}},
		{"PutVerified", func(t *testing.T, s *Store, objs []Object) {
			for _, o := range objs {
				if err := s.PutVerified(o.Key, o.Data); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"WriteBatch", func(t *testing.T, s *Store, objs []Object) {
			if err := s.WriteBatch(objSeq(objs, -1)); err != nil {
				t.Fatal(err)
			}
		}},
		{"WriteBatch of encoded records", func(t *testing.T, s *Store, objs []Object) {
			recs := make([]Object, len(objs))
			for i, o := range objs {
				rec, err := amberpack.EncodeRecordWith(o.Key, o.Data, zstdDefault)
				if err != nil {
					t.Fatal(err)
				}
				recs[i] = Object{Key: o.Key, Record: rec}
			}
			if err := s.WriteBatch(objSeq(recs, -1)); err != nil {
				t.Fatal(err)
			}
		}},
		{"WriteParallel", func(t *testing.T, s *Store, objs []Object) {
			if _, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Verify: true, Writers: 4}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, WithSegmentSize(4<<10), WithSync(false))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			objs := gitBlobObjs(t, 40)
			c.write(t, s, objs)
			got := gitPerVersion(t, dir) // fails on a git record below version 4
			if got[4][1] != len(objs) {
				t.Fatalf("segments: %v, want %d git records", got, len(objs))
			}
			for _, o := range objs {
				mustGet(t, s, o)
			}
		})
	}
}

func TestCompactionKeepsGitRecordsInVersion4Segments(t *testing.T) {
	dir := t.TempDir()
	objs := gitBlobObjs(t, 40)
	w, err := Open(dir, WithSegmentSize(8<<10), WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, w, objs)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// The compacting handle has seen no git record: what it copies decides.
	s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := func(k key.Key) bool { return k[0]%2 == 0 }
	stats, err := s.Compact(live, CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied == 0 {
		t.Fatalf("the pass copied nothing: %+v", stats)
	}
	if got := gitPerVersion(t, dir); got[4][1] == 0 { // fails on a git record below version 4
		t.Fatalf("no git record survived the pass: %v", got)
	}
	for _, o := range objs {
		if live(o.Key) {
			mustGet(t, s, o)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Reopened without a sidecar the active segment is scanned; a release that does
// not know the type takes a git record for a torn tail there. This one does not.
func TestGitRecordsSurviveAScanOfTheActiveSegment(t *testing.T) {
	dir := t.TempDir()
	objs := gitBlobObjs(t, 10)
	s, err := Open(dir, WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	putAll(t, s, objs)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sidecars, err := filepath.Glob(filepath.Join(dir, "*"+sidecarSuffix))
	if err != nil || len(sidecars) == 0 {
		t.Fatalf("sidecars: %v, %v", sidecars, err)
	}
	for _, p := range sidecars {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(dir, WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, o := range objs {
		mustGet(t, r, o)
	}
	// Writing is what makes the scan truncate.
	extra := blobObj(t, []byte("after the scan"))
	if err := r.Put(extra.Key, extra.Data); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		mustGet(t, r, o)
	}
	mustGet(t, r, extra)
}

func TestRepairOfAGitRecordKeepsVersion4(t *testing.T) {
	s := rawStore(t, WithCompression(zstdDefault))
	objs := gitBlobObjs(t, 2)
	for _, o := range objs {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	off := s.active.index[objs[0].Key].off
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	path := s.sealed[0].path
	damageRepairRecord(t, path, off+amberpack.RecHeaderSize)
	if err := s.PutVerified(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if v, git := segmentGitRecords(t, path); v != 4 || git != 2 {
		t.Fatalf("after the repair: version %d, %d git records", v, git)
	}
	for _, o := range objs {
		mustGet(t, s, o)
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsAGitRecordBelowVersion4(t *testing.T) {
	// Built by hand: no writer of this release produces it.
	o := gitBlobObj(t, []byte("hello\n"))
	rec, err := amberpack.EncodeRecord(o.Key, o.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version byte
		corrupt bool
	}{{2, true}, {3, true}, {4, false}} {
		body := append(bytes.Clone(magicHeader), rec...)
		body[7] = tc.version
		entry := indexEntry{k: o.Key, off: uint64(len(magicHeader)), slen: uint32(len(rec) - amberpack.RecHeaderSize)}
		footer, err := buildFooter(int64(len(body)), []indexEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "0000000000000001.seg"), append(body, footer...), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("version %d: %v", tc.version, err)
		}
		mustGet(t, s, o)
		err = s.Verify(context.Background())
		if tc.corrupt != errors.Is(err, ErrCorrupt) || (!tc.corrupt && err != nil) {
			t.Fatalf("version %d: Verify = %v; a git record is corruption below version 4 only", tc.version, err)
		}
		s.Close()
	}
}

// A repair raises the segment for its replacement, as for lz4: a git record that
// somehow sits lower cannot get a segment it may not be in.
func TestRepairRaisesTheVersionForAGitRecord(t *testing.T) {
	o := gitBlobObj(t, incompressible(8192))
	rec, err := amberpack.EncodeRecord(o.Key, o.Data)
	if err != nil {
		t.Fatal(err)
	}
	body := append(bytes.Clone(magicHeader), rec...)
	body[7] = 3
	entry := indexEntry{k: o.Key, off: uint64(len(magicHeader)), slen: uint32(len(rec) - amberpack.RecHeaderSize)}
	footer, err := buildFooter(int64(len(body)), []indexEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "0000000000000001.seg")
	if err := os.WriteFile(path, append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	damageRepairRecord(t, path, int64(len(magicHeader))+amberpack.RecHeaderSize)
	if err := s.PutVerified(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if v, git := segmentGitRecords(t, path); v != 4 || git != 1 {
		t.Fatalf("after the repair: version %d, %d git records", v, git)
	}
	mustGet(t, s, o)
}

func TestVerifyObject_Git(t *testing.T) {
	good := gitBlobObj(t, []byte("hello\n"))
	if err := verifyObject(good); err != nil {
		t.Fatalf("a good object: %v", err)
	}
	if err := verifyObject(Object{Key: good.Key, Data: []byte("blob 6\x00hullo\n")}); !errors.Is(err, ErrVerify) {
		t.Errorf("another payload: %v, want ErrVerify", err)
	}
	unframed := []byte("hello\n")
	k, err := gitobj.KeyOf(unframed)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyObject(Object{Key: k, Data: unframed}); !errors.Is(err, ErrVerify) || !errors.Is(err, gitobj.ErrMalformed) {
		t.Errorf("no frame: %v, want ErrVerify and ErrMalformed", err)
	}
	junkCommit := gitobj.Encode(gitobj.Commit, []byte("junk"))
	if k, err = gitobj.KeyOf(junkCommit); err != nil {
		t.Fatal(err)
	}
	if err := verifyObject(Object{Key: k, Data: junkCommit}); !errors.Is(err, ErrVerify) || !errors.Is(err, gitobj.ErrMalformed) {
		t.Errorf("a commit that does not parse: %v, want ErrVerify and ErrMalformed", err)
	}
}

// See gitobj: the first 320 bytes of shattered-1.pdf, plain SHA-1 f92d74e3….
const shatteredPrefix = "" +
	"255044462d312e330a25e2e3cfd30a0a0a312030206f626a0a3c3c2f57696474" +
	"682032203020522f4865696768742033203020522f547970652034203020522f" +
	"537562747970652035203020522f46696c7465722036203020522f436f6c6f72" +
	"53706163652037203020522f4c656e6774682038203020522f42697473506572" +
	"436f6d706f6e656e7420383e3e0a73747265616d0affd8fffe00245348412d31" +
	"20697320646561642121212121852fec092339759c39b1a1c63c4c97e1fffe01" +
	"7346dc9166b67e118f029ab621b2560ff9ca67cca8c7f85ba84c79030c2b3de2" +
	"18f86db3a90901d5df45c14f26fedfb3dc38e96ac22fe7bd728f0e45bce046d2" +
	"3c570feb141398bb552ef5a0a82be331fea48037b8b5d71f0e332edf93ac3500" +
	"eb4ddc0decc1a864790c782c76215660dd309791d06bd0af3f98cda4bc4629b1"

func TestVerifyObject_GitCollisionIsRefused(t *testing.T) {
	data, err := hex.DecodeString(shatteredPrefix)
	if err != nil {
		t.Fatal(err)
	}
	var sum [key.GitSHA1Size]byte
	if _, err := hex.Decode(sum[:], []byte("f92d74e3874587aaf443d1db961d4e26dde13e9c")); err != nil {
		t.Fatal(err)
	}
	err = verifyObject(Object{Key: key.NewGitSHA1(sum), Data: data})
	if !errors.Is(err, ErrVerify) || !errors.Is(err, gitobj.ErrCollision) {
		t.Errorf("verifyObject = %v, want ErrVerify and ErrCollision", err)
	}
}

// A store's dedup check answers for the SHA-1, so what is verified on the way
// in is all that stands between a key and the wrong bytes.
func TestPutVerifiedRefusesTheWrongPayloadUnderAGitKey(t *testing.T) {
	s := rawStore(t)
	o := gitBlobObj(t, []byte("hello\n"))
	if err := s.PutVerified(o.Key, []byte("blob 6\x00hullo\n")); !errors.Is(err, ErrVerify) {
		t.Fatalf("PutVerified = %v, want ErrVerify", err)
	}
	if ok, err := s.Has(o.Key); err != nil || ok {
		t.Fatalf("Has after a refused write: %v, %v", ok, err)
	}
}

func TestCompressionCallbackIsAskedForGitObjects(t *testing.T) {
	var asked atomic.Int32
	s := rawStore(t, WithCompressionFor(func(k key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
		if k.Type() == key.GitSHA1 {
			asked.Add(1)
		}
		return def
	}))
	o := gitBlobObj(t, []byte("hello\n"))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 1 {
		t.Fatalf("asked %d times", asked.Load())
	}
}

func TestMarkSet_MarkWalked(t *testing.T) {
	s, objs := compactStore(t) // sealed segments and an active one
	git := gitBlobObj(t, []byte("in the active segment"))
	if err := s.Put(git.Key, git.Data); err != nil {
		t.Fatal(err)
	}
	all := append(append([]Object{}, objs...), git)
	m := s.NewMarkSet()
	for i, o := range all {
		if m.Contains(o.Key) {
			t.Fatalf("object %d marked before Mark", i)
		}
		// Marking does not walk: that is the point of a second bit.
		if newly, present := m.Mark(o.Key); !newly || !present {
			t.Fatalf("Mark of object %d: newly=%v present=%v", i, newly, present)
		}
		if !m.MarkWalked(o.Key) {
			t.Fatalf("first MarkWalked of object %d after Mark said it was walked", i)
		}
		if m.MarkWalked(o.Key) {
			t.Fatalf("second MarkWalked of object %d said it was not", i)
		}
		if newly, _ := m.Mark(o.Key); newly {
			t.Fatalf("Mark of object %d after MarkWalked: newly", i)
		}
	}
	if m.Marked() != len(all) {
		t.Fatalf("Marked() = %d, want %d: walking is no second mark", m.Marked(), len(all))
	}
	absent := blobObj(t, []byte("never stored"))
	if m.MarkWalked(absent.Key) {
		t.Fatal("MarkWalked of an absent key said it was first")
	}
}
