package packstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// liveOf is a live function over some objects.
func liveOf(objs ...Object) func(key.Key) bool {
	m := map[key.Key]bool{}
	for _, o := range objs {
		m[o.Key] = true
	}
	return func(k key.Key) bool { return m[k] }
}

// threeVictims writes three segments, each one live and one dead record, and
// damages the live record of the first.
func threeVictims(t *testing.T) (dir string, live, dead []Object) {
	t.Helper()
	dir = t.TempDir()
	objs := distinct(t, 6)
	for i := range 3 {
		payload := sealedFile(t, dir, uint64(i+1), objs[i], objs[3+i])
		if i == 0 {
			flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[objs[0].Key])
		}
	}
	return dir, objs[:3], objs[3:]
}

// #29. One live record that fails verification keeps its segment, and the rest of
// the pass goes on: the other victims are copied and removed.
func TestCompactLeavesABadVictimAndReapsTheRest(t *testing.T) {
	dir, live, dead := threeVictims(t)
	s := openStore(t, dir, WithSync(false))
	size := func(id uint64) uint64 {
		st, err := os.Stat(filepath.Join(dir, segmentName(id, sealedSuffix)))
		if err != nil {
			t.Fatal(err)
		}
		return uint64(st.Size())
	}
	freed := size(2) + size(3)

	stats, err := s.Compact(liveOf(live...), CompactOpts{MarkVerified: true})

	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact = %v, want ErrCorrupt", err)
	}
	var skipped *SkippedError
	if !errors.As(err, &skipped) {
		t.Fatalf("Compact = %v, want a *SkippedError", err)
	}
	if len(stats.Skipped) != 1 || len(skipped.Skipped) != 1 {
		t.Fatalf("Skipped: stats %+v, error %+v", stats.Skipped, skipped.Skipped)
	}
	got := stats.Skipped[0]
	if got.ID != 1 || got.Key != live[0].Key || got.Offset != uint64(len(magicHeader)) || !errors.Is(got.Err, ErrCorrupt) {
		t.Errorf("Skipped[0] = %+v", got)
	}
	if !strings.HasSuffix(got.Path, segmentName(1, sealedSuffix)) {
		t.Errorf("Skipped[0].Path = %q", got.Path)
	}

	// What the stats say is what happened.
	if !slices.Equal(stats.Victims, []uint64{2, 3}) || stats.SegmentsCompacted != 2 || stats.BytesFreed != freed {
		t.Errorf("stats = %+v, want victims [2 3], %d bytes freed", stats, freed)
	}
	if !segExists(t, dir, 1) || segExists(t, dir, 2) || segExists(t, dir, 3) {
		t.Errorf("on disk: 1 %v, 2 %v, 3 %v; want only 1", segExists(t, dir, 1), segExists(t, dir, 2), segExists(t, dir, 3))
	}
	for _, o := range live[1:] {
		mustGet(t, s, o)
	}
	for _, o := range dead[1:] {
		if ok, err := s.Has(o.Key); err != nil || ok {
			t.Errorf("a dead object of a removed victim: has %v, %v", ok, err)
		}
	}
	// The kept segment keeps what it held, its dead record too.
	if ok, err := s.Has(dead[0].Key); err != nil || !ok {
		t.Errorf("a record of the kept segment: has %v, %v", ok, err)
	}
}

// It has to say what to do, and name the object: a CRC failure says nothing else
// about which one it was.
func TestCompactSkipErrorNamesTheKeyAndWhatToDo(t *testing.T) {
	dir, live, _ := threeVictims(t)
	s := openStore(t, dir, WithSync(false))
	_, err := s.Compact(liveOf(live...), CompactOpts{MarkVerified: true})
	if err == nil {
		t.Fatal("Compact passed a bad record")
	}
	for _, want := range []string{"left 1 segment in place", live[0].Key.String(), segmentName(1, sealedSuffix), "PutVerified", "record CRC mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
}

// The trap in carrying on: a key with a bad copy in one victim and a good copy in
// another. The second victim may be removed only with the good copy kept, whichever
// the pass meets first.
func TestCompactKeepsTheGoodCopyOfAKeyWhoseOtherCopyIsBad(t *testing.T) {
	for _, badIn := range []uint64{1, 2} {
		dir := t.TempDir()
		objs := distinct(t, 3)
		k := objs[0]
		payload := sealedFile(t, dir, 1, k, objs[1])
		sealedFile(t, dir, 2, k, objs[2])
		flipByte(t, filepath.Join(dir, segmentName(badIn, sealedSuffix)), payload[k.Key])

		s := openStore(t, dir)
		// Selection asks once per segment, then the walk once more each. Before the
		// walk asks about the second copy, give the verifiers time to have found
		// the first one bad: that is when a pass could wrongly take the key as dealt
		// with.
		var asked atomic.Int32
		live := func(q key.Key) bool {
			if q != k.Key {
				return false
			}
			if asked.Add(1) == 4 {
				time.Sleep(200 * time.Millisecond)
			}
			return true
		}
		if _, err := s.Compact(live, CompactOpts{MarkVerified: true}); err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("bad copy in %d: Compact = %v", badIn, err)
		}
		good := 3 - badIn
		if segExists(t, dir, good) {
			t.Errorf("bad copy in %d: the victim with the good copy is still there", badIn)
		}
		got, err := s.Get(k.Key)
		if err != nil || !bytes.Equal(got, k.Data) {
			t.Fatalf("bad copy in %d: Get = %d bytes, %v; the good copy is lost", badIn, len(got), err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// And after a restart.
		r := openStore(t, dir)
		got, err = r.Get(k.Key)
		if err != nil || !bytes.Equal(got, k.Data) {
			t.Fatalf("bad copy in %d, reopened: Get = %d bytes, %v", badIn, len(got), err)
		}
	}
}

// A kept segment is selected again on every pass. What it already gave up stays
// where it went, so the store does not grow by a copy a cycle.
func TestCompactDoesNotGrowAroundABadVictim(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 5)
	payload := sealedFile(t, dir, 1, objs...)
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[objs[0].Key])
	s := openStore(t, dir, WithSync(false))
	live := liveOf(objs[:4]...) // objs[4] is dead, so the segment is a victim
	copied := 0
	for pass := range 5 {
		stats, err := s.Compact(live, CompactOpts{MarkVerified: true})
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("pass %d: Compact = %v", pass, err)
		}
		copied += stats.RecordsCopied
		if len(stats.Victims) != 0 || !segExists(t, dir, 1) {
			t.Fatalf("pass %d: victims %v, kept segment there: %v", pass, stats.Victims, segExists(t, dir, 1))
		}
	}
	if copied > 3 { // the three good live records, each at most once
		t.Fatalf("%d records copied over 5 passes, want at most 3", copied)
	}
}

