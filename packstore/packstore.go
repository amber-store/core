package packstore

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
	"golang.org/x/sys/unix"
)

// ErrNotFound is returned by Get for a key that is not present in the store.
var ErrNotFound = errors.New("packstore: object not found")

// ErrClosed is returned by operations on a closed store.
var ErrClosed = errors.New("packstore: store closed")

// DefaultSegmentSize is the default rotation threshold: the active segment is
// sealed once it reaches this many bytes.
const DefaultSegmentSize = 2 << 30 // 2 GiB

const (
	sealedSuffix = ".seg"
	activeSuffix = ".seg.active"
)

// Option configures a Store at Open time.
type Option func(*config)

type config struct {
	segmentSize int64
	sync        bool
}

func defaultConfig() config {
	return config{segmentSize: DefaultSegmentSize, sync: true}
}

// WithSegmentSize sets the rotation threshold in bytes. A single oversized
// record may push one segment past it.
func WithSegmentSize(n int64) Option {
	return func(c *config) { c.segmentSize = n }
}

// WithSync controls whether writes are fsynced for crash durability. Default
// is true; disabling it speeds bulk loads and tests.
func WithSync(b bool) Option {
	return func(c *config) { c.sync = b }
}

// activeSegment is the single append-only segment accepting writes.
type activeSegment struct {
	id    uint64
	path  string
	f     *os.File
	size  int64 // accessed only under appendMu
	index map[key.Key]activeLoc
}

// Store is an on-disk content-addressable store over segment files. It is
// safe for concurrent use. Lock ordering: appendMu before mu, never the
// reverse. appendMu serializes the write path (append, fsync, seal, Close);
// mu guards sealed/active/closed for readers.
type Store struct {
	dir  string
	dirF *os.File // holds the flock; also used for directory fsyncs
	cfg  config

	appendMu sync.Mutex

	// write-barrier grey capture, see barrier.go
	capturing atomic.Bool
	greyMu    sync.Mutex
	grey      map[key.Key]struct{}

	mu     sync.RWMutex
	sealed []*sealedSegment // ascending id; newest last
	active *activeSegment   // nil until the first write of a session
	nextID uint64
	closed bool
	failed error // sticky write-path failure; written under appendMu+mu, read under either

	// scrubMu/scrubN/scrubC track in-flight lock-free mmap walks (Verify,
	// ScanIndex, Record): Close/Wipe/Remove wait for scrubN to reach 0 before
	// munmap. A plain sync.WaitGroup does not work here: Remove and Wipe
	// release mu before waiting, so a new scrub can register (Add) while the
	// wait is in progress, and WaitGroup treats a concurrent Add-during-Wait
	// as misuse and panics. The cond var tolerates that: a late scrub cannot
	// reach a segment already detached from the probe list, so the waiter
	// just keeps waiting until scrubN drops back to 0. See beginScrub,
	// endScrub, waitScrubs.
	scrubMu sync.Mutex
	scrubN  int
	retired []*sealedSegment // protected by scrubMu; unmapped when the last scrub ends
	scrubC  *sync.Cond

	writesMu sync.Mutex
	writes   map[*writeToken]time.Time // in-flight Put/WriteBatch/WriteParallel starts

	fsyncs atomic.Int64 // active-segment fsyncs issued, for tests
}

// beginScrub registers a lock-free mmap walk. Call while holding mu.RLock
// after the closed check, so registration is ordered before Close, Wipe and
// Remove detach segments.
func (s *Store) beginScrub() {
	s.scrubMu.Lock()
	s.scrubN++
	s.scrubMu.Unlock()
}

// endScrub deregisters a walk started with beginScrub.
func (s *Store) endScrub() {
	s.scrubMu.Lock()
	s.scrubN--
	if s.scrubN == 0 {
		for _, seg := range s.retired {
			seg.close()
		}
		s.retired = nil
		s.scrubC.Broadcast()
	}
	s.scrubMu.Unlock()
}

// waitScrubs blocks until no scrub is in flight. Unlike WaitGroup.Wait it
// tolerates concurrent registrations: a late scrub cannot reach a segment
// already detached from the probe list, and the waiter just keeps waiting.
func (s *Store) waitScrubs() {
	s.scrubMu.Lock()
	for s.scrubN > 0 {
		s.scrubC.Wait()
	}
	s.scrubMu.Unlock()
}

