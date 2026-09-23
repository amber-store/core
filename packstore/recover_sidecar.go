package packstore

import (
	"bytes"
	"cmp"
	"io"
	"os"
	"slices"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// recovered is what an active segment holds, worked out from its sidecar and
// its data file. Working it out never writes; the segment's owner applies it
// (truncating the data, bringing the sidecar in line), a reader just uses the
// index.
type recovered struct {
	index      map[key.Key]activeLoc
	dataEnd    int64        // end of the valid data; short of the header: it never became durable
	sidecarEnd int64        // leading sidecar bytes that agree with the data; 0: start the sidecar over
	missing    []sidecarRec // valid records the agreeing part of the sidecar does not list, in data order
	sealed     bool         // the data file carries a complete footer: a seal's rename is outstanding
}

// recoverSegment recovers the active segment at path by the sidecar's
// reading rules (sidecar.go), falling back to a full scan of the data when
// the sidecar cannot be used.
func recoverSegment(path string) (recovered, error) {
	f, err := os.Open(path)
	if err != nil {
		return recovered{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return recovered{}, err
	}
	// An unreadable sidecar is a missing one: the data file is the truth.
	sidecar, _ := os.ReadFile(path + sidecarSuffix)
	res, ok, err := recoverFrom(f, st.Size(), sidecar)
	if err != nil {
		return recovered{}, err
	}
	if ok {
		return res, nil
	}
	return fullScan(path)
}

// fullScan is recovery without a sidecar: every record is read and checked.
func fullScan(path string) (recovered, error) {
	res, err := scanActive(path)
	if err != nil {
		return recovered{}, err
	}
	out := recovered{index: res.index, dataEnd: res.size, sealed: res.sealed}
	for k, loc := range res.index {
		out.missing = append(out.missing, entryRec(k, loc))
	}
	slices.SortFunc(out.missing, func(a, b sidecarRec) int { return cmp.Compare(a.off, b.off) })
	return out, nil
}

// recoverFrom applies the reading rules to a data file of the given size and
// the bytes of its sidecar. ok is false when the sidecar cannot be relied on
// at all, or the file looks like a crashed seal: the caller then scans it
// whole.
//
// Entries whose record ends within the data known durable (the largest synced
// record) are trusted unread. Later entries are verified against the data, in
// order, until one fails. What follows the last good entry is scanned record
// by record, which finds records written — perhaps acknowledged — just before
// a crash kept their entries from the sidecar.
func recoverFrom(data io.ReaderAt, size int64, sidecar []byte) (res recovered, ok bool, err error) {
	recs, valid := readSidecar(sidecar, true)
	headerLen := int64(len(magicHeader))
	if valid == 0 || size < headerLen {
		return recovered{}, false, nil
	}
	header, err := readRange(data, 0, headerLen)
	if err != nil {
		return recovered{}, false, err
	}
	if !bytes.Equal(header, magicHeader) {
		return recovered{}, false, nil
	}
	if size >= headerLen+trailerSize {
		tail, err := readRange(data, size-int64(len(magicTrailer)), int64(len(magicTrailer)))
		if err != nil {
			return recovered{}, false, err
		}
		if bytes.Equal(tail, magicTrailer) {
			return recovered{}, false, nil // a footer: the full scan decides whether it is whole
		}
	}
	durable := headerLen
	for _, r := range recs {
		if r.kind != sidecarSynced {
			continue
		}
		if int64(r.off) > size || int64(r.off) < 0 {
			// More durable data than the file holds: whatever happened to
			// the file, this sidecar does not describe it.
			return recovered{}, false, nil
		}
		durable = max(durable, int64(r.off))
	}

	res = recovered{index: make(map[key.Key]activeLoc), sidecarEnd: int64(len(sidecarMagic))}
	pos := headerLen
	for _, r := range recs {
		if r.kind == sidecarEntry {
			// Records are contiguous and every one has its entry, so an
			// entry that does not start where the last one ended, or runs
			// past the file, ends what the sidecar can vouch for.
			if int64(r.off) != pos || r.end() > size || r.end() < pos {
				break
			}
			if r.end() > durable {
				good, err := verifyEntry(data, r)
				if err != nil {
					return recovered{}, false, err
				}
				if !good {
					break
				}
			}
			res.index[r.k] = r.loc()
			pos = r.end()
		}
		res.sidecarEnd += sidecarRecSize
	}

	if pos < size {
		tail, err := readRange(data, pos, size-pos)
		if err != nil {
			return recovered{}, false, err
		}
		off := 0
		for off < len(tail) && tail[off] != tagSeal { // a seal marker without a whole footer: a torn seal
			rec, err := amberpack.ParseRecord(tail[off:])
			if err != nil {
				break // invalid or truncated: everything from here on is garbage
			}
			loc := activeLoc{off: pos + int64(off), flags: rec.Flags, ulen: rec.Ulen, slen: rec.Slen}
			res.index[rec.Key] = loc
			res.missing = append(res.missing, entryRec(rec.Key, loc))
			off += amberpack.RecHeaderSize + int(rec.Slen)
		}
		pos += int64(off)
	}
	res.dataEnd = pos
	return res, true, nil
}

// verifyEntry reads the record an entry points at and checks that it is
// whole and is the record the entry describes.
func verifyEntry(data io.ReaderAt, r sidecarRec) (bool, error) {
	raw, err := readRange(data, int64(r.off), r.end()-int64(r.off))
	if err != nil {
		return false, err
	}
	rec, err := amberpack.ParseRecord(raw)
	return err == nil && rec.Key == r.k && rec.Flags == r.flags && rec.Ulen == r.ulen && rec.Slen == r.slen, nil
}

func readRange(data io.ReaderAt, off, n int64) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(data, off, n), b); err != nil {
		return nil, err
	}
	return b, nil
}

// openOwnedSidecar brings a recovered segment's sidecar in line for its new
// owner: it keeps the part that agrees with the data, drops the rest and
// lists the records that were missing. It never fails the open. A sidecar
// that cannot be written must not stay as it is — recovery would go on
// trusting it — so it is removed, and the segment runs without one.
func openOwnedSidecar(dataPath string, res recovered) *sidecarWriter {
	path := dataPath + sidecarSuffix
	w, err := openSidecarAt(path, res.sidecarEnd)
	if err != nil {
		os.Remove(path)
		return nil
	}
	for _, r := range res.missing {
		w.write(r)
	}
	return w
}