// What clears it: a repair, or the reference going.
func TestCompactReapsARepairedVictim(t *testing.T) {
	dir, live, _ := threeVictims(t)
	s := openStore(t, dir, WithSync(false))
	if _, err := s.Compact(liveOf(live...), CompactOpts{MarkVerified: true}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact = %v", err)
	}
	if err := s.PutVerified(live[0].Key, live[0].Data); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Compact(liveOf(live...), CompactOpts{MarkVerified: true})
	if err != nil || len(stats.Skipped) != 0 {
		t.Fatalf("after the repair: %v, skipped %+v", err, stats.Skipped)
	}
	if !slices.Contains(stats.Victims, 1) || segExists(t, dir, 1) {
		t.Errorf("after the repair: victims %v, segment there: %v", stats.Victims, segExists(t, dir, 1))
	}
	mustGet(t, s, live[0])
}

func TestCompactReapsAVictimOnceItsBadRecordIsDead(t *testing.T) {
	dir, live, _ := threeVictims(t)
	s := openStore(t, dir, WithSync(false))
	if _, err := s.Compact(liveOf(live...), CompactOpts{MarkVerified: true}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact = %v", err)
	}
	stats, err := s.Compact(liveOf(live[1:]...), CompactOpts{MarkVerified: true})
	if err != nil || len(stats.Skipped) != 0 {
		t.Fatalf("with the record dead: %v, skipped %+v", err, stats.Skipped)
	}
	if segExists(t, dir, 1) {
		t.Error("the segment is still there")
	}
}

func TestCompactEveryVictimBad(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 4)
	for i := range 2 {
		payload := sealedFile(t, dir, uint64(i+1), objs[i], objs[2+i])
		flipByte(t, filepath.Join(dir, segmentName(uint64(i+1), sealedSuffix)), payload[objs[i].Key])
	}
	s := openStore(t, dir, WithSync(false))
	stats, err := s.Compact(liveOf(objs[:2]...), CompactOpts{MarkVerified: true})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact = %v", err)
	}
	var ids []uint64
	for _, sk := range stats.Skipped {
		ids = append(ids, sk.ID)
	}
	if !slices.Equal(ids, []uint64{1, 2}) {
		t.Errorf("skipped %v, want [1 2] ascending", ids)
	}
	if len(stats.Victims) != 0 || stats.BytesFreed != 0 || !segExists(t, dir, 1) || !segExists(t, dir, 2) {
		t.Errorf("stats %+v, on disk 1 %v, 2 %v", stats, segExists(t, dir, 1), segExists(t, dir, 2))
	}
}

// A bad index entry is the same kind of trouble: that segment stays.
func TestCompactLeavesAVictimWithABadIndexEntry(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 4)
	good := objs[0]
	rec, err := amberpack.EncodeRecord(good.Key, good.Data)
	if err != nil {
		t.Fatal(err)
	}
	body := append(headerAt(versionBase), rec...)
	entries := []indexEntry{
		{k: good.Key, off: uint64(len(magicHeader)), slen: uint32(len(good.Data)) + 1<<20}, // past the body
		{k: objs[1].Key, off: uint64(len(magicHeader)), slen: 0},
	}
	footer, err := buildFooter(int64(len(body)), entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, segmentName(1, sealedSuffix)), append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}
	sealedFile(t, dir, 2, objs[2], objs[3])
	s := openStore(t, dir, WithSync(false))
	stats, err := s.Compact(liveOf(good, objs[2]), CompactOpts{MarkVerified: true})
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "index entry out of bounds") {
		t.Fatalf("Compact = %v", err)
	}
	if len(stats.Skipped) != 1 || stats.Skipped[0].ID != 1 || stats.Skipped[0].Key != good.Key {
		t.Errorf("skipped %+v", stats.Skipped)
	}
	if !slices.Equal(stats.Victims, []uint64{2}) || !segExists(t, dir, 1) || segExists(t, dir, 2) {
		t.Errorf("victims %v, on disk 1 %v, 2 %v", stats.Victims, segExists(t, dir, 1), segExists(t, dir, 2))
	}
	mustGet(t, s, objs[2])
}