// Open opens (creating if necessary) a store rooted at dir. Only one Store
// may have a given dir open at a time (flock on the directory). Sealed
// segments are mmap'd and validated; the active segment, if any, is
// tail-scanned and truncated to its last valid record.
func Open(dir string, opts ...Option) (*Store, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("packstore: creating %s: %w", dir, err)
	}
	dirF, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(dirF.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		dirF.Close()
		return nil, fmt.Errorf("packstore: %s is already open: %w", dir, err)
	}
	s := &Store{dir: dir, dirF: dirF, cfg: cfg, nextID: 1, writes: make(map[*writeToken]time.Time)}
	s.scrubC = sync.NewCond(&s.scrubMu)
	if err := s.load(); err != nil {
		s.releaseDir()
		return nil, err
	}
	return s, nil
}

func (s *Store) releaseDir() {
	for _, seg := range s.sealed {
		seg.close()
	}
	s.dirF.Close() // releases the flock
}

// load scans the directory: sealed segments are opened and validated, the
// active segment (at most one) is recovered.
func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var activePaths []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, activeSuffix):
			activePaths = append(activePaths, name)
		case strings.HasSuffix(name, sealedSuffix):
			id, err := parseSegmentID(name, sealedSuffix)
			if err != nil {
				return err
			}
			seg, err := openSealed(filepath.Join(s.dir, name), id)
			if err != nil {
				return err
			}
			s.sealed = append(s.sealed, seg)
			if id >= s.nextID {
				s.nextID = id + 1
			}
		}
		// Anything else (e.g. .DS_Store) is ignored.
	}
	slices.SortFunc(s.sealed, func(a, b *sealedSegment) int { return cmp.Compare(a.id, b.id) })

	if len(activePaths) > 1 {
		return fmt.Errorf("%w: %d active segments, want at most one: %v", ErrCorrupt, len(activePaths), activePaths)
	}
	if len(activePaths) == 0 {
		return nil
	}
	return s.recoverActive(activePaths[0])
}

func parseSegmentID(name, suffix string) (uint64, error) {
	hex := strings.TrimSuffix(name, suffix)
	id, err := strconv.ParseUint(hex, 16, 64)
	if err != nil || len(hex) != 16 {
		return 0, fmt.Errorf("%w: bad segment file name %q", ErrCorrupt, name)
	}
	return id, nil
}

// recoverActive tail-scans name, then either completes a crashed seal-rename
// or truncates the file to its valid prefix and resumes it as the active
// segment.
func (s *Store) recoverActive(name string) error {
	id, err := parseSegmentID(name, activeSuffix)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	res, err := scanActive(path)
	if err != nil {
		return err
	}
	if id >= s.nextID {
		s.nextID = id + 1
	}
	if res.sealed {
		// Crash between footer-write and rename: complete the rename.
		sealedPath := strings.TrimSuffix(path, ".active")
		if err := os.Rename(path, sealedPath); err != nil {
			return err
		}
		if err := s.dirF.Sync(); err != nil {
			return err
		}
		seg, err := openSealed(sealedPath, id)
		if err != nil {
			return err
		}
		s.sealed = append(s.sealed, seg)
		slices.SortFunc(s.sealed, func(a, b *sealedSegment) int { return cmp.Compare(a.id, b.id) })
		return nil
	}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	size := res.size
	if size < int64(len(magicHeader)) {
		// Header never became durable: reset to a fresh header.
		// This is deliberate and silent; nothing acknowledged is lost.
		if err := f.Truncate(0); err != nil {
			f.Close()
			return err
		}
		if _, err := f.WriteAt(magicHeader, 0); err != nil {
			f.Close()
			return err
		}
		size = int64(len(magicHeader))
	} else {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return err
		}
	}
	s.active = &activeSegment{id: id, path: path, f: f, size: size, index: res.index}
	return nil
}

// createActive opens the next-numbered active segment. Called under appendMu.
func (s *Store) createActive() error {
	id := s.nextID
	s.nextID++
	path := filepath.Join(s.dir, fmt.Sprintf("%016x%s", id, activeSuffix))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(path) // never leave a second .seg.active to brick the next Open
		return err
	}
	if _, err := f.WriteAt(magicHeader, 0); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := s.dirF.Sync(); err != nil {
		return fail(err)
	}
	a := &activeSegment{id: id, path: path, f: f, size: int64(len(magicHeader)), index: make(map[key.Key]activeLoc)}
	s.mu.Lock()
	s.active = a
	s.mu.Unlock()
	return nil
}

