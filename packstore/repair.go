package packstore

import (
	"cmp"
	"fmt"
	"os"
	"slices"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// PutVerified verifies data and repairs every corrupt indexed copy of k.
// Repairs preserve segment IDs and unrelated records through a synced rename.
// Existing readers retain their old mappings until their walks finish.
// New objects use the configured write durability, like Put.
func (s *Store) PutVerified(k key.Key, data []byte) error {
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
	found, damaged := false, false
	if s.active != nil {
		if loc, ok := s.active.index[k]; ok {
			found = true
			raw := make([]byte, amberpack.RecHeaderSize+int(loc.slen))
			_, err := s.active.f.ReadAt(raw, loc.off)
			if err != nil {
				return err
			}
			damaged = !validRepairRecord(k, raw)
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
		if off, slen, ok := seg.fv.lookup(k); ok {
			found = true
			raw, err := repairRecord(seg, indexEntry{k: k, off: off, slen: slen})
			if err != nil || !validRepairRecord(k, raw) {
				damaged = true
			}
		}
	}
	if found && !damaged {
		if s.cfg.sync && s.active != nil {
			if _, ok := s.active.index[k]; ok {
				if err := s.active.f.Sync(); err != nil {
					s.setFailed(err)
					return err
				}
				s.active.sc.synced(s.active.size)
			}
		}
		return nil
	}
	replacement, err := amberpack.EncodeRecord(k, data)
	if err != nil {
		return err
	}
	if !found {
		return s.appendLocked(k, replacement, true)
	}
	// Seal from the live index: a prefix scan would discard later records
	// after a damaged active record.
	if err := s.sealActiveLocked(); err != nil {
		s.setFailed(err)
		return err
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
				return err
			}
		}
	}
	return nil
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
	temporary := seg.path + ".repair"
	if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	defer file.Close()
	if _, err := file.Write(magicHeader); err != nil {
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
	if err := file.Sync(); err != nil {
		return err
	}
	ready, err := openSealed(temporary, seg.id)
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, seg.path); err != nil {
		ready.close()
		return err
	}
	if err := s.dirF.Sync(); err != nil {
		ready.close()
		s.setFailed(err)
		return err
	}
	ready.path = seg.path
	// The replacement takes seg's place by id. Readers holding mu have
	// drained; registered mmap walks — this one included — can still use seg,
	// which is unmapped when the last of them ends. Never wait under
	// appendMu: a ScanIndex callback may itself write.
	s.mu.Lock()
	s.publishSealedLocked(ready)
	s.mu.Unlock()
	return nil
}