// An entry pointing into the header is out of bounds too, and is told apart from a
// record that fails its checks.
func TestCompactNamesAnIndexEntryInTheHeader(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 2)
	rec, err := amberpack.EncodeRecord(objs[0].Key, objs[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	body := append(headerAt(versionBase), rec...)
	footer, err := buildFooter(int64(len(body)), []indexEntry{
		{k: objs[0].Key, off: 0, slen: uint32(len(objs[0].Data))},
		{k: objs[1].Key, off: uint64(len(magicHeader)), slen: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, segmentName(1, sealedSuffix)), append(body, footer...), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir, WithSync(false))
	_, err = s.Compact(liveOf(objs[0]), CompactOpts{MarkVerified: true})
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "index entry out of bounds") {
		t.Fatalf("Compact = %v", err)
	}
}

// Carrying on is for bad records. A store that cannot write is another matter: the
// copies are not all there, so nothing may be removed.
func TestCompactAppendFailureRemovesNothing(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 6)
	sealedFile(t, dir, 1, objs[0], objs[1], objs[4])
	sealedFile(t, dir, 2, objs[2], objs[3], objs[5])
	s := openStore(t, dir, WithSync(false))
	boom := errors.New("boom")
	stats, err := s.Compact(liveOf(objs[:4]...), CompactOpts{Pace: func(int) { s.setFailed(boom) }})
	if !errors.Is(err, boom) {
		t.Fatalf("Compact = %v, want the write failure", err)
	}
	var skipped *SkippedError
	if errors.As(err, &skipped) {
		t.Errorf("a write failure reported as a skip: %v", err)
	}
	if len(stats.Victims) != 0 || stats.SegmentsCompacted != 0 || stats.BytesFreed != 0 {
		t.Errorf("stats say something was removed: %+v", stats)
	}
	if !segExists(t, dir, 1) || !segExists(t, dir, 2) {
		t.Error("a victim was removed after a failed copy")
	}
}

// A victim with nothing live needs no reads, and is not held up by a bad neighbour.
func TestCompactReapsAFullyDeadVictimNextToABadOne(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 4)
	payload := sealedFile(t, dir, 1, objs[0], objs[1])
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[objs[0].Key])
	sealedFile(t, dir, 2, objs[2], objs[3])
	s := openStore(t, dir, WithSync(false))
	stats, err := s.Compact(liveOf(objs[0]), CompactOpts{MarkVerified: true})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Compact = %v", err)
	}
	if !slices.Equal(stats.Victims, []uint64{2}) || segExists(t, dir, 2) {
		t.Errorf("victims %v, segment 2 there: %v", stats.Victims, segExists(t, dir, 2))
	}
}

// Going on past a bad record is safe only if the mark verified what it read. Any other
// walk may have read a bad directory as a smaller one, and the record that shows it up
// may be another one beside it: a pass that gave up on the pack would never look at the
// directory. So without a verified mark every bad record stops the pass, of whatever
// type, with nothing removed.
func TestCompactWithoutAVerifiedMarkStopsOnAnyBadRecord(t *testing.T) {
	typed := func(typ key.Type, data []byte) Object {
		k, err := key.New(typ, uint64(len(data)), data)
		if err != nil {
			t.Fatal(err)
		}
		return Object{Key: k, Data: data}
	}
	body := bytes.Repeat([]byte("x"), 64)
	for name, bad := range map[string]Object{
		"Blob":     typed(key.Blob, body),
		"XattrSet": typed(key.XattrSet, body),
		"DirLeaf":  typed(key.DirLeaf, body),
		"DirNode":  typed(key.DirNode, body),
		"FileNode": typed(key.FileNode, body),
		"Commit":   typed(key.Commit, body),
		"git":      gitBlobObj(t, body),
	} {
		t.Run(name, func(t *testing.T) {
			objs := distinct(t, 3)
			setup := func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				payload := sealedFile(t, dir, 1, bad, objs[0])
				flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[bad.Key])
				sealedFile(t, dir, 2, objs[1], objs[2])
				return dir
			}
			live := liveOf(bad, objs[1])

			dir := setup(t)
			s := openStore(t, dir, WithSync(false))
			stats, err := s.Compact(live, CompactOpts{})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Compact = %v, want ErrCorrupt", err)
			}
			var skipped *SkippedError
			if errors.As(err, &skipped) {
				t.Errorf("reported as a skip: %v", err)
			}
			if len(stats.Victims) != 0 || len(stats.Skipped) != 0 || !segExists(t, dir, 1) || !segExists(t, dir, 2) {
				t.Errorf("something was removed: stats %+v", stats)
			}

			// A caller whose mark verified what it read knows nothing was hidden.
			dir = setup(t)
			s = openStore(t, dir, WithSync(false))
			stats, err = s.Compact(live, CompactOpts{MarkVerified: true})
			if !errors.As(err, &skipped) {
				t.Fatalf("with a verified mark: %v, want a skip", err)
			}
			if !slices.Equal(stats.Victims, []uint64{2}) || !segExists(t, dir, 1) || segExists(t, dir, 2) {
				t.Errorf("with a verified mark: victims %v", stats.Victims)
			}
		})
	}
}

// Two bad records in one pack, as a damaged region of a disk leaves: one report, and
// nothing else in the pack goes anywhere it should not.
func TestCompactTwoBadRecordsInOnePack(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 5)
	payload := sealedFile(t, dir, 1, objs[0], objs[1], objs[2], objs[4])
	p := filepath.Join(dir, segmentName(1, sealedSuffix))
	flipByte(t, p, payload[objs[0].Key])
	flipByte(t, p, payload[objs[1].Key])
	sealedFile(t, dir, 2, objs[3], objs[4])
	s := openStore(t, dir, WithSync(false))
	stats, err := s.Compact(liveOf(objs[:4]...), CompactOpts{MarkVerified: true})
	var skipped *SkippedError
	if !errors.As(err, &skipped) {
		t.Fatalf("Compact = %v", err)
	}
	if len(stats.Skipped) != 1 || stats.Skipped[0].ID != 1 {
		t.Fatalf("skipped %+v, want the pack once", stats.Skipped)
	}
	if k := stats.Skipped[0].Key; k != objs[0].Key && k != objs[1].Key {
		t.Errorf("the report names %s, which is a good record", k)
	}
	if !segExists(t, dir, 1) || segExists(t, dir, 2) {
		t.Errorf("on disk: 1 %v, 2 %v", segExists(t, dir, 1), segExists(t, dir, 2))
	}
	mustGet(t, s, objs[2])
	mustGet(t, s, objs[3])
}