// append writes one encoded record to the active segment (creating it if
// needed), publishes it in the active index, optionally fsyncs, and seals the
// segment if it reached the rotation threshold.
func (s *Store) append(k key.Key, rec []byte, syncNow bool) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.appendLocked(k, rec, syncNow)
}

// appendLocked is append's body. The caller must hold appendMu.
func (s *Store) appendLocked(k key.Key, rec []byte, syncNow bool) error {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if s.failed != nil {
		return s.failed
	}
	if s.active == nil {
		if err := s.createActive(); err != nil {
			return err
		}
	}
	a := s.active
	if _, ok := a.index[k]; ok {
		return nil // lost a Put race for this key; the record is already appended
	}
	off := a.size
	if _, err := a.f.WriteAt(rec, off); err != nil {
		return err
	}
	loc := activeLoc{
		off:   off,
		flags: rec[33],
		ulen:  binary.BigEndian.Uint32(rec[34:38]),
		slen:  binary.BigEndian.Uint32(rec[38:42]),
	}
	s.mu.Lock()
	a.index[k] = loc
	s.mu.Unlock()
	a.size = off + int64(len(rec))

	if syncNow && s.cfg.sync {
		if err := a.f.Sync(); err != nil {
			s.setFailed(err)
			return err
		}
	}
	if a.size >= s.cfg.segmentSize {
		// A mid-seal failure can leave a renamed-but-unpublished segment or
		// an un-mmap'd sealed file; reads stay correct (the fd is still
		// open), but accepting further writes could append past a footer.
		// Poison the write path; reopen recovers cleanly.
		if err := s.sealActiveLocked(); err != nil {
			s.setFailed(err)
			return err
		}
	}
	return nil
}

// syncActive fsyncs the active segment, if syncing is enabled and one exists.
func (s *Store) syncActive() error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if s.failed != nil {
		return s.failed
	}
	if !s.cfg.sync || s.active == nil {
		return nil
	}
	if err := s.active.f.Sync(); err != nil {
		s.setFailed(err)
		return err
	}
	s.fsyncs.Add(1)
	return nil
}

// setFailed poisons the write path after an fsync failure: a failed fsync may
// have dropped dirty pages, so later appends could be acknowledged while
// sitting behind a garbage hole that tail-scan recovery would truncate. Reads
// stay available; only new acknowledgments stop. Called under appendMu.
func (s *Store) setFailed(err error) {
	s.mu.Lock()
	if s.failed == nil {
		s.failed = fmt.Errorf("packstore: write path failed: %w", err)
	}
	s.mu.Unlock()
}

