package packstore

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// PutVerified verifies data and repairs every corrupt indexed copy of k.
// Repairs preserve segment IDs and unrelated records through a synced rename.
// Existing readers retain their old mappings until their walks finish.
// New objects use the configured write durability, like Put.
//
// A bad copy in another store's active segment is reached by taking that segment over,
// and sealing it. One that another store holds, in an active segment or a crashed seal,
// cannot be repaired, and lookups may read it before any other copy, so that is an
// error: call again when that store has let go.
func (s *Store) PutVerified(k key.Key, data []byte) error {
	return s.putVerified(k, data, true)
}

// PutVerifiedDeferred is PutVerified without the fsync of a new or healthy
// active record. The caller ends a run of these with Sync, and must not treat
// the content as stored before that returns. Repairs still sync.
//
// It looks at the directory at most once a second (deferredLookEvery), so it does not
// answer for a copy that appeared since. Before it repairs or refuses anything it looks.
func (s *Store) PutVerifiedDeferred(k key.Key, data []byte) error {
	return s.putVerified(k, data, false)
}

// deferredLookEvery is how often a deferred put looks at the directory. An ingest cannot
// afford a listing per object, and what appeared since is too new to have gone bad.
const deferredLookEvery = time.Second

// finishCrashedSeals finishes every crashed seal in the view that holds k. Called
// under appendMu.
func (s *Store) finishCrashedSeals(k key.Key) error {
	s.mu.RLock()
	var crashed []*sealedSegment
	for _, g := range s.sealed {
		if g.crashedSeal() && g.has(k) {
			crashed = append(crashed, g)
		}
	}
	s.mu.RUnlock()
	for _, g := range crashed {
		if err := s.finishCrashedSeal(g); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) putVerified(k key.Key, data []byte, syncNow bool) error {
	if err := verifyObject(Object{Key: k, Data: data}); err != nil {
		return err
	}
	w, gerr := s.beginWrite()
	if gerr != nil {
		return gerr
	}
	defer s.endWrite(w)
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.mu.RLock()
	closed, failed := s.closed, s.failed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if failed != nil {
		return failed
	}
	s.observe(k)
	// What is in the directory now, not what this view last saw: this call answers for
	// every copy, and appending takes over a segment without looking at what it holds.
	s.mu.RLock()
	listed := s.listedAt
	s.mu.RUnlock()
	skipped := !syncNow && time.Since(listed) < deferredLookEvery
	if !skipped {
		if _, err := s.refreshAfterMiss(false); err != nil {
			return err
		}
	}
	lookFirst, err := s.putVerifiedInView(k, data, syncNow, skipped)
	if !lookFirst {
		return err
	}
	// What it found wrong is what this view remembers, and a repair next door moves no
	// generation. A repair works from the mapping, so one from an old mapping would put
	// back what that repair took away.
	if err := s.refresh(); err != nil {
		return err
	}
	_, err = s.putVerifiedInView(k, data, syncNow, false)
	return err
}

// putVerifiedInView is putVerified over the view as it stands. With skipped, this call
// has not looked at the directory, and rather than rewrite or refuse anything it says so,
// by lookFirst. Called under appendMu.
func (s *Store) putVerifiedInView(k key.Key, data []byte, syncNow, skipped bool) (lookFirst bool, err error) {
	if lookFirst, err := s.takeBadForeignCopies(k, skipped); lookFirst || err != nil {
		return lookFirst, err
	}
	found, damaged := false, false
	if s.active != nil {
		if loc, ok := s.active.index[k]; ok {
			found = true
			raw := make([]byte, amberpack.RecHeaderSize+int(loc.slen))
			_, err := s.active.f.ReadAt(raw, loc.off)
			if err != nil {
				return false, err
			}
			damaged = !validRepairRecord(k, raw)
		}
	}
	// A seal whose writer crashed may never have been fsynced: finish it, so that
	// what it holds can be relied on, or repaired, like any other segment's.
	if s.cfg.sync {
		if err := s.finishCrashedSeals(k); err != nil {
			return false, err
		}
	}
	// The view can change under a refresh, which takes no appendMu: walk a
	// snapshot, registered as a scrub so that nothing in it is unmapped.
	s.mu.RLock()
	s.beginScrub()
	sealed := slices.Clone(s.sealed)
	s.mu.RUnlock()
	defer s.endScrub()
	for _, seg := range sealed {
		off, slen, ok := seg.fv.lookup(k)
		if !ok {
			continue
		}
		raw, err := repairRecord(seg, indexEntry{k: k, off: off, slen: slen})
		bad := err != nil || !validRepairRecord(k, raw)
		if s.cfg.sync && seg.crashedSeal() {
			// Somebody else holds it (finishCrashedSeals): no copy to rely on yet, and
			// not this store's to rewrite.
			if bad && skipped {
				return true, nil
			}
			if bad {
				return false, errHeldBadCopy(k, seg.id)
			}
			continue
		}
		found = true
		damaged = damaged || bad
	}
	if found && !damaged {
		if syncNow && s.cfg.sync && s.active != nil {
			if _, ok := s.active.index[k]; ok {
				if err := s.fsync(s.active.f); err != nil {
					s.setFailed(err)
					return false, err
				}
				s.active.sc.synced(s.active.size)
			}
		}
		return false, nil
	}
	if found && skipped {
		return true, nil
	}
	replacement, err := s.encode(k, data)
	if err != nil {
		return false, err
	}
	if !found {
		if !syncNow {
			// Before the append: Put reads the flag without appendMu, once
			// it has found the record.
			s.deferred.Store(true)
		}
		return false, s.appendLocked(k, replacement, syncNow)
	}
	// Seal from the live index: a prefix scan would discard later records
	// after a damaged active record.
	if err := s.sealActiveLocked(); err != nil {
		return false, err
	}
	s.mu.RLock()
	sealed = slices.Clone(s.sealed) // again: it now holds what was the active segment
	s.mu.RUnlock()
	for _, seg := range sealed {
		damaged := false
		if off, slen, ok := seg.fv.lookup(k); ok {
			raw, err := repairRecord(seg, indexEntry{k: k, off: off, slen: slen})
			damaged = err != nil || !validRepairRecord(k, raw)
		}
		if damaged {
			if err := s.repairSegment(seg, k, replacement); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

// takeBadForeignCopies takes over every active segment of somebody else's that holds a
// bad copy of k, and seals all but the last, which puts the copies where a repair can
// reach them. With skipped, it says lookFirst instead of acting on a bad one. Called
// under appendMu.
func (s *Store) takeBadForeignCopies(k key.Key, skipped bool) (lookFirst bool, err error) {
	s.mu.RLock()
	copies := s.foreignCopiesLocked(k)
	s.mu.RUnlock()
	for _, c := range copies {
		if c.raw != nil && validRepairRecord(k, c.raw) {
			continue
		}
		if skipped {
			return true, nil
		}
		// Before this store's own segment is given up for it: a caller that keeps trying
		// while the other store lives must not seal a pack per try.
		held, err := isHeld(c.fa.path)
		if err != nil {
			return false, err
		}
		if held {
			return false, errHeldBadCopy(k, c.fa.id)
		}
		if err := s.leaveActiveLocked(); err != nil {
			return false, err
		}
		adopted, err := s.adopt(c.fa.id, c.fa.path, versionBase) // whatever its version: it is to be sealed
		if err != nil {
			return false, err
		}
		if !adopted {
			if _, err := os.Stat(c.fa.path); !errors.Is(err, fs.ErrNotExist) {
				return false, errHeldBadCopy(k, c.fa.id)
			}
			// Sealed by whoever held it, or finished here as a crashed seal.
			if err := s.refresh(); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

func errHeldBadCopy(k key.Key, id uint64) error {
	return fmt.Errorf("packstore: key %s: the copy in segment %016x is bad, and another store holds that segment", k, id)
}

// isHeld reports whether somebody holds the segment at path, for now. A file that is
// gone is held by nobody.
func isHeld(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	ok, err := tryLock(f)
	return err == nil && !ok, err
}

func validRepairRecord(k key.Key, raw []byte) bool {
	rec, err := amberpack.ParseRecord(raw)
	return err == nil && len(raw) == amberpack.RecHeaderSize+int(rec.Slen) && checkRecord(k, raw) == nil
}

func repairRecord(seg *sealedSegment, e indexEntry) ([]byte, error) {
	body := uint64(seg.fv.bodyLen)
	size := uint64(amberpack.RecHeaderSize) + uint64(e.slen)
	if e.off < uint64(len(magicHeader)) || e.off > body || size > body-e.off {
		return nil, fmt.Errorf("%w: repair record outside segment", ErrCorrupt)
	}
	return seg.mm[e.off : e.off+size], nil
}

// repairSegment runs under appendMu. Replacement never changes an old mmap.
func (s *Store) repairSegment(seg *sealedSegment, k key.Key, replacement []byte) error {
	entries := slices.Collect(seg.fv.allEntries())
	slices.SortFunc(entries, func(a, b indexEntry) int { return cmp.Compare(a.off, b.off) })
	path := seg.file()
	temporary := path + ".repair"
	if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	defer file.Close()
	// The rewritten segment keeps its version, raised when the replacement
	// is a record that the old one may not hold.
	if _, err := file.Write(headerAt(max(seg.version(), versionFor(k, replacement[33])))); err != nil {
		return err
	}
	offset := int64(len(magicHeader))
	index := make([]indexEntry, 0, len(entries))
	for _, e := range entries {
		raw := replacement
		if e.k != k {
			raw, err = repairRecord(seg, e)
			if err != nil {
				return err
			}
		}
		if _, err := file.Write(raw); err != nil {
			return err
		}
		index = append(index, indexEntry{k: e.k, off: uint64(offset), slen: uint32(len(raw) - amberpack.RecHeaderSize)})
		offset += int64(len(raw))
	}
	footer, err := buildFooter(offset, index)
	if err != nil {
		return err
	}
	if _, err := file.Write(footer); err != nil {
		return err
	}
	if err := s.fsync(file); err != nil {
		return err
	}
	ready, err := openSealed(temporary, seg.id)
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		ready.close()
		return err
	}
	if err := s.fsync(s.dirF); err != nil {
		ready.close()
		s.setFailed(err)
		return err
	}
	ready.path = path
	// The replacement takes seg's place by id. Readers holding mu have
	// drained; registered mmap walks — this one included — can still use seg,
	// which is unmapped when the last of them ends. Never wait under
	// appendMu: a ScanIndex callback may itself write.
	s.mu.Lock()
	s.publishSealedLocked(ready)
	s.mu.Unlock()
	return nil
}