func TestGetVerified(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 2)
	payload := sealedFile(t, dir, 1, objs...)
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[objs[0].Key])
	s := openStore(t, dir, WithSync(false))
	if got, err := s.GetVerified(objs[1].Key); err != nil || !bytes.Equal(got, objs[1].Data) {
		t.Errorf("a good record: %d bytes, %v", len(got), err)
	}
	if _, err := s.GetVerified(objs[0].Key); !errors.Is(err, ErrVerify) {
		t.Errorf("a bad record: %v, want ErrVerify", err)
	}
	if _, err := s.GetVerified(blobObj(t, []byte("absent")).Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("an absent record: %v, want ErrNotFound", err)
	}
}

// A copy in a pack that is not a victim stands in for the victim's only if it is
// good. Otherwise the pass removes the only good copy, with no error at all.
func TestABadSurvivorDoesNotStandInForTheGoodCopy(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 3)
	k, other, dead := objs[0], objs[1], objs[2]
	payload := sealedFile(t, dir, 1, k, other) // nothing dead: never a victim
	sealedFile(t, dir, 2, k, dead)             // the good copy, and garbage
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[k.Key])
	s := openStore(t, dir)
	stats, err := s.Compact(liveOf(k, other), CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied != 1 {
		t.Errorf("copied %d records, want the good one", stats.RecordsCopied)
	}
	if got, err := s.Get(k.Key); err != nil || !bytes.Equal(got, k.Data) {
		t.Fatalf("the only good copy of a live key was lost: Get = %d bytes, %v", len(got), err)
	}
	// And a good one saves the copy.
	dir = t.TempDir()
	sealedFile(t, dir, 1, k, other)
	sealedFile(t, dir, 2, k, dead)
	s = openStore(t, dir)
	stats, err = s.Compact(liveOf(k, other), CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied != 0 {
		t.Errorf("copied %d records past a good survivor", stats.RecordsCopied)
	}
}

// The same for another writer's active segment.
func TestABadCopyInAForeignSegmentDoesNotStandIn(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 2)
	k, dead := objs[0], objs[1]
	sealedFile(t, dir, 1, k, dead)

	w := openStore(t, dir) // another store, alive, its segment synced
	rec, err := amberpack.EncodeRecord(k.Key, k.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.append(k.Key, rec, true); err != nil {
		t.Fatal(err)
	}
	flipByte(t, w.active.path, int64(len(magicHeader))+amberpack.RecHeaderSize)

	s := openStore(t, dir)
	stats, err := s.Compact(liveOf(k), CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied != 1 || !slices.Equal(stats.Victims, []uint64{1}) {
		t.Fatalf("stats %+v, want the good copy copied and its pack gone", stats)
	}
}

// A pack left in place is charged its whole live size though it copies next to
// nothing, so under a budget it could starve the packs behind it for good.
func TestABadPackDoesNotEatTheCopyBudget(t *testing.T) {
	dir, live, _ := threeVictims(t)
	s := openStore(t, dir, WithSync(false))
	budget := uint64(amberpack.RecHeaderSize + len(live[0].Data)) // one pack's live bytes
	for range 6 {
		s.Compact(liveOf(live...), CompactOpts{MaxCopyBytes: &budget, MarkVerified: true})
	}
	if segExists(t, dir, 2) || segExists(t, dir, 3) {
		t.Errorf("after 6 passes: pack 2 there %v, pack 3 there %v", segExists(t, dir, 2), segExists(t, dir, 3))
	}
	if !segExists(t, dir, 1) {
		t.Error("the bad pack went")
	}
}

// It reports when it is a victim: what the line and the budget leave out, it cannot.
func TestABadPackReportsWhenItIsAVictim(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 3)
	payload := sealedFile(t, dir, 1, objs...)
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[objs[0].Key])
	s := openStore(t, dir, WithSync(false))
	live := liveOf(objs[0], objs[1]) // a third dead
	if _, err := s.Compact(live, CompactOpts{MinDeadRatio: 0, MarkVerified: true}); err == nil {
		t.Fatal("no report at a line it is above")
	}
	if _, err := s.Compact(live, CompactOpts{MinDeadRatio: 0.9, MarkVerified: true}); err != nil {
		t.Fatalf("a report at a line it is below: %v", err)
	}
}

// The core promise: survivors are durable before anything is unlinked.
func TestCopiesAreDurableBeforeAnyVictimGoes(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 4)
	payload := sealedFile(t, dir, 1, objs[0], objs[1])
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[objs[0].Key])
	sealedFile(t, dir, 2, objs[2], objs[3])
	s := openStore(t, dir) // syncs
	checked := false
	var copiesSynced, dirSyncedAfter bool
	s.beforeFsync = func(f *os.File) error {
		switch {
		case !checked && s.active != nil && f == s.active.f && s.active.size > int64(len(magicHeader)):
			copiesSynced = true
		case checked && f == s.dirF:
			dirSyncedAfter = true
		}
		return nil
	}
	s.afterDetach = func() {
		checked = true
		if got, want := durableMark(t, s), s.active.size; got < want {
			t.Errorf("victims leaving with %d of %d bytes known durable", got, want)
		}
		if !copiesSynced {
			t.Error("victims leaving before the fsync of what was copied out of them")
		}
	}
	if _, err := s.Compact(liveOf(objs[0], objs[2]), CompactOpts{MarkVerified: true}); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("nothing was removed")
	}
	if !dirSyncedAfter {
		t.Error("no fsync of the directory after the unlink")
	}
}