// sealActiveLocked seals the active segment: build the footer from the
// in-RAM index (no body re-read), append it, fsync, rename to .seg, fsync the
// directory, and swap in the mmap'd sealed segment. Called under appendMu.
func (s *Store) sealActiveLocked() error {
	a := s.active
	if a == nil || len(a.index) == 0 {
		return nil
	}
	entries := make([]indexEntry, 0, len(a.index))
	for k, loc := range a.index {
		entries = append(entries, indexEntry{k: k, off: uint64(loc.off), slen: loc.slen})
	}
	footer, err := buildFooter(a.size, entries)
	if err != nil {
		return err
	}
	// The footer is located from EOF, so drop anything a failed WriteAt
	// left past a.size.
	if err := a.f.Truncate(a.size); err != nil {
		return err
	}
	if _, err := a.f.WriteAt(footer, a.size); err != nil {
		return err
	}
	if err := a.f.Sync(); err != nil {
		return err
	}
	sealedPath := strings.TrimSuffix(a.path, ".active")
	if err := os.Rename(a.path, sealedPath); err != nil {
		return err
	}
	if err := s.dirF.Sync(); err != nil {
		return err
	}
	seg, err := openSealed(sealedPath, a.id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sealed = append(s.sealed, seg)
	s.active = nil
	s.mu.Unlock()
	// Close the fd only after the swap: in-flight readers hold mu.RLock for
	// their whole pread, so the Lock above drained them, and post-swap
	// readers route to the sealed mmap. A close error fails the triggering
	// Put even though the object is sealed and durable — harmless; a retried
	// Put dedups via Has.
	return a.f.Close()
}

// WriteBatch stores every object the iterator yields, fsyncing once at the
// end (when WithSync is enabled): on return, all yielded objects are durable.
// It is NOT atomic — a crash or iterator error
// can leave a valid prefix stored. In a content-addressed store that prefix
// is harmless: identical re-pushed content deduplicates. Objects repeated
// within the batch, or already present, are written once. When WriteBatch
// returns an error after appending part of the batch, it best-effort fsyncs
// that prefix first, so Has-visible records never stay non-durable.
func (s *Store) WriteBatch(seq iter.Seq2[Object, error]) error {
	w := s.beginWrite()
	defer s.endWrite(w)
	seen := make(map[key.Key]struct{})
	appended := false
	fail := func(err error) error {
		if appended {
			s.syncActive() // best-effort; an fsync failure poisons the store
		}
		return err
	}
	for obj, err := range seq {
		if err != nil {
			return fail(err)
		}
		if _, dup := seen[obj.Key]; dup {
			continue
		}
		seen[obj.Key] = struct{}{}
		s.observe(obj.Key)
		has, err := s.Has(obj.Key)
		if err != nil {
			return fail(fmt.Errorf("exists (%s): %w", obj.Key, err))
		}
		if has {
			continue
		}
		rec, _, err := prepare(obj, false)
		if err != nil {
			return fail(err)
		}
		if err := s.append(obj.Key, rec, false); err != nil {
			return fail(err)
		}
		appended = true
	}
	return s.syncActive()
}

// Put stores a single object under k, deduplicating against existing content.
// A dedup hit returns success without fsyncing; if the matching record was
// appended by a still-running batch, its durability rides on that batch's commit.
func (s *Store) Put(k key.Key, data []byte) error {
	w := s.beginWrite()
	defer s.endWrite(w)
	s.mu.RLock()
	failed := s.failed
	s.mu.RUnlock()
	if failed != nil {
		return failed
	}
	s.observe(k)
	has, err := s.Has(k)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	rec, err := amberpack.EncodeRecord(k, data)
	if err != nil {
		return err
	}
	return s.append(k, rec, true)
}

// Get returns the bytes stored under k, or ErrNotFound if k is absent. The
// returned slice is caller-owned.
func (s *Store) Get(k key.Key) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.active != nil {
		if loc, ok := s.active.index[k]; ok {
			stored := make([]byte, loc.slen)
			if _, err := s.active.f.ReadAt(stored, loc.off+amberpack.RecHeaderSize); err != nil {
				return nil, err
			}
			return amberpack.DecodePayload(loc.flags, loc.ulen, stored)
		}
	}
	for i := len(s.sealed) - 1; i >= 0; i-- {
		data, found, err := s.sealed[i].get(k)
		// A corrupt segment fails the read loudly rather than falling back to
		// older copies: masking corruption would hide real damage from scrub.
		if err != nil {
			return nil, err
		}
		if found {
			return data, nil
		}
	}
	return nil, ErrNotFound
}

// GetRecord returns a caller-owned copy of the full on-disk record stored under
// k — its 46-byte header plus the stored (still-compressed) payload, exactly as
// written by amberpack.EncodeRecord — or ErrNotFound if k is absent. This is the
// zero-copy push path: the record is wire-format-identical, so a caller can hand
// it to amberpack.Writer.AddRecord without decompressing and re-encoding. Like
// Get, it does not CRC-check; the receiving Reader validates framing and CRC.
func (s *Store) GetRecord(k key.Key) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.active != nil {
		if loc, ok := s.active.index[k]; ok {
			rec := make([]byte, amberpack.RecHeaderSize+int(loc.slen))
			if _, err := s.active.f.ReadAt(rec, loc.off); err != nil {
				return nil, err
			}
			return rec, nil
		}
	}
	for i := len(s.sealed) - 1; i >= 0; i-- {
		rec, found, err := s.sealed[i].getRecord(k)
		if err != nil {
			return nil, err
		}
		if found {
			return rec, nil
		}
	}
	return nil, ErrNotFound
}

// StoredSize returns the stored (post-compression) payload length of the object
// under k and whether k was found, reading only the index — no payload read.
// It sizes objects for byte-balanced push batching against the bytes that
// actually travel.
func (s *Store) StoredSize(k key.Key) (uint64, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, false, ErrClosed
	}
	if s.active != nil {
		if loc, ok := s.active.index[k]; ok {
			return uint64(loc.slen), true, nil
		}
	}
	for i := len(s.sealed) - 1; i >= 0; i-- {
		if slen, ok := s.sealed[i].storedSize(k); ok {
			return uint64(slen), true, nil
		}
	}
	return 0, false, nil
}

