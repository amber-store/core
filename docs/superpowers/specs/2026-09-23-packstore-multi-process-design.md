# Packstore: sidecar index for active segments, and multi-process access

**Date:** 2026-09-23
**Status:** directed by the user on 2026-09-23 ("add sidecar index for the open
packs and add multi-writer support for it"), after the design discussion of the
same day (per-writer active segments with adoption, lock-free readers, refresh
on a miss). Stage 2 of 3 on the way to a jj backend. Branch
`packstore-multi-process`, stacked on `refstore-sqlite`.
**Repos:** `amber-store/core` (this change); `amber-store/core-rs` follows from
`architecture/packstore.md`, which this change adds.

## Background

Three properties of the packstore assume one process owns the store:

- `Open` takes an exclusive, non-blocking `flock` on the directory for the
  store's whole life. A second process fails at open, readers included. jj
  runs a process per command and overlapping commands are normal (a prompt, an
  editor integration, a pager left open on the log).
- There is exactly one active segment; its index lives only in memory, so
  every `Open` reads and parses the whole active file to rebuild it. Measured
  on the development Mac, warm cache, 4 KiB objects: 24 ms at 64 MiB, 45 ms
  at 256 MiB, 197 ms at 1 GiB — per command, and a cold cache reads the whole
  file from disk. A second `.seg.active` file is treated as corruption.
- What makes GC safe against writers — the write barrier's grey set and the
  collector's reference lock — lives in process memory.

Stage 1 made the reference store multi-process; this stage does the same for
objects.

## Goal

Any number of processes may hold a store open and write to it at once. No
reader ever takes a lock or waits. Opening a store costs the same whatever the
size of its active segments. A GC cycle stays safe when other processes write.
The sealed-segment format does not change; stores written by earlier releases
open as they are.

## Non-goals

- A combined index across sealed segments. Measured: a lookup miss costs about
  5 ns per sealed segment and opening one about 15 µs, so at jj scale it buys
  nothing; it is a lever for GC marks over hundreds of packs.
- Changing when writes are fsynced (one sync per put stays; syncing once per
  commit is the larger write-path win and a separate change).
- Letting writers in *other* processes run during a GC cycle. They wait for
  it (see "GC across processes"); writers in the collector's own process keep
  running through the mark behind the write barrier, as today.
- Network filesystems, and platforms without `flock`. As today.
- The `core-rs` port.

## On-disk layout

```
<packstore>/
  0000000000000007.seg              sealed segment: unchanged
  0000000000000009.seg.active       an active segment: unchanged, but several may exist
  0000000000000009.seg.active.idx   its sidecar index (new)
  gc.lock                           the cross-process gate and the view generation (new)
```

Segment ids stay one namespace shared by sealed and active files, allocated
as "highest id in the directory plus one" and claimed with `O_EXCL`; a
collision (another process took the id) re-lists and retries.

## The sidecar index

`<id>.seg.active.idx` is an append-only log that mirrors what the owner holds
in memory. It starts with the 8-byte magic `AMBERIX\x01` and continues with
fixed-size 56-byte records, big-endian:

```
offset  size  field
0       1     kind     0x01 entry, 0x02 synced
1       32    key      entry: the record's key            synced: zero
33      8     off      entry: offset of the record header synced: data length known durable
41      1     flags    entry: the record's flags byte     synced: zero
42      4     ulen     entry: uncompressed payload length synced: zero
46      4     slen     entry: stored payload length       synced: zero
50      2     zero     reserved
52      4     crc      CRC-32C of bytes [0:52]
```

Writing rules, for the process that owns the segment:

1. A record is written to the data file first; its **entry** is appended to
   the index only after that write returned.
2. After an fsync of the data file returned, a **synced** record carrying the
   data length that fsync covered is appended. `Put` with sync on therefore
   appends an entry and a synced record; a batch appends its entries as it
   goes and one synced record per fsync; `Close` fsyncs and appends one.
3. The index file itself is never fsynced. It is a cache: losing its tail
   costs a longer scan, never data.

Reading rules, for any process:

1. Read records while the CRC is valid; stop at the first bad or partial one.
   Let *S* be the data length of the last synced record (the header length if
   there is none).
2. An entry whose record ends at or before *S* is **trusted**: the data was
   durable before the entry was written.
3. Entries beyond *S*, in order, are **verified** by parsing their record
   from the data file (framing and CRC). The first failure ends the valid
   data.
4. Past the last verified entry the data file is **tail-scanned** as today,
   record by record, until the first invalid byte. This finds records that
   were written — possibly acknowledged — just before a crash that kept their
   entries from being written.

So after a clean close, or while the owner runs with sync on, opening reads
the small index and nothing else; after a crash it verifies only what was not
known durable. A missing or unreadable index degrades to today's full scan.
An index whose data file is gone (a crash between a seal's rename and the
index's removal) is deleted by whoever notices.

A reader never modifies either file. Only the owner — the process holding the
segment's lock — truncates a torn tail or rewrites the index, and it does so
when it takes ownership.

## Ownership, adoption and sealing

A writer owns an active segment by holding an exclusive `flock` on its data
file, from the moment it takes the segment until `Close` or the seal. Nothing
is locked at `Open`: a process that only reads never owns anything, so a pager
left open cannot keep a segment from the next writer.

At its first write a store **adopts** before it creates: it lists the active
segments, largest first, and tries a non-blocking lock on each. On success it
recovers the segment with the owner's rights — completing a crashed seal,
truncating a torn tail, bringing the index in line — and appends to it. Only
when every active segment is locked by a live process does it create a new
one. Serial commands therefore keep filling one segment until it reaches the
segment size, exactly as today; the number of active segments is bounded by
the peak number of simultaneous writers and drifts back to one as they fill.

Sealing is unchanged (footer, fsync, rename, directory fsync) and then removes
the sidecar. `Close` does not seal.

## Reading other processes' segments

At `Open` a store builds a read-only view of every segment: sealed ones are
mapped as today, active ones are indexed from their sidecars by the reading
rules above, with no lock and no modification.

The view goes stale as others write. **A miss refreshes it**: `Get`, `Has`
and their relatives, on not finding a key, re-list the directory and

- map sealed segments that appeared (including active ones that were sealed),
- drop segments that vanished (compaction victims, whose mappings stayed
  valid until now),
- read what was appended to the known sidecars, and index active segments
  that appeared,

then look once more. The write path's own duplicate check does not refresh:
a duplicate it fails to see costs a redundant record, which compaction folds,
while a directory listing per new object would cost every ingest dearly.

## GC across processes

A cycle must not lose an object that a writer in another process just relied
on: written, or skipped as a duplicate of a record the cycle is about to reap.
In one process the write barrier and the reference lock prevent that
(`specs/gc.qnt`, policy *barrier*). Across processes this change uses the
spec's simpler safe policy, *quiesce*, through one lock file, `gc.lock`:

- **Shared**, held by a store while any write span or reference publication
  is in flight: every exported write (`Put`, `WriteBatch`, `WriteParallel`,
  `PutVerified`, `AppendRecord`) and the collector's `BeginWrite` and
  `PrepareRef` spans. Counted per store, so nested spans cost nothing.
- **Exclusive**, held by the collector for a whole cycle, from before the
  roots snapshot until after the sweep, and by `Wipe`.

So while a cycle runs, writers and reference puts in *other* processes wait;
readers never do. Writers in the collector's *own* process do not take the
file lock while their process holds it exclusively: they run through the mark
behind the barrier, and the sweep excludes them with the reference lock, all
as today.

Taking and dropping the exclusive lock happens only at moments when the
collector already holds its reference lock exclusively, so no local span is
in flight: `flock` cannot convert between shared and exclusive atomically,
and this way it never has to. Before the exclusive lock is dropped, the store
also waits out local writes that bypassed the collector.

**The view generation.** `gc.lock` holds an 8-byte counter. Whoever held the
lock exclusively increments it before letting go. A store reads it each time
it takes the shared lock; if it moved, the store refreshes its view before
doing anything else. Without this a writer could skip an object as a
duplicate of a record in a segment it still has mapped but which the sweep
already deleted.

**What a cycle sees.** After taking the exclusive lock the collector refreshes
its view, which is then complete and stable: nobody else can write. The mark
set covers every active segment it can see, not only its own. Compaction
seals the store's own active segment, as today, and also every active segment
it can adopt (idle ones), so that a small store whose segments never fill
still gets collected; segments owned by a live process are never victims.

Two cycles cannot overlap across processes, because both need the exclusive
lock; a second one waits or, with a cancelled context, gives up.

## Compatibility

Sealed segments are unchanged, and a store written by an earlier release opens
as is: its single active segment has no sidecar, so its first open scans it
once, and the writer that adopts it writes the sidecar.

To keep an earlier release from opening a store at the same time — it would
take the old exclusive directory lock, assume it owns the one active segment
and truncate or seal it under a live writer — every store now holds a
**shared** `flock` on the directory for its whole life. The old exclusive,
non-blocking lock fails against it, and a new store fails to open while an
old binary holds the directory. An old binary opening a store that has two
active segments fails on its "at most one" check, which is safe.

`TestSecondOpenFails` and `TestMultipleActiveFilesFailOpen` pin the old
behaviour and are replaced by tests of the new one.

## Code layout

| File | Responsibility |
| --- | --- |
| `packstore/sidecar.go` (new) | sidecar format: encode, append, load by the reading rules |
| `packstore/recover.go` | recovery of an active segment from sidecar plus data, read-only or as owner |
| `packstore/active.go` (new) | ownership: adopt, create with id allocation, release; foreign active views |
| `packstore/view.go` (new) | directory listing into a view; refresh on a miss and on a generation change |
| `packstore/gate.go` (new) | `gc.lock`: shared/exclusive modes per store, the generation counter |
| `packstore/packstore.go` | `Open` without the exclusive lock; reads over own and foreign segments; writes through the gate |
| `packstore/compact.go`, `markset.go`, `gc.go` | foreign active segments in the mark set and in `HasOutside`; sealing idle segments; `Wipe`/`Remove` under the gate |
| `gc/collector.go`, `gc/cycle.go` | the cycle holds the exclusive gate; `BeginWrite`/`PrepareRef` hold the shared one |
| `cmd/amber-store/commit.go` | `commit create` brackets its write with `BeginWrite` |
| `architecture/packstore.md` (new), `architecture/mark-sweep-gc.md`, `README.md`, `CLAUDE.md` | the on-disk layout and the multi-process protocol; measured open cost |

## Testing

Two stores opened on one directory behave like two processes, because `flock`
belongs to the open file, so most of the protocol is tested in one process,
deterministically; one test per mechanism also runs a real second process.

- Sidecar: round trip; every truncation point of the index; an index ahead of
  its data (entries beyond *S* pointing at garbage); a missing index; records
  present in the data but absent from the index; a corrupt middle entry; the
  seal removes the index; an orphan index is cleaned up.
- Open cost: opening a store with a large, cleanly closed active segment
  reads the index and not the data (asserted by making the data unreadable
  beyond what the rules allow, not by timing).
- Ownership: a second store creates its own segment while the first holds
  one; after the first closes, a third adopts the larger one; ids never
  collide when several stores create segments at once; a crashed seal is
  completed by the adopter.
- Reading: a store sees what another wrote after it opened (own miss, then
  refresh); sees a segment that was sealed or compacted away by another;
  a reader holds no lock (a writer adopts while the reader is open).
- Gate: a write span in one store blocks a cycle in another and the reverse;
  local spans run during a local cycle; the generation makes a stale writer
  refresh before its duplicate check; the scenario the generation exists for —
  a duplicate hit against a reaped segment — is reproduced without it and
  fixed with it.
- GC end to end with two stores: one ingests and deletes, the other collects;
  every kept reference stays complete; an idle foreign active segment is
  sealed and collected; one owned by a live store is left alone.
- A second process: writes are visible to the first; a cycle in one waits for
  a write span in the other.
- Compatibility: a directory held by a new store refuses the old exclusive
  lock; a store written by the old layout (one active segment, no sidecar)
  opens, reads, and gains a sidecar on its first write.
- The existing packstore, gc, ingest and CLI suites pass, with the two
  replaced tests noted above; `go test -race` over `packstore`, `refstore`
  and `gc`.
