# amber-store uses core: parity and de-duplication

**Date:** 2026-09-07
**Repos:** `amber-store/core` (phase 1), `amber-store/amber-store` (phase 2)

## Background

`amber-store/amber-store` (the daemon, server, client, remotes, identity
and FUSE code) carries its own copies of 13 of core's 14 packages:
`amberignore`, `amberpack`, `cborx`, `chunkers`, `fstree`, `gc`, `inbox`,
`key`, `packstore`, `reference`, `refstore`, `tarexport`, `tarextract`.
Core was split out of it on 2026-07-20; since then Mic92's August fixes were
applied on both sides, and each side grew a little the other lacks.

Measured on 2026-09-07 (import paths normalized):

- Eight copies are byte-identical to core: `amberignore`, `cborx`,
  `chunkers`, `fstree`, `key`, `refstore`, `tarexport`, `tarextract`.
- Core is ahead in `amberpack` (`Reader.Records`, `RawRecord`) and
  `packstore` (`Object.Record`, `prepare.go`, the nil-record check in
  `AppendRecord`). All additive; amber-store's callers compile against
  them unchanged.
- amber-store is ahead in three places:
  - `gc.Collector.BeginWrite() (done func())`: a write-span gate that holds
    the reference lock shared, so a sweep waits out in-flight writes and a
    write stalls during a sweep (never during the mark). Used by the daemon,
    remote sync and, through the inbox gate, the inbox drain. Two tests.
  - `gc.Status` takes `cycleMu` for the advisory mark so a sweep cannot
    munmap a victim under the walk.
  - `inbox.Option`, `inbox.WithGate(gate func() (done func())) Option`, and
    `Open(dir, store, workers, log, opts ...Option)`: the drain brackets
    `WriteParallel` with the gate.
  - `reference.DecodeVerified(raw) (Reference, error)`: decodes a record and
    verifies its SSH signature through amber-store's `sshsign` package
    (614 lines; hiddeco/sshsig, x/crypto/ssh, ssh-agent, x/term).

A throwaway compile of amber-store against core v0.0.4 failed in exactly five
places: three `reference.DecodeVerified` calls in `embedded/embedded.go`,
two `coll.BeginWrite()` calls in `daemon/`; `inbox.WithGate` is used once in
`cmd/amber-store/serve.go`. Nothing else in amber-store's non-shared code
depends on anything core lacks.

Downstream: no Go module in jobs-build, amber-store, fables-for-robots,
netice9 or draganm depends on `amber-store/amber-store` except the old
`draganm/jobs`, pinned to a July commit of `draganm/amber-store`, which is
unaffected.

## Goal

amber-store imports `github.com/amber-store/core` and carries no copy of its
packages. The amber-store binaries behave exactly as before.

## Non-goals

- The `amber-store` CLI's `ingest` command is its own pack-writing
  implementation (writes an amberpack to a file), not a copy of core's
  `ingest` package; it stays.
- amber-store's `architecture/` and `specs/` docs are a superset of core's
  with their own daemon, FUSE, remote and generational-GC documents; they
  stay as they are.
- No amber-store release; it has not tagged since v0.0.3 (July).
- Moving `sshsign` into core. Core is a local store with no auth; signature
  verification stays in amber-store.

## Phase 1: core parity (release v0.0.5)

One PR on branch `amber-store-parity`.

`gc`:
- Add `BeginWrite` to `Collector`, verbatim from amber-store, after
  `ReleaseRef`.
- `Status` locks `cycleMu` for its duration, verbatim.
- Port `TestBeginWriteDedupHitSurvivesCycle` and
  `TestStatusSerializesWithCycle` into `gc/collector_test.go` (the helpers
  `newTestStore`, `openCollector`, `storeTree`, `putTestRef`,
  `backdatePacks`, `Collector.midMark` already exist there).

`inbox`:
- `gate func() func()` field, `type Option func(*Inbox)`, `WithGate`,
  variadic `opts ...Option` on `Open`, applied after construction; the drain
  acquires the gate around `WriteParallel` when set. Verbatim.
- New test `TestWithGateBracketsDrain`: an inbox opened with a gate that
  counts acquires and releases drains one entry; afterwards both counts are
  1 and the release happened after the write.

Release: merge, tag `v0.0.5`, notes.

## Phase 2: amber-store imports core

One PR on branch `use-core`.

- `git rm -r` the 13 package directories.
- Rewrite `"github.com/amber-store/amber-store/<pkg>` to
  `"github.com/amber-store/core/<pkg>` for those 13 package names across the
  repository (imports only; the module's own path is untouched).
- `sshsign/verifyref.go`: `func DecodeVerifiedReference(raw []byte)
  (reference.Reference, error)`, the body of the old `DecodeVerified`, with
  the old test moved to `sshsign/verifyref_test.go` as an external test.
  The three callers in `embedded/embedded.go` call
  `sshsign.DecodeVerifiedReference`. `sshsign` does not import `reference`
  today and core's `reference` does not import `sshsign`, so no cycle.
- `go get github.com/amber-store/core@v0.0.5`, `go mod tidy`; direct
  dependencies only the copies used (pebble, xorfilter, cdc-chunkers, cbor,
  blake3, klauspost/compress) become indirect or drop.
- Verify: `gofmt -l`, `go build ./...`, `go vet ./...`, `go test ./...`.
- Merge; fast-forward the local checkout `~/draganm/amber-store`.

## Risk

Low. The compile spike proved the API surface; the ported tests pin the gate
and the Status serialization; amber-store's own suite runs against core's
packages before the merge. Rollback is `git revert` of one merge on either
side; core v0.0.5 is additive over v0.0.4.

## Order

1. Core PR → merge → `v0.0.5`.
2. amber-store PR pinned to `v0.0.5` → merge → pull local checkout.