// The stats say what was removed, also when an unlink fails.
func TestCompactStatsAfterAFailedRemove(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 4)
	sealedFile(t, dir, 1, objs[0], objs[1])
	sealedFile(t, dir, 2, objs[2], objs[3])
	s := openStore(t, dir, WithSync(false))
	s.afterDetach = func() { os.Remove(filepath.Join(dir, segmentName(1, sealedSuffix))) }
	stats, err := s.Compact(liveOf(objs[0], objs[2]), CompactOpts{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Compact = %v", err)
	}
	if stats.SegmentsCompacted != 1 || !slices.Equal(stats.Victims, []uint64{2}) {
		t.Fatalf("stats %+v", stats)
	}
}

// And what was copied: a key in two victims is appended once.
func TestCompactCountsWhatItAppended(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 66)
	sealedFile(t, dir, 1, append(objs[:64:64], objs[64])...)
	sealedFile(t, dir, 2, append(objs[:64:64], objs[65])...)
	s := openStore(t, dir, WithSync(false), WithSegmentSize(1<<30))
	stats, err := s.Compact(liveOf(objs[:64]...), CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied != 64 {
		t.Fatalf("copied %d records, appended 64", stats.RecordsCopied)
	}
}

// A crashed seal somebody holds may not be fsynced yet, so it does not stand in for a
// victim's copy either.
func TestACrashedSealDoesNotStandIn(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 3)
	k, other, dead := objs[0], objs[1], objs[2]
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, o := range []Object{k, other} {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	sealedFile(t, dir, 2, k, dead) // w holds the file of id 1: the good copy is this
	s := openStore(t, dir)
	stats, err := s.Compact(liveOf(k, other), CompactOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RecordsCopied != 1 || !slices.Equal(stats.Victims, []uint64{2}) {
		t.Fatalf("stats %+v, want the good copy copied and its pack gone", stats)
	}
}

// orderedVictim makes a big blob that sorts first in a pack's index, small blobs, and a
// small target that sorts last: with one verifier the copy follows that order, so that
// the big one rotates the segment before the target's turn.
func orderedVictim(t *testing.T, big, n int) (all []Object, target, dead Object) {
	t.Helper()
	objs := make([]Object, n)
	last, minK := 0, key.Key{}
	for i := range objs {
		objs[i] = blobObj(t, []byte(fmt.Sprintf("small-%04d-%s", i, bytes.Repeat([]byte("x"), 80))))
		if bytes.Compare(objs[i].Key[:], objs[last].Key[:]) > 0 {
			last = i
		}
		if i == 0 || bytes.Compare(objs[i].Key[:], minK[:]) < 0 {
			minK = objs[i].Key
		}
	}
	base := incompressible(big)
	for i := 0; ; i++ {
		base[0], base[1], base[2] = byte(i), byte(i>>8), byte(i>>16)
		if o := blobObj(t, bytes.Clone(base)); bytes.Compare(o.Key[:], minK[:]) < 0 {
			return append(objs, o), objs[last], blobObj(t, []byte("dead"))
		}
	}
}

func oneVerifier(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
}

// What a pass's own segment holds stands in for a victim's copy, on the grounds that the
// pass verified it. A segment somebody left while the pass ran holds what nobody verified:
// taking it at a rotation would let a rotted copy stand in, and the good pack go.
func TestAPassDoesNotAdoptASegmentWhileItCopies(t *testing.T) {
	const segSize = 64 << 10
	setup := func(t *testing.T) (dir string, w *Store, all []Object, target, dead Object) {
		oneVerifier(t)
		dir = t.TempDir()
		all, target, dead = orderedVictim(t, segSize, 60)
		sealedFile(t, dir, 1, append(all[:len(all):len(all)], dead)...)
		w = openStore(t, dir, WithSegmentSize(segSize))
		rec, err := amberpack.EncodeRecord(target.Key, target.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.append(target.Key, rec, true); err != nil {
			t.Fatal(err)
		}
		flipByte(t, w.active.path, int64(len(magicHeader))+amberpack.RecHeaderSize) // rot
		return dir, w, all, target, dead
	}
	check := func(t *testing.T, s *Store, dir string, all []Object, stats CompactStats, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Compact = %v", err)
		}
		if segExists(t, dir, 1) {
			t.Errorf("the victim is still there: stats %+v", stats)
		}
		// Every copy the store now holds of a live key, the rotted one aside, is good, and
		// there is one: read each segment, since a lookup stops at the first.
		for _, o := range all {
			if !someCopyVerifies(t, s, o) {
				t.Fatalf("no good copy left of %s", o.Key)
			}
		}
	}
	t.Run("a writer exits during the pass", func(t *testing.T) {
		dir, w, all, _, _ := setup(t)
		s := openStore(t, dir, WithSegmentSize(segSize))
		once := false
		stats, err := s.Compact(liveOf(all...), CompactOpts{Pace: func(int) {
			if !once {
				once = true
				w.Close() // the big copy rotated: the next append would adopt w's segment
			}
		}})
		check(t, s, dir, all, stats, err)
	})
	t.Run("a torn segment keeps the idle sealing from reaching it", func(t *testing.T) {
		dir, w, all, _, _ := setup(t)
		w.Close()
		// A writer that died on its first record: nothing valid, but the largest file.
		torn := append(headerAt(versionBase), bytes.Repeat([]byte{0x7a}, 8<<10)...)
		if err := os.WriteFile(filepath.Join(dir, segmentName(99, activeSuffix)), torn, 0o644); err != nil {
			t.Fatal(err)
		}
		s := openStore(t, dir, WithSegmentSize(segSize))
		stats, err := s.Compact(liveOf(all...), CompactOpts{})
		check(t, s, dir, all, stats, err)
	})
}

// copiesOf reads every copy of o that the view holds, a lookup stopping at the first,
// and reports how many are good and how many are not.
func copiesOf(t *testing.T, s *Store, o Object) (good, bad int) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	note := func(raw []byte, err error) {
		if err == nil && validRepairRecord(o.Key, raw) {
			good++
		} else {
			bad++
		}
	}
	if s.active != nil {
		if loc, ok := s.active.index[o.Key]; ok {
			raw := make([]byte, amberpack.RecHeaderSize+int(loc.slen))
			_, err := s.active.f.ReadAt(raw, loc.off)
			note(raw, err)
		}
	}
	for _, fa := range s.foreign {
		if loc, ok := fa.scan.index[o.Key]; ok {
			note(fa.readRecord(o.Key, loc))
		}
	}
	for _, g := range s.sealed {
		if off, slen, ok := g.fv.lookup(o.Key); ok {
			note(repairRecord(g, indexEntry{k: o.Key, off: off, slen: slen}))
		}
	}
	return good, bad
}