// locateLocked returns the segment id and record offset where k lives, for
// ordering reads by physical layout. The caller must hold s.mu (read or write).
func (s *Store) locateLocked(k key.Key) (seg, off uint64, ok bool) {
	if s.active != nil {
		if loc, ok := s.active.index[k]; ok {
			return s.active.id, uint64(loc.off), true
		}
	}
	for i := len(s.sealed) - 1; i >= 0; i-- {
		if o, found := s.sealed[i].locate(k); found {
			return s.sealed[i].id, o, true
		}
	}
	return 0, 0, false
}

// SortByLocation reorders keys in place to follow the store's on-disk layout —
// grouped by segment, ascending offset within a segment — so reading them in
// order is a near-sequential sweep per segment rather than scattered random
// access. Absent keys sort last (their reads surface ErrNotFound later). It is
// a no-op on a closed store.
func (s *Store) SortByLocation(keys []key.Key) {
	type located struct {
		k        key.Key
		seg, off uint64
		ok       bool
	}
	items := make([]located, len(keys))
	s.mu.RLock()
	closed := s.closed
	if !closed {
		for i, k := range keys {
			seg, off, ok := s.locateLocked(k)
			items[i] = located{k, seg, off, ok}
		}
	}
	s.mu.RUnlock()
	if closed {
		return
	}
	slices.SortFunc(items, func(a, b located) int {
		if a.ok != b.ok {
			if a.ok { // present keys before absent ones
				return -1
			}
			return 1
		}
		if c := cmp.Compare(a.seg, b.seg); c != 0 {
			return c
		}
		return cmp.Compare(a.off, b.off)
	})
	for i := range items {
		keys[i] = items[i].k
	}
}

// Has reports whether an object is stored under k.
func (s *Store) Has(k key.Key) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false, ErrClosed
	}
	if s.active != nil {
		if _, ok := s.active.index[k]; ok {
			return true, nil
		}
	}
	for i := len(s.sealed) - 1; i >= 0; i-- {
		if s.sealed[i].has(k) {
			return true, nil
		}
	}
	return false, nil
}

// Wipe deletes every object: the active segment and all sealed segments are
// closed and their files removed, leaving an empty, still-open store (the
// store-wipe operation). Readers are drained via the write lock before
// segments are detached; in-flight Verify walks are waited out before
// unmapping, exactly like Close. nextID stays monotonic so segment names
// never repeat within a session.
func (s *Store) Wipe() error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	active := s.active
	sealed := s.sealed
	s.active = nil
	s.sealed = nil
	// A sticky write-path failure (setFailed after a bad fsync) poisons the
	// data the fsync may have torn — data the wipe is about to destroy. The
	// reset clears it: the reopened-empty store must accept writes again.
	s.failed = nil
	// From here readers see an empty store; a Verify that started after this
	// unlock walks an empty snapshot. Pre-existing scrubs still hold the old
	// mmaps, so wait before unmapping (see Close). waitScrubs tolerates a
	// scrub registering after this unlock (unlike WaitGroup.Wait): it can
	// only ever reach the empty snapshot above, never the detached segments.
	s.mu.Unlock()
	s.waitScrubs()

	var firstErr error
	if active != nil {
		if err := active.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := os.Remove(active.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, seg := range sealed {
		if err := seg.close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := os.Remove(seg.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.cfg.sync {
		if err := s.dirF.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close fsyncs and closes the active segment (without sealing it), unmaps all
// sealed segments, and releases the directory lock.
func (s *Store) Close() error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	var firstErr error
	if s.active != nil {
		if err := s.active.f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.active.f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.active = nil
	}
	// Wait for in-flight Verify walks before unmapping: the scrub reads the
	// mmaps lock-free, and munmap under it is an uncatchable SIGSEGV. New
	// scrubs cannot start (closed is set; Verify checks it under mu.RLock).
	// Callers wanting a faster Close cancel Verify's context first.
	s.mu.Unlock()
	s.waitScrubs()

	for _, seg := range s.sealed {
		if err := seg.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.sealed = nil
	if err := s.dirF.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
