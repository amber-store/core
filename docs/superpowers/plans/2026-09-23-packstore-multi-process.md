# Packstore Multi-Process Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give active segments a sidecar index so that opening a store no longer scans them, and let any number of processes read and write one packstore at once, with GC staying safe.

**Architecture:** Every active segment gets an append-only sidecar (`<id>.seg.active.idx`) of fixed-size, CRC'd records: one entry per data record and a `synced` marker per data fsync. Opening trusts entries up to the last marker, verifies the rest against the data, and tail-scans what the index does not cover. The exclusive directory lock goes away: a writer owns an active segment through an `flock` on its data file and adopts an unlocked one before creating another; readers lock nothing and refresh their view of the directory on a miss. One lock file, `gc.lock`, carries the cross-process form of the collector's reference lock — shared for write spans and reference publication, exclusive for a whole GC cycle — and a generation counter that makes stale views refresh before a duplicate check.

**Tech Stack:** Go 1.26, `golang.org/x/sys/unix` (`flock`), no new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-23-packstore-multi-process-design.md`

**On the form of this plan.** The executor is the author of the design and works test-first, so tasks give exact files, signatures, the tests with what each pins, and the algorithms; code is written against the existing 800-line `packstore.go` during execution rather than copied from here. Anything execution changes is recorded under "Amendments during execution".

## Global Constraints

- Sealed segment format unchanged. A store written by an earlier release opens as is.
- Sidecar: `<id>.seg.active.idx`; magic `AMBERIX\x01`; 56-byte big-endian records `kind(1) key(32) off(8) flags(1) ulen(4) slen(4) zero(2) crc32c(4)`; kinds `0x01` entry, `0x02` synced; CRC-32C (Castagnoli) over bytes `[0:52]`. Never fsynced.
- Owner writes data before its entry; appends `synced{dataLen}` only after a data fsync returned.
- Readers never lock, never modify a file. Only the holder of a segment's `flock` truncates, seals, or rewrites its sidecar.
- A store holds a **shared** `flock` on the directory for its life (keeps pre-change binaries, which take it exclusively, out).
- Segment ids: one namespace, highest-plus-one, claimed with `O_EXCL`, retry on collision.
- Adoption before creation; largest unlocked active segment first. `Close` does not seal.
- Public lookups (`Get`, `GetRecord`, `Has`, `StoredSize`, `Missing`, `SortByLocation`) refresh the view on a miss — `Missing` and `SortByLocation` at most once per call. The write path's duplicate check never refreshes.
- `gc.lock`: shared = any exported write and the collector's `BeginWrite`/`PrepareRef`; exclusive = a whole GC cycle, and `Wipe`. The file's first 8 bytes are a big-endian generation counter, incremented by the exclusive holder before it lets go; a store that takes the shared lock and sees a new generation refreshes first.
- `flock` is never converted between modes: exclusive is taken and dropped only with no local span in flight.
- Lock waits poll with `LOCK_NB` (1 ms doubling to 50 ms) so they honour a context or a closing store.
- Branch `packstore-multi-process`, stacked on `refstore-sqlite`. Commit per task; do not push until the final review is done.
- Verification commands must preserve exit status (`set -o pipefail`; never `go test | tail` bare).
- Commit message style is `pkg: summary`. End every commit message with:

  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_019kMb3Ckqnion4SKma81cfN
  ```

## Review Focus

1. **A writer killed between the data fsync and the index append** (`Put` acknowledged, entry missing): every later open, by any process, must still find the record. Task 2, `TestAcknowledgedRecordMissingFromIndexIsFound`.
2. **An index entry that outlived its data** (power loss with sync off, or a torn data tail): it must never resolve to bytes of a *later* record after the owner truncates and appends again. Task 2, `TestStaleEntriesNeverPointIntoNewRecords`.
3. **A reader open for a long time** (pager) while segments are sealed, compacted away and replaced: reads keep working from old mappings, a miss finds the new home, and the reader never blocks adoption. Task 3, `TestLongLivedReaderSurvivesSealAndCompaction`.
4. **A stale writer's duplicate hit against a reaped segment** after another process's cycle: must refresh first. Task 4, `TestStaleWriterRefreshesBeforeDedup`, reproduced failing with the generation check disabled.
5. **Pre-change binaries on a live store**: the old exclusive directory lock must fail while a new store is open, and a new open must fail while the old lock is held. Task 3, `TestOldExclusiveDirectoryLockIsRefused`.