func someCopyVerifies(t *testing.T, s *Store, o Object) bool {
	t.Helper()
	good, _ := copiesOf(t, s, o)
	return good > 0
}

func badCopies(t *testing.T, s *Store, o Object) int {
	t.Helper()
	_, bad := copiesOf(t, s, o)
	return bad
}

// A crashed seal somebody holds is a victim like any other. When the holder exits during
// the pass, adopting the file would finish the seal and unmap what the pass is reading.
func TestAPassDoesNotFinishASealItIsReading(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, WithSegmentSize(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	var objs []Object
	for i := range 200 {
		objs = append(objs, blobObj(t, append([]byte(fmt.Sprintf("%06d", i)), incompressible(4<<10)...)))
	}
	dead := blobObj(t, []byte("dead"))
	for _, o := range append(objs[:200:200], dead) {
		if err := w.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	w.afterFooter = func() error { return errors.New("EIO") } // footer written, store poisoned, flock held
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	s := openStore(t, dir, WithSegmentSize(32<<10))
	once := false
	if _, err := s.Compact(liveOf(objs...), CompactOpts{Pace: func(int) {
		if !once {
			once = true
			w.Close()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		mustGet(t, s, o)
	}
}

// The repair the error names has to repair, wherever the bad copy is.
func TestPutVerifiedReachesWhatLookupsRead(t *testing.T) {
	badCopy := func(t *testing.T, dir string, o Object) string {
		t.Helper()
		w, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		// Next to a good record: repairing it must not lose its neighbours.
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.append(o.Key, rec, true); err != nil {
			t.Fatal(err)
		}
		p := w.active.path
		other := blobObj(t, []byte("a neighbour"))
		if err := w.Put(other.Key, other.Data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		flipByte(t, p, int64(len(magicHeader))+amberpack.RecHeaderSize+1)
		return p
	}
	verified := func(t *testing.T, s *Store, o Object) {
		t.Helper()
		if got, err := s.GetVerified(o.Key); err != nil || !bytes.Equal(got, o.Data) {
			t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
		}
		if bad := badCopies(t, s, o); bad != 0 {
			t.Fatalf("%d bad copies left", bad)
		}
		if got, err := s.Get(blobObj(t, []byte("a neighbour")).Key); err != nil || string(got) != "a neighbour" {
			t.Fatalf("the neighbour: %q, %v", got, err)
		}
	}
	reopened := func(t *testing.T, dir string, o Object) {
		t.Helper()
		r := openStore(t, dir)
		verified(t, r, o)
	}
	o := testObjects(t, 1)[0]

	// The first write after an open used to adopt the segment on its way to appending,
	// and take the bad record for one already there.
	t.Run("the only copy, in a segment nobody holds", func(t *testing.T) {
		dir := t.TempDir()
		badCopy(t, dir, o)
		s := openStore(t, dir)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		verified(t, s, o)
		s.Close()
		reopened(t, dir, o)
	})
	// The good sealed copy used to satisfy it, and lookups read the other one first.
	t.Run("beside a good sealed copy", func(t *testing.T) {
		dir := t.TempDir()
		sealedFile(t, dir, 1, o)
		badCopy(t, dir, o)
		s := openStore(t, dir)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		verified(t, s, o)
		s.Close()
		reopened(t, dir, o)
	})
	t.Run("in two segments nobody holds", func(t *testing.T) {
		dir := t.TempDir()
		badCopy(t, dir, o)
		// Another segment, held open meanwhile so that the second writer makes its own.
		h, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Put(blobObj(t, []byte("hold")).Key, []byte("hold")); err != nil {
			t.Fatal(err)
		}
		badCopy(t, dir, o)
		h.Close()
		s := openStore(t, dir)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		verified(t, s, o)
		s.Close()
		reopened(t, dir, o)
	})
	// A store that has written already does not look at the directory again by itself,
	// and has to: this call answers for every copy.
	t.Run("in a segment this store has not seen yet", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		own := blobObj(t, []byte("the store's own"))
		if err := s.Put(own.Key, own.Data); err != nil {
			t.Fatal(err)
		}
		badCopy(t, dir, o)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if err := s.Refresh(); err != nil {
			t.Fatal(err)
		}
		verified(t, s, o)
	})
	// Nothing can be done to a segment a live writer holds, and saying nothing would be
	// a lie: lookups go on reading the bad copy.
	t.Run("in a segment a live writer holds", func(t *testing.T) {
		dir := t.TempDir()
		w := openStore(t, dir)
		rec, err := amberpack.EncodeRecord(o.Key, o.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.append(o.Key, rec, true); err != nil {
			t.Fatal(err)
		}
		flipByte(t, w.active.path, int64(len(magicHeader))+amberpack.RecHeaderSize+1)
		s := openStore(t, dir)
		err = s.PutVerified(o.Key, o.Data)
		if err == nil {
			t.Fatal("acknowledged a repair of a copy that is still bad")
		}
		for _, want := range []string{o.Key.String(), segmentName(1, activeSuffix)[:16], "another store"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not say %q: %v", want, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		// And once it has let go, the same call does the job.
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetVerified(o.Key); err != nil || !bytes.Equal(got, o.Data) {
			t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
		}
	})
	// A record whose own header went bad does not even say it is that record.
	t.Run("with a bad header", func(t *testing.T) {
		dir := t.TempDir()
		p := badCopy(t, dir, o)
		flipByte(t, p, int64(len(magicHeader))+1) // in the key
		s := openStore(t, dir)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		verified(t, s, o)
	})
	// Whoever held it has sealed it since this view looked: the view catches up, and the
	// repair is done on the sealed segment.
	t.Run("sealed under the view", func(t *testing.T) {
		dir := t.TempDir()
		p := badCopy(t, dir, o)
		s := openStore(t, dir)
		other := openStore(t, dir)
		other.appendMu.Lock() // seal what nobody holds
		err := other.sealIdleLocked()
		other.appendMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the segment is still active: %v", err)
		}
		s.appendMu.Lock()
		_, err = s.takeBadForeignCopies(o.Key, false)
		s.appendMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.sealed) != 1 || len(s.foreign) != 0 {
			t.Fatalf("the view has not caught up: %d sealed, %d foreign", len(s.sealed), len(s.foreign))
		}
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		verified(t, s, o)
	})
	// A good foreign copy needs nothing.
	t.Run("a good copy in a segment a live writer holds", func(t *testing.T) {
		dir := t.TempDir()
		w := openStore(t, dir)
		if err := w.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		s := openStore(t, dir)
		if err := s.PutVerified(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetVerified(o.Key); err != nil || !bytes.Equal(got, o.Data) {
			t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
		}
	})
}

// A pass copies into a new segment. An empty one it could have taken has an id below the
// pack that survives with a bad copy: once sealed, that pack would be read first, and Get
// does not check what it reads.
func TestAPassCopiesAboveEveryPackThatSurvives(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 3)
	x, other, dead := objs[0], objs[1], objs[2]
	e, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e.appendMu.Lock()
	err = e.ensureActiveLocked()
	e.appendMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	payload := sealedFile(t, dir, 2, x, other) // nothing dead: never a victim; its copy of x goes bad
	sealedFile(t, dir, 3, x, dead)             // the good copy
	flipByte(t, filepath.Join(dir, segmentName(2, sealedSuffix)), payload[x.Key])

	s := openStore(t, dir)
	if _, err := s.Compact(liveOf(x, other), CompactOpts{MarkVerified: true}); err != nil {
		t.Fatal(err)
	}
	s.appendMu.Lock()
	err = s.sealActiveLocked()
	s.appendMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetVerified(x.Key); err != nil || !bytes.Equal(got, x.Data) {
		t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
	}
}

// An empty one does not end the sealing. A torn file is the fullest by size and empty once
// recovered, so it would come first.
func TestAPassSealsEveryIdleSegmentPastAnEmptyOne(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 2)
	w := openStore(t, dir)
	if err := w.Put(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	torn := append(headerAt(versionBase), bytes.Repeat([]byte{0x7a}, 64<<10)...)
	if err := os.WriteFile(filepath.Join(dir, segmentName(99, activeSuffix)), torn, 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir)
	if _, err := s.Compact(liveOf(objs[0]), CompactOpts{}); err != nil {
		t.Fatal(err)
	}
	if !segExists(t, dir, 1) {
		t.Fatal("an idle segment with an object in it was left unsealed")
	}
}

// "Call again when the writer has let go", with writes in between: each try must not seal
// the caller's segment on its way to finding out.
func TestARefusedRepairLeavesTheCallersSegmentAlone(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 3)
	o := objs[0]
	w := openStore(t, dir)
	rec, err := amberpack.EncodeRecord(o.Key, o.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.append(o.Key, rec, true); err != nil {
		t.Fatal(err)
	}
	flipByte(t, w.active.path, int64(len(magicHeader))+amberpack.RecHeaderSize)
	s := openStore(t, dir)
	if err := s.Put(objs[1].Key, objs[1].Data); err != nil {
		t.Fatal(err)
	}
	id := s.active.id
	if err := s.PutVerified(o.Key, o.Data); err == nil || !strings.Contains(err.Error(), "another store holds") {
		t.Fatalf("PutVerified = %v", err)
	}
	if s.active == nil || s.active.id != id {
		t.Fatalf("the caller's segment %d was given up; now %+v, sealed %v", id, s.active, sealedFiles(t, dir))
	}
}

// A verifying ingest writes to the directory it looks at, and a directory changed within
// the racy window is listed every time it is looked at.
func TestDeferredPutsLookAtTheDirectoryOnceASecond(t *testing.T) {
	dir := t.TempDir()
	for i := range 8 {
		sealedFile(t, dir, uint64(i+1), blobObj(t, []byte{byte(i), 1, 2, 3}))
	}
	s := openStore(t, dir)
	objs := distinct(t, 100)
	before, start := s.refreshes.Load(), time.Now()
	for _, o := range objs {
		if err := s.PutVerifiedDeferred(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if n, limit := s.refreshes.Load()-before, int64(3+time.Since(start)/deferredLookEvery); n > limit {
		t.Fatalf("%d listings for 100 puts, limit %d", n, limit)
	}
	// A second on, with nothing else having ended anything, it sees what appeared meanwhile.
	o := blobObj(t, []byte("in between"))
	sealedFile(t, dir, 50, o)
	flipByte(t, filepath.Join(dir, segmentName(50, sealedSuffix)), int64(len(magicHeader))+amberpack.RecHeaderSize+1)
	s.mu.Lock()
	s.listedAt = s.listedAt.Add(-deferredLookEvery)
	s.mu.Unlock()
	if err := s.PutVerifiedDeferred(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	if bad := badCopies(t, s, o); bad != 0 {
		t.Fatalf("%d bad copies left", bad)
	}
	if got, err := s.GetVerified(o.Key); err != nil || !bytes.Equal(got, o.Data) {
		t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
	}
}

// A repair works from the view's mapping, and copies the rest of the pack from there. One
// from a mapping older than another store's repair would undo that repair.
func TestADeferredPutDoesNotUndoARepairAnotherStoreMade(t *testing.T) {
	for _, tc := range []struct {
		name string
		put  func(*Store, key.Key, []byte) error
	}{{"deferred", (*Store).PutVerifiedDeferred}, {"plain", (*Store).PutVerified}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			objs := distinct(t, 3)
			k1, k2, other := objs[0], objs[1], objs[2]
			payload := sealedFile(t, dir, 1, k1, k2)
			p := filepath.Join(dir, segmentName(1, sealedSuffix))
			flipByte(t, p, payload[k1.Key])
			flipByte(t, p, payload[k2.Key])
			b := openStore(t, dir)
			if err := b.PutVerifiedDeferred(other.Key, other.Data); err != nil {
				t.Fatal(err)
			}
			a := openStore(t, dir)
			if err := a.PutVerified(k1.Key, k1.Data); err != nil {
				t.Fatal(err)
			}
			if err := tc.put(b, k2.Key, k2.Data); err != nil {
				t.Fatal(err)
			}
			if err := b.Sync(); err != nil {
				t.Fatal(err)
			}
			r := openStore(t, dir)
			for _, o := range []Object{k1, k2} {
				if got, err := r.GetVerified(o.Key); err != nil || !bytes.Equal(got, o.Data) {
					t.Errorf("%d bytes, %v", len(got), err)
				}
			}
		})
	}
}

// Refused, because a store holds a bad crashed seal. "Call again" must work as soon as it
// has gone, however soon that is.
func TestARefusedDeferredPutGoesThroughWhenTheHolderHasGone(t *testing.T) {
	objs := testObjects(t, 2)
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	rec, err := amberpack.EncodeRecord(objs[0].Key, objs[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.append(objs[0].Key, rec, true); err != nil {
		t.Fatal(err)
	}
	path := w.active.path
	w.afterFooter = func() error { return errors.New("killed") }
	w.appendMu.Lock()
	err = w.sealActiveLocked()
	w.appendMu.Unlock()
	if err == nil {
		t.Fatal("the seal went through")
	}
	flipByte(t, path, int64(len(magicHeader))+amberpack.RecHeaderSize)
	s := openStore(t, dir)
	if err := s.PutVerifiedDeferred(objs[0].Key, objs[0].Data); err == nil || !strings.Contains(err.Error(), "another store holds") {
		t.Fatalf("PutVerifiedDeferred = %v", err)
	}
	w.Close()
	c := openStore(t, dir)
	if err := c.Put(objs[1].Key, objs[1].Data); err != nil { // adopts, and so finishes the seal
		t.Fatal(err)
	}
	if err := s.PutVerifiedDeferred(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetVerified(objs[0].Key); err != nil || !bytes.Equal(got, objs[0].Data) {
		t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
	}
}

// The same for a bad copy in somebody's active segment: a deferred put does not act on
// what it remembers of that one either.
func TestADeferredPutLooksBeforeTakingOverAForeignSegment(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 2)
	o := objs[0]
	s := openStore(t, dir)
	if err := s.Put(objs[1].Key, objs[1].Data); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := amberpack.EncodeRecord(o.Key, o.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.append(o.Key, rec, true); err != nil {
		t.Fatal(err)
	}
	path := w.active.path
	flipByte(t, path, int64(len(magicHeader))+amberpack.RecHeaderSize)
	if err := s.Refresh(); err != nil { // the view now holds the bad copy, and has just looked
		t.Fatal(err)
	}
	before := s.refreshes.Load()
	if err := s.PutVerifiedDeferred(o.Key, o.Data); err == nil || !strings.Contains(err.Error(), "another store holds") {
		t.Fatalf("PutVerifiedDeferred = %v", err)
	}
	if s.refreshes.Load() == before {
		t.Fatal("it refused on the strength of what it remembered")
	}
	w.Close()
}

// A repair moves no generation, so nothing else makes a long-lived store look again.
func TestGetVerifiedSeesARepairAnotherStoreMade(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 2)
	k := objs[0]
	payload := sealedFile(t, dir, 1, k, objs[1])
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[k.Key])
	b := openStore(t, dir)
	if _, err := b.GetVerified(k.Key); !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrVerify) {
		t.Fatalf("before the repair: GetVerified = %v", err)
	}
	a := openStore(t, dir)
	if err := a.PutVerified(k.Key, k.Data); err != nil {
		t.Fatal(err)
	}
	if got, err := b.GetVerified(k.Key); err != nil || !bytes.Equal(got, k.Data) {
		t.Fatalf("after the repair: %d bytes, %v", len(got), err)
	}
}

// Between its two looks at a damaged copy, another store repairs it. There is nothing left
// to do, and that is no failure.
func TestARepairAnotherStoreMadeInTheMeantimeIsNoFailure(t *testing.T) {
	dir := t.TempDir()
	objs := distinct(t, 3)
	k, neighbour, own := objs[0], objs[1], objs[2]
	payload := sealedFile(t, dir, 1, k, neighbour)
	flipByte(t, filepath.Join(dir, segmentName(1, sealedSuffix)), payload[k.Key])
	s := openStore(t, dir)
	if err := s.Put(own.Key, own.Data); err != nil {
		t.Fatal(err)
	}
	a := openStore(t, dir)
	s.afterFooter = func() error {
		if err := a.PutVerified(k.Key, k.Data); err != nil {
			t.Error(err)
		}
		return s.Refresh()
	}
	if err := s.PutVerified(k.Key, k.Data); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetVerified(k.Key); err != nil || !bytes.Equal(got, k.Data) {
		t.Fatalf("GetVerified = %d bytes, %v", len(got), err)
	}
}