## File Structure

| File | Responsibility |
| --- | --- |
| `packstore/sidecar.go` (new) | record encode/decode, `sidecarWriter`, `readSidecar` |
| `packstore/recover.go` | `recoverActive(data, idx, owner)` by the reading rules; `scanActive` becomes the tail scanner with a start offset |
| `packstore/active.go` (new) | `ownedSegment` lifecycle: adopt, create (id allocation), seal hook, release; `foreignActive` views |
| `packstore/view.go` (new) | `listSegments`, building and refreshing the view, retiring vanished mappings |
| `packstore/gate.go` (new) | `gate`: shared/exclusive `flock` per store with local counting, generation counter |
| `packstore/packstore.go`, `gc.go`, `compact.go`, `markset.go`, `missing.go`, `repair.go` | integration |
| `gc/collector.go`, `gc/cycle.go` | cycle under the exclusive gate; spans under the shared one |
| `architecture/packstore.md` (new), `architecture/mark-sweep-gc.md`, `README.md`, `CLAUDE.md`, `specs/gc.qnt` (comment only) | docs |

---

### Task 1: the sidecar format

**Files:** create `packstore/sidecar.go`, `packstore/sidecar_test.go`.

**Interfaces (produces):**

```go
const sidecarSuffix = ".idx"            // appended to the active file's name
const sidecarRecSize = 56
var sidecarMagic = []byte("AMBERIX\x01")

type sidecarRec struct {                 // kind entry: all fields; kind synced: off = durable data length
	kind  byte
	k     key.Key
	off   uint64
	flags byte
	ulen  uint32
	slen  uint32
}
func (r sidecarRec) encode() [sidecarRecSize]byte
func decodeSidecarRec(b []byte) (sidecarRec, bool)      // false: bad CRC, bad kind, nonzero reserved

// readSidecar parses b (a whole sidecar file, or the part from a record
// boundary with atStart=false). It returns the records up to the first
// invalid or partial one and the number of bytes they cover.
func readSidecar(b []byte, atStart bool) (recs []sidecarRec, valid int)

type sidecarWriter struct{ f *os.File; off int64; broken bool }
func createSidecar(path string) (*sidecarWriter, error)             // truncates, writes the magic
func openSidecarAt(path string, valid int64) (*sidecarWriter, error) // truncates to valid, appends after
func (w *sidecarWriter) entry(k key.Key, loc activeLoc) // failures set broken and are not returned
func (w *sidecarWriter) synced(dataLen int64)
func (w *sidecarWriter) close() error
```

A failed sidecar write must not fail the store write: the data is intact and recovery copes with a short index. After the first failure the writer stops writing (`broken`), so a later record can never follow a hole.

**Tests (`sidecar_test.go`):**
- `TestSidecarRecordRoundTrip`: entry and synced survive encode/decode; every single-bit flip in a record makes `decodeSidecarRec` report false.
- `TestReadSidecarStopsAtFirstBadRecord`: truncation at every byte of a three-record file yields the whole records before the cut and the right `valid`; a corrupted middle record hides everything after it.
- `TestReadSidecarRejectsBadMagic`.
- `TestSidecarWriterStopsAfterAFailure`: closing the file under the writer makes the next `entry` a no-op and `broken` true, without panicking.

- [ ] Write the tests, watch them fail, implement, pass, `go vet ./packstore`, commit `packstore: the sidecar index format for active segments`.

---

### Task 2: recover an active segment from its sidecar

**Files:** modify `packstore/recover.go`, `packstore/packstore.go` (append, sync, seal, close, wipe, open); create `packstore/recover_sidecar_test.go`; adjust `packstore/recover_test.go` call sites.

**Interfaces:**

```go
// scanActiveFrom is today's scanActive starting at off (the magic is checked by the caller when off == 0).
func scanActiveFrom(b []byte, off int64, index map[key.Key]activeLoc) (end int64, sealed bool)

type recovered struct {
	index      map[key.Key]activeLoc
	dataEnd    int64 // end of valid data
	sidecarEnd int64 // bytes of the sidecar that agree with dataEnd (0: rewrite it)
	missing    []sidecarRec // records found by verification or tail scan that the sidecar lacks
	sealed     bool  // the data file carries a complete footer
}
// recoverActive applies the spec's reading rules. It never writes.
func recoverActive(dataPath, sidecarPath string) (recovered, error)
```

Reading the data: trusted entries need no data bytes; verification and the tail scan read only from the first untrusted byte on (`ReadAt` into a buffer, not `os.ReadFile` of the whole file). The footer check for a crashed seal reads the fixed-size trailer first and the footer only if the trailer is valid.

The owner (single active segment still, in this task) on open: `recoverActive`, truncate the data to `dataEnd`, `openSidecarAt(sidecarEnd)` (or `createSidecar` plus all entries when `sidecarEnd == 0`), append `missing`. `appendLocked` adds `entry` after the data write; every successful data fsync (`append` with `syncNow`, `syncActive`, `Close`, `PutVerified`) adds `synced{a.size}`. `sealActiveLocked` closes and removes the sidecar after the rename's directory fsync. `Wipe` removes it. Open removes a sidecar whose data file is gone.

**Tests:**
- `TestOpenTrustsSyncedEntries`: write with sync on, close, flip a payload byte of an early record, reopen: open succeeds and `Has` is still true for every key (today's scan would truncate there; `Verify`/scrub is where corruption is reported). Pins "trusted entries are not re-read".
- `TestOpenReadsOnlyTheUntrustedTail`: with a counting `ReadAt` seam or by file size arithmetic: after a clean close, recovery reads zero data bytes beyond the trailer probe.
- `TestAcknowledgedRecordMissingFromIndexIsFound` (Review Focus 1): truncate the sidecar by one entry and one marker after a synced `Put`; reopen; the record is found and the sidecar is whole again.
- `TestUnsyncedEntriesAreVerified`: sync off, no clean close (copy the files mid-run), corrupt the last record's payload: reopen drops exactly that record, keeps the earlier ones, truncates the data.
- `TestStaleEntriesNeverPointIntoNewRecords` (Review Focus 2): after the previous scenario, append new records and reopen twice; every key resolves to its own bytes.
- `TestMissingSidecarFallsBackToFullScan`, `TestGarbageSidecarFallsBackToFullScan`.
- `TestSealRemovesSidecar`, `TestOrphanSidecarIsRemoved`, `TestWipeRemovesSidecar`.
- `TestCrashBetweenFooterAndRename` (existing) still passes with a sidecar present.
- All existing `recover_test.go` cases pass against `scanActiveFrom`.

- [ ] Tests first, implement, `go test ./packstore`, `go test -race ./packstore`, vet, commit `packstore: open active segments from their sidecar index`.
- [ ] Re-run the throwaway open-cost measurement (64 MiB, 256 MiB, 1 GiB of 4 KiB objects, warm cache, best of 3; outside the repo, removed afterwards) and keep the numbers for Task 6.

---

### Task 3: many active segments, ownership, and views that refresh

**Files:** create `packstore/active.go`, `packstore/view.go`, `packstore/multi_test.go`, `packstore/process_test.go`; modify `packstore/packstore.go`, `gc.go`, `compact.go`, `markset.go`, `missing.go`, `repair.go`, `packstore_test.go`.

**Interfaces:**

```go
type foreignActive struct {            // another writer's segment, read-only
	id         uint64
	path       string
	f          *os.File
	index      map[key.Key]activeLoc
	dataEnd    int64
	sidecarEnd int64
}
// Store gains: foreign []*foreignActive (guarded by mu), refreshMu sync.Mutex, refreshSeq uint64.

func (s *Store) refresh() error                       // re-list; map new sealed; retire vanished; extend/add/drop foreign actives
func (s *Store) lookup(k key.Key) (where, bool)       // own active, foreign actives, sealed newest-first; caller holds mu
func (s *Store) ensureActiveLocked() error            // adopt (largest unlocked first) or create; called under appendMu
func allocateSegment(dir string, after uint64) (id uint64, f *os.File, err error) // O_EXCL, retry on EEXIST
```

`Open`: shared `flock` on the directory (non-blocking; failure means a pre-change binary holds it), then `refresh()` builds the first view; nothing else is locked and no file is modified. The first write calls `ensureActiveLocked`. `Close` releases the owned segment by closing its file after the final fsync and `synced` marker.

Public lookups: on a miss, drop `mu`, `refresh()` (coalesced: a caller that waited on `refreshMu` and sees `refreshSeq` moved skips its own), retry once. `Missing` and `SortByLocation` refresh once per call when they saw any miss. `hasLocal` (the write path) never refreshes.

Every walk that today visits "the active segment" visits the owned one and the foreign ones: `NewMarkSet`, `HasOutside`, `Liveness`, `locateLocked`, `StoredSize`, `GetRecord`, `copyLive`'s `survivorHas`. `PutVerified` repairs only segments this store can write: its own active or sealed ones; a damaged record in a foreign active segment is reported, not repaired.

Retiring a vanished sealed segment reuses `Remove`'s discipline (detach under `mu`, unmap after scrubs).

**Tests (`multi_test.go`; two stores on one directory stand in for two processes):**
- `TestTwoStoresOpenOneDirectory` (replaces `TestSecondOpenFails`).
- `TestManyActiveSegmentsOpen` (replaces `TestMultipleActiveFilesFailOpen`).
- `TestOldExclusiveDirectoryLockIsRefused` (Review Focus 5): with a store open, `flock(LOCK_EX|LOCK_NB)` on the directory fails; with that lock held by the test, `Open` fails with a message naming an older release.
- `TestSecondWriterCreatesItsOwnSegment`, `TestWriterAdoptsTheLargestUnlockedSegment`, `TestSerialWritersFillOneSegment` (three open-write-close rounds leave one active file).
- `TestConcurrentCreatorsGetDistinctIDs` (8 stores, first write at once).
- `TestAdopterCompletesACrashedSeal`.
- `TestReaderSeesWhatAnotherWroteAfterItOpened` (own miss, refresh, hit), for a record in a foreign active segment and for one in a segment sealed since.
- `TestReaderHoldsNoLock`: a reader open on a store does not stop a writer from adopting the only active segment.
- `TestLongLivedReaderSurvivesSealAndCompaction` (Review Focus 3).
- `TestDedupDoesNotRefresh`: counts refreshes across a `WriteBatch` of new keys: zero.
- `TestMissingRefreshesOnce`.
- `process_test.go`: `TestSecondProcessWritesAreVisible` (re-exec helper as in `refstore`).

- [ ] Tests first, implement, full `go test ./...` and race over `packstore`, `gc`; commit `packstore: many active segments; writers adopt, readers refresh on a miss`.

---

### Task 4: the cross-process gate and the view generation

**Files:** create `packstore/gate.go`, `packstore/gate_test.go`; modify `packstore/packstore.go`, `gc.go`, `compact.go`.

**Interfaces:**

```go
// BeginWrite marks a write span: it holds gc.lock shared (unless this store
// holds it exclusively) until done is called. Every exported write takes one
// itself; callers bracket larger spans, such as a completeness walk followed
// by a reference put. If the view generation moved, the view is refreshed
// before BeginWrite returns.
func (s *Store) BeginWrite() (done func(), err error)

// BeginSweep takes gc.lock exclusively for a GC cycle or a wipe: it waits
// out local spans, then other processes', refreshes the view, and returns.
// done bumps the generation, waits out local spans again and unlocks.
func (s *Store) BeginSweep(ctx context.Context) (done func(), err error)
```

`gate` state per store: `shared int`, `exclusive bool`, `blocked bool` (new local spans wait), a `sync.Cond`. Transitions: first local span with `!exclusive` polls `LOCK_SH`; last one unlocks. `BeginSweep`: set `blocked`, wait `shared == 0`, poll `LOCK_EX`, set `exclusive`, clear `blocked`. Its `done`: set `blocked`, wait `shared == 0`, write generation+1, unlock, clear both. `Compact` additionally blocks new local spans and waits for the ones in flight before it takes `appendMu`, which closes the long-standing in-process exposure of writes that bypass the collector.

`Wipe` runs inside `BeginSweep`.

**Tests (`gate_test.go`):**
- `TestWriteSpanBlocksAForeignSweep` and `TestSweepBlocksAForeignWriteSpan` (two stores; bounded waits asserted with channels, not sleeps).
- `TestLocalSpansRunDuringALocalSweepLock`: with `BeginSweep` held, a `Put` on the same store completes.
- `TestNestedSpansCount`.
- `TestBeginSweepHonoursContext`.
- `TestGenerationMovesOnSweepAndWipe`.
- `TestStaleWriterRefreshesBeforeDedup` (Review Focus 4): store B maps segment X holding K; store A reaps X with K dead; B then writes K: the record must exist afterwards in a live segment. A subtest disables the generation check through a test hook and shows K lost, so the test is known to bite.
- `TestWipeUnderTheGate`.

- [ ] Tests first, implement, race run, commit `packstore: gc.lock gates writers against a sweep across processes; view generation`.

---

### Task 5: the collector across processes

**Files:** modify `gc/collector.go`, `gc/cycle.go`, `packstore/compact.go`; create `gc/multi_test.go`.

`cycle`: under the first `refLock.Lock()` call `objects.BeginSweep(ctx)`; hold it until the end of the cycle; its `done` runs under `refLock.Lock()` on every exit path (the sweep already holds it; the abort paths take it briefly). `BeginWrite` and `PrepareRef` take `objects.BeginWrite()` inside their `refLock.RLock()`. `Compact` seals the owned active segment and then adopts and seals every unlocked one before it selects victims.

**Tests (`multi_test.go`; two store/refstore/collector triples on one directory):**
- `TestCycleWaitsForAForeignWriteSpan`, `TestForeignPrepareRefWaitsForACycle`.
- `TestCollectWhileAnotherStoreIngests`: A ingests trees and publishes references, deletes some; B runs cycles at `--garbage 0`, grace 0; afterwards every surviving reference is complete from both views and the deleted data is gone.
- `TestIdleForeignActiveSegmentIsCollected`, `TestOwnedForeignActiveSegmentIsLeftAlone`.
- `TestLocalIngestRunsThroughTheMark` (existing barrier behaviour, now with the gate in place).
- Existing `gc` suite passes unchanged.

- [ ] Tests first, implement, race run over `packstore`, `refstore`, `gc`, commit `gc: a cycle holds the cross-process gate; idle active segments are sealed and collected`.

---

### Task 6: documentation and measurements

- `architecture/packstore.md` (new): directory layout, the sidecar format and its reading and writing rules, ownership and adoption, views and refresh, `gc.lock` and the generation, compatibility. Written so that `core-rs` can implement it.
- `architecture/mark-sweep-gc.md`: "Across processes" section (quiesce for foreign writers, barrier for local ones; why `flock` is never converted).
- `specs/gc.qnt`: header comment noting that foreign writers are held to the *quiesce* policy the model already checks; no model change.
- `README.md` package table and the store-layout paragraph; `CLAUDE.md`: open cost before and after, and the multi-process note under Benchmarks.
- [ ] Full verification with exit status preserved: `go test ./...`, `go test -race ./packstore ./refstore ./gc`, `go vet ./...`, `gofmt -l .`, `go test ./cmd/amber-bench`.
- [ ] Commit `docs: the packstore's on-disk layout and multi-process protocol`.

---

## Amendments during execution

### Task 2 and 3: recovery belongs to the owner, so it moved out of `Open`

Task 2 repaired an active segment at `Open`. Task 3 makes `Open` lock and
modify nothing, so truncating a torn tail, finishing a crashed seal and
bringing a sidecar in line now happen when a writer **adopts** the segment. A
store that only reads serves the segment as it finds it: a crashed seal is
mapped as the sealed segment it is, under its active name. Four tests that
asserted the repair right after `Open` now write first; one of them predates
this work, `TestCrashBetweenFooterAndRename`, which also expects one active
segment afterwards (the write that finished the seal needed somewhere to go).

### Task 3

- **Segment identity is (id, file), not id.** A repair replaces a sealed
  segment under its name, and an id can come back once compaction removed the
  highest segment. Views compare `os.SameFile` against the listing and remap.
- **Creating a segment.** The id is claimed by creating
  `<id>.seg.active.tmp` exclusively; the file is locked, the final names are
  checked (the temporary name is free again once its creator renamed it, so
  holding it proves nothing), the header is written and synced, and only then
  is it renamed to the name adopters look for. Nobody ever sees a segment that
  is not ready and owned. A later creator removes unlocked leftovers.
- **Reads from another writer's segment check the record.** The key and
  stored length at the indexed offset must match; otherwise the view is
  rebuilt and, failing again, the read returns `ErrCorrupt` rather than
  another object's bytes. `TestForeignReadNeverReturnsTheWrongRecord`.
- **A refresh that raced this store's own seal, adoption or repair only
  adds.** Such changes bump `structEpoch`; the listing predates them, so
  dropping is left to the next refresh.
- **`Missing` refreshes once, up front**, because misses are what it expects.
  **`SortByLocation` does not refresh**: it is an ordering hint, and the reads
  that follow refresh for themselves.
- **`PutVerified`** walks a snapshot of the sealed segments registered as a
  scrub (a refresh can now retire a mapping without `appendMu`), takes it
  again after it sealed the active segment, and replaces a repaired segment
  by identity rather than by position. It leaves other writers' active
  segments alone: a damaged copy there is repaired once the segment is sealed.
- **`Wipe` refuses while another store owns an active segment**, deleting
  nothing: that writer would go on appending to a file that is gone.
- The scan of an active segment became incremental (`segmentScan.advance`), so
  that a reader's view follows a live writer by reading only what was
  appended. The sidecar is read before the data's length, since a record is
  written before its entry.

### Task 3, found later: a creator must re-check its temporary file

Under the race detector's timing, a store clearing away crash leftovers locked
and removed another store's brand-new temporary segment in the instant before
its creator locked it; the creator then locked an unlinked file and its rename
failed. Creation now checks, like adoption, that the locked file is still at
its path. Its own commit.

### Task 4

- The gate's state machine has one more flag than planned, `busy`: while one
  goroutine takes or drops the file lock, the others wait, so that the first
  span's view refresh (after a generation change) is finished before any
  span reaches a duplicate check.
- `Compact`, `Wipe` and `Remove` take the exclusive gate themselves and find
  it held inside a collector's `BeginSweep` (the depth is counted), so none
  of them can be run unsafely by a caller that forgot the gate.

### Task 5

- **A sweep yields.** With the yield disabled, a writer polling from another
  store did not get the lock across 200 back-to-back sweeps: the lock is free
  for microseconds between two sweeps. A store that just swept now leaves the
  lock alone for 100 ms (two polls' worth) before `BeginSweep` takes it
  again. `TestBackToBackSweepsYieldToAWaitingWriter`, seen failing without it.
- `TestIdleForeignActiveSegmentIsCollected` was seen failing with idle
  sealing disabled. `TestForeignPrepareRefWaitsForACycle` failed before the
  change, and showed a test bug on the way: its helper cycle stayed parked on
  a channel when the assertion failed, and the collector's `Close` then
  waited for it until the ten-minute test timeout. It now releases the helper
  on every path. Verification runs carry a short `-timeout` since then.
- The concurrent test brackets each ingest and its reference put in one write
  span. Objects another process wrote and has not referenced yet are
  protected from a foreign cycle by the grace period alone, which the test
  sets to a nanosecond; the span is what a writer uses when it wants more.
  `amber-store ingest --ref` now does the same.

### After Task 5: a lookup miss must not list the directory every time

Measured against the branch before this work (dev Mac, 512 MiB of 4 KiB
objects): with every miss listing the directory, `Has` on an absent key went
from 0.04 / 0.27 / 2.35 us to 33 / 113 / 760 us at 8 / 64 / 516 sealed
segments. A miss now first checks that the directory's modification time is
unchanged since the last listing and that no active segment it reads has
changed size, and lists only otherwise; the time is trusted only if it was two
seconds old at the listing (git's racy-timestamp rule), so coarse filesystem
timestamps cannot hide a new segment. `TestMissesDoNotListAnUnchangedDirectory`,
`TestFastPathStillSeesOtherStoresWrites`. Its own commit. A miss that finds
the view current does not search it a second time either. Steady state, with
one foreign active segment: 2.5 / 2.8 / 5.6 us.

Not done, a known lever: adoption runs the recovery a second time although
the store already holds a view of the segment (open plus first write costs
about twice an open). Taking over the view's index when the segment is clean
would halve that; at jj scale it is about a millisecond.
