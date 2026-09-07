# amber-store uses core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the three things only amber-store's copy of core had into core, release v0.0.5, then make amber-store delete its 13 copied packages and import core.

**Architecture:** Phase 1 (Tasks 1–3) is in `amber-store/core` on branch `amber-store-parity`: `gc.Collector.BeginWrite` and the `Status` serialization with their tests, and `inbox.WithGate`. Phase 2 (Tasks 4–5) is in `amber-store/amber-store` on branch `use-core`: remove the copies, rewrite imports, relocate `DecodeVerified` next to `sshsign`, verify, merge.

**Tech Stack:** Go 1.26.x, `github.com/amber-store/core`, `gh` CLI.

**Spec:** `docs/superpowers/specs/2026-09-07-amber-store-uses-core-design.md`

## Global Constraints

- Core's go.mod says `go 1.26.3`; amber-store's says `go 1.26.3`. The local toolchain is 1.26.3.
- Every commit leaves `gofmt -l $(git ls-files '*.go')` empty and `go build ./... && go vet ./... && go test ./...` green in the repo being changed.
- Ported code is copied verbatim from amber-store's copy at commit `e6232c2` (the analysis clone at `/private/tmp/claude-502/-Users-dragan-jobs-build-amber-store-core/32131b9f-765f-4003-8381-d843b1046270/scratchpad/deps/amber-store`, called `$A` below); when in doubt, diff against it.
- Core work happens in `/Users/dragan/jobs-build/amber-store-core` (called `$C`), currently clean on `main`. amber-store work happens in `$A` on a new branch; the user's checkout `~/draganm/amber-store` is only fast-forwarded at the end.
- Commit messages end with:
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GAJmL5jzzyWvFiikdwFCEn`.

---

## Phase 1: core

### Task 1: gc — BeginWrite and Status serialization

**Files:**
- Modify: `$C/gc/collector.go` (after `ReleaseRef`)
- Modify: `$C/gc/status.go:40-48` (`Status`)
- Test: `$C/gc/collector_test.go`

**Interfaces:**
- Produces: `func (c *Collector) BeginWrite() (done func())`. Task 2's gate type `func() (done func())` matches it; amber-store's daemon calls it directly.

- [ ] **Step 1: Add the failing tests**

Add `"bytes"` as the first entry of the standard-library import group in `gc/collector_test.go`, then append:

```go
// TestBeginWriteDedupHitSurvivesCycle re-puts an object that lives only in
// a condemned pack while a cycle sits between its mark and its sweep: the
// dedup hit writes nothing, so only the write barrier's grey set (mark) and
// the BeginWrite gate (sweep) keep the record from vanishing with its pack.
func TestBeginWriteDedupHitSurvivesCycle(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{})
	rootA, _ := storeTree(t, ts.objects, "live", 30)
	putTestRef(t, c, ts.refs, "live", rootA)
	_, keysB := storeTree(t, ts.objects, "dead", 30) // never referenced
	deadKey := keysB[0]
	want, err := ts.objects.Get(deadKey)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.midMark = func() {
		done := c.BeginWrite()
		defer done()
		if err := ts.objects.Put(deadKey, want); err != nil {
			t.Errorf("dedup-hit put during cycle: %v", err)
		}
	}
	c.mu.Unlock()
	backdatePacks(t, ts)
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) == 0 {
		t.Fatal("cycle reaped nothing; the dedup-hit race was never exercised")
	}
	got, err := ts.objects.Get(deadKey)
	if err != nil {
		t.Fatalf("dedup-hit object lost to the sweep: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("dedup-hit object corrupted by the sweep")
	}
}

// TestStatusSerializesWithCycle pins Status to cycleMu: the advisory mark
// probes sealed footers without scrub registration, so it must never
// overlap a sweep's munmap. Status called mid-cycle returns only after the
// cycle does.
func TestStatusSerializesWithCycle(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{})
	root, _ := storeTree(t, ts.objects, "a", 10)
	putTestRef(t, c, ts.refs, "a", root)

	entered := make(chan struct{})
	release := make(chan struct{})
	c.mu.Lock()
	c.midMark = func() { close(entered); <-release }
	c.mu.Unlock()
	cycleDone := make(chan struct{})
	go func() {
		defer close(cycleDone)
		if _, err := c.Run(context.Background(), -1); err != nil {
			t.Errorf("cycle: %v", err)
		}
	}()
	<-entered
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		if _, err := c.Status(context.Background()); err != nil {
			t.Errorf("status: %v", err)
		}
	}()
	select {
	case <-statusDone:
		t.Error("Status returned while a cycle was mid-mark")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-cycleDone
	<-statusDone
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd $C && go test ./gc/ -run 'TestBeginWriteDedupHitSurvivesCycle|TestStatusSerializesWithCycle' 2>&1 | head -5`
Expected: `c.BeginWrite undefined`.

- [ ] **Step 3: Implement**

In `gc/collector.go`, after the `ReleaseRef` method, add:

```go
// BeginWrite gates one object-write span (an ingest, a pull, an inbox
// drain) against the sweep. The span holds the reference lock shared:
// the sweep waits out in-flight writes, and a write stalls while a sweep
// runs — never during the mark, which writers pass behind the write
// barrier. Without the gate a dedup hit against a record in a condemned
// pack could report success and then lose the record to the pack's
// removal (packstore.Compact must not overlap ingests). Returns the
// release, idempotent; call it when the span's writes are durable.
func (c *Collector) BeginWrite() (done func()) {
	c.refLock.RLock()
	var once sync.Once
	return func() { once.Do(c.refLock.RUnlock) }
}
```

In `gc/status.go`, at the top of `Status`, before `recs, err := c.refs.All()`, add:

```go
	// The advisory mark probes sealed footers without scrub registration,
	// so serialize with cycles: a sweep must not munmap a victim under the
	// walk. A Run racing a long Status reports ErrCycleRunning, exactly as
	// it would against another cycle.
	c.cycleMu.Lock()
	defer c.cycleMu.Unlock()
```

- [ ] **Step 4: Run the package tests**

Run: `cd $C && go test -race ./gc/`
Expected: `ok`.

- [ ] **Step 5: Diff against amber-store**

Run: `diff <(sed 's#amber-store/amber-store/#amber-store/core/#g' $A/gc/collector.go) $C/gc/collector.go; diff <(sed 's#amber-store/amber-store/#amber-store/core/#g' $A/gc/status.go) $C/gc/status.go`
Expected: no output from either.

- [ ] **Step 6: Commit**

```bash
cd $C && git add gc/collector.go gc/status.go gc/collector_test.go && git commit -m "gc: BeginWrite write-span gate; Status serializes with cycles

Ported from amber-store's copy (3ac1ee7, 8a8cf83). A dedup hit inside a
write span no longer races a sweep's pack removal, and Status's
advisory mark cannot overlap a sweep's munmap.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GAJmL5jzzyWvFiikdwFCEn"
```

---

### Task 2: inbox — WithGate

**Files:**
- Modify: `$C/inbox/inbox.go` (struct, `Open`, drain)
- Test: `$C/inbox/inbox_test.go`

**Interfaces:**
- Produces: `type Option func(*Inbox)`, `func WithGate(gate func() (done func())) Option`, `func Open(dir string, store *packstore.Store, workers int, log *slog.Logger, opts ...Option) (*Inbox, error)`.

- [ ] **Step 1: Add the failing test**

Add `"sync"` to the standard-library import group of `inbox/inbox_test.go` (keep the group sorted), then append:

```go
func TestWithGateBracketsDrain(t *testing.T) {
	store := newTestStore(t)
	obj := blobObject(t, []byte("gated entry"))
	var mu sync.Mutex
	acquired, released := 0, 0
	writtenAtRelease := false
	gate := func() func() {
		mu.Lock()
		acquired++
		mu.Unlock()
		return func() {
			has, _ := store.Has(obj.Key)
			mu.Lock()
			released++
			writtenAtRelease = has
			mu.Unlock()
		}
	}
	ib, err := Open(filepath.Join(t.TempDir(), "inbox"), store, 1, nil, WithGate(gate))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { ib.Close() })

	root := obj.Key
	tmp, h, _, err := ib.Stage(Meta{Ref: "r", Root: root[:]}, bytes.NewReader(packBody(t, obj)))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if added, err := ib.Commit(tmp, h, root); err != nil || !added {
		t.Fatalf("Commit: added=%v err=%v", added, err)
	}
	ib.WaitFor(root)

	mu.Lock()
	defer mu.Unlock()
	if acquired != 1 || released != 1 {
		t.Fatalf("gate acquired %d released %d, want 1/1", acquired, released)
	}
	if !writtenAtRelease {
		t.Fatal("gate released before the entry's objects were written")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd $C && go test ./inbox/ -run TestWithGateBracketsDrain 2>&1 | head -3`
Expected: `undefined: WithGate`.

- [ ] **Step 3: Implement**

In `inbox/inbox.go`:

Add a field to `Inbox` after `log     *slog.Logger`:

```go
	log     *slog.Logger
	gate    func() func() // see WithGate
```

Before `func Open(`, add:

```go
// Option tweaks Open.
type Option func(*Inbox)

// WithGate brackets every entry's store write with gate: acquire before the
// WriteParallel, release after. The GC collector's BeginWrite is the intended
// gate — it keeps a drain's dedup hits from racing a sweep's pack removal.
func WithGate(gate func() (done func())) Option {
	return func(ib *Inbox) { ib.gate = gate }
}
```

Change the signature to `func Open(dir string, store *packstore.Store, workers int, log *slog.Logger, opts ...Option) (*Inbox, error)` and, right after the `ib := &Inbox{...}` literal is built (before anything starts goroutines), add:

```go
	for _, o := range opts {
		o(ib)
	}
```

In the drain, replace

```go
	_, werr := ib.store.WriteParallel(seq, packstore.WriteOpts{Verify: true})
```

with

```go
	var werr error
	if ib.gate != nil {
		done := ib.gate()
		_, werr = ib.store.WriteParallel(seq, packstore.WriteOpts{Verify: true})
		done()
	} else {
		_, werr = ib.store.WriteParallel(seq, packstore.WriteOpts{Verify: true})
	}
```

- [ ] **Step 4: Run the tests**

Run: `cd $C && go test -race ./inbox/ && go build ./...`
Expected: `ok`; the CLI still compiles (its `inbox.Open` call passes no options).

- [ ] **Step 5: Diff against amber-store**

Run: `diff <(sed 's#amber-store/amber-store/#amber-store/core/#g' $A/inbox/inbox.go) $C/inbox/inbox.go`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
cd $C && git add inbox/inbox.go inbox/inbox_test.go && git commit -m "inbox: WithGate brackets each drain's store write

Ported from amber-store's copy (8a8cf83). Pass gc.Collector.BeginWrite
so a drain's dedup hits cannot race a sweep's pack removal.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GAJmL5jzzyWvFiikdwFCEn"
```

---

### Task 3: core PR, merge, v0.0.5

- [ ] **Step 1: Verify and push**

```bash
cd $C && gofmt -l $(git ls-files '*.go'); go build ./... && go vet ./... && go test -race ./... && git status --short && git push -u origin amber-store-parity
```

Expected: nothing from gofmt, all packages `ok`, clean tree.

- [ ] **Step 2: PR, merge, release**

```bash
cd $C && gh pr create --title "gc.BeginWrite, Status serialization, inbox.WithGate from amber-store" --body "$(cat <<'EOF'
Ports the three things amber-store's copy of these packages had that core lacked, so amber-store can drop its copies and import core (design: `docs/superpowers/specs/2026-09-07-amber-store-uses-core-design.md`):

* `gc.Collector.BeginWrite() (done func())`: a write-span gate that holds the reference lock shared, so a sweep waits out in-flight writes and a write stalls during a sweep, never during the mark. With amber-store's two tests.
* `gc.Status` serializes with cycles (`cycleMu`), so the advisory mark cannot overlap a sweep's munmap.
* `inbox.Option` / `inbox.WithGate`, a variadic option on `inbox.Open` that brackets each drain's `WriteParallel` with the gate. Existing callers are unchanged.

Verified with `gofmt -l`, `go build ./...`, `go vet ./...`, `go test -race ./...`.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01GAJmL5jzzyWvFiikdwFCEn
EOF
)" && N=$(gh pr list --head amber-store-parity --json number --jq '.[0].number') && gh pr merge "$N" --merge --delete-branch && git checkout main && git pull --ff-only && gh release create v0.0.5 --target main --title v0.0.5 --notes "$(cat <<'EOF'
## What's Changed
* `gc.Collector.BeginWrite`: a write-span gate against the sweep, for callers that write objects outside `PrepareRef` (an ingest, a pull, an inbox drain). `gc.Status` now serializes with running cycles. Both ported from amber-store, with tests.
* `inbox.WithGate`: a variadic option on `inbox.Open` that brackets each drain's store write with a gate such as `BeginWrite`.
* No wire or on-disk format change; v0.0.4 callers compile unchanged.

**Full Changelog**: https://github.com/amber-store/core/compare/v0.0.4...v0.0.5
EOF
)" && cd /private/tmp && GOFLAGS=-mod=mod go list -m github.com/amber-store/core@v0.0.5
```

Expected: the release URL, then `github.com/amber-store/core v0.0.5`.

---

## Phase 2: amber-store

### Task 4: Replace the copies with the dependency

**Files:**
- Delete: `$A/{amberignore,amberpack,cborx,chunkers,fstree,gc,inbox,key,packstore,reference,refstore,tarexport,tarextract}/`
- Create: `$A/sshsign/verifyref.go`, `$A/sshsign/verifyref_test.go`
- Modify: `$A/embedded/embedded.go:145,185,229`; every Go file importing one of the 13 packages; `$A/go.mod`, `$A/go.sum`

**Interfaces:**
- Consumes: core v0.0.5 (`gc.Collector.BeginWrite`, `inbox.WithGate`, and everything the copies exported).
- Produces: `func DecodeVerifiedReference(raw []byte) (reference.Reference, error)` in package `sshsign`.

- [ ] **Step 1: Branch, delete, rewrite imports**

```bash
cd $A && git status --short | wc -l && git checkout -b use-core && git rm -r -q amberignore amberpack cborx chunkers fstree gc inbox key packstore reference refstore tarexport tarextract && git ls-files -z | xargs -0 grep -Il 'github.com/amber-store/amber-store/' | grep -v '^go.sum$' | xargs sed -i -E 's#github.com/amber-store/amber-store/(amberignore|amberpack|cborx|chunkers|fstree|gc|inbox|key|packstore|reference|refstore|tarexport|tarextract)([/"])#github.com/amber-store/core/\1\2#g' && grep -rn 'amber-store/amber-store/\(amberignore\|amberpack\|cborx\|chunkers\|fstree\|gc\|inbox\|key\|packstore\|reference\|refstore\|tarexport\|tarextract\)[/"]' --include='*.go' . | wc -l
```

Expected: `0` dirty files first, `0` old imports at the end. (Note the `([/"])` guard: `key` must not match `keylist`, `reference` must not match a longer name.)

- [ ] **Step 2: Write the failing relocation test**

Create `sshsign/verifyref_test.go`:

```go
package sshsign_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/amber-store/amber-store/sshsign"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/reference"
	"golang.org/x/crypto/ssh"
)

func TestDecodeVerifiedReference(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := fstree.EncodeBlob([]byte("payload")) // a real canonical key
	if err != nil {
		t.Fatal(err)
	}
	rec := reference.Reference{Name: "a:1", Key: blob.Key[:], User: "u", CreatedAt: 42}
	// PublicKey must be set before SignaturePayload is computed: the payload
	// binds the signer's key (see Reference.SignaturePayload), matching
	// signReference in cmd/amber-store/sign.go and every other call site.
	rec.PublicKey = signer.PublicKey().Marshal()
	payload, err := rec.SignaturePayload()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := sshsign.SignWith(signer, payload)
	if err != nil {
		t.Fatal(err)
	}
	rec.Signature = sig
	raw, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sshsign.DecodeVerifiedReference(raw); err != nil {
		t.Fatal(err)
	}

	t.Run("unsigned record is rejected", func(t *testing.T) {
		u := rec
		u.Signature, u.PublicKey = nil, nil
		rawUnsigned, err := u.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sshsign.DecodeVerifiedReference(rawUnsigned); err == nil {
			t.Fatal("unsigned record must be rejected")
		}
	})
	t.Run("tampered signature is rejected", func(t *testing.T) {
		bad := rec
		bad.Signature = append([]byte(nil), rec.Signature...)
		bad.Signature[len(bad.Signature)/2] ^= 0xff
		rawBad, err := bad.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sshsign.DecodeVerifiedReference(rawBad); err == nil {
			t.Fatal("tampered record must be rejected")
		}
	})
}
```

- [ ] **Step 3: Add the dependency, then confirm exactly the expected failures**

```bash
cd $A && go get github.com/amber-store/core@v0.0.5 && go mod tidy && go build ./... 2>&1 | grep -v '^#'; go vet ./sshsign/ 2>&1 | grep -v '^#' | head -3
```

Expected: build errors only at `embedded/embedded.go:145`, `:185`, `:229` (`undefined: reference.DecodeVerified`); vet of `sshsign` reports `undefined: sshsign.DecodeVerifiedReference`. Nothing else.

- [ ] **Step 4: Relocate DecodeVerified**

Create `sshsign/verifyref.go`:

```go
package sshsign

import (
	"errors"
	"fmt"

	"github.com/amber-store/core/reference"
)

// DecodeVerifiedReference decodes raw and verifies its embedded signature;
// unsigned records are rejected. It is the one-call validation used
// wherever a record crosses a trust boundary (remote pull, embedded
// publish). It lives here rather than in core's reference package because
// signature verification, and the ssh dependencies it needs, are this
// repository's concern.
func DecodeVerifiedReference(raw []byte) (reference.Reference, error) {
	rec, err := reference.Decode(raw)
	if err != nil {
		return reference.Reference{}, err
	}
	if len(rec.Signature) == 0 || len(rec.PublicKey) == 0 {
		return reference.Reference{}, errors.New("reference record is not signed")
	}
	payload, err := rec.SignaturePayload()
	if err != nil {
		return reference.Reference{}, err
	}
	if _, err := Verify(payload, rec.Signature, rec.PublicKey); err != nil {
		return reference.Reference{}, fmt.Errorf("reference signature does not verify: %w", err)
	}
	return rec, nil
}
```

In `embedded/embedded.go`, change the three `reference.DecodeVerified(` calls to `sshsign.DecodeVerifiedReference(` and add `"github.com/amber-store/amber-store/sshsign"` to the import block if it is not already there (it is not: the import list has identity, key, packstore, reference, refstore, remoteclient, remotes, remotesync). If `reference` is then unused in that file, gofmt will not remove it; check with `go build` and drop the import if the compiler says it is unused.

- [ ] **Step 5: Verify everything**

```bash
cd $A && go mod tidy && f=$(gofmt -l $(git ls-files '*.go')); [ -n "$f" ] && echo "$f" | xargs gofmt -w; gofmt -l $(git ls-files '*.go'); go build ./... && go vet ./... && go test ./... 2>&1 | grep -E '^(ok|FAIL|---|panic)' | tail -40; echo "--- go.mod direct block:"; awk '/^require \(/{f=1;next} /^\)/{f=0} f && !/indirect/' go.mod
```

Expected: no gofmt output; all packages `ok`; the direct block contains `github.com/amber-store/core v0.0.5` and no longer lists `xorfilter`, `go-cdc-chunkers`, `pebble`, `blake3` or `klauspost/compress` as direct (they become indirect or drop; `fxamacker/cbor` stays direct only if amber-store's own code imports it).

- [ ] **Step 6: Commit**

```bash
cd $A && git add -A && git commit -m "Use github.com/amber-store/core instead of copied packages

Delete the 13 package copies (amberignore, amberpack, cborx, chunkers,
fstree, gc, inbox, key, packstore, reference, refstore, tarexport,
tarextract) and import core v0.0.5, which now carries everything the
copies had grown (gc.BeginWrite, Status serialization, inbox.WithGate).
reference.DecodeVerified moves to sshsign.DecodeVerifiedReference: it
verifies SSH signatures, which is this repository's concern, not core's.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01GAJmL5jzzyWvFiikdwFCEn"
```

---

### Task 5: PR, merge, local checkout

- [ ] **Step 1: Push and open the PR**

```bash
cd $A && git push -u origin use-core && gh pr create --repo amber-store/amber-store --title "Use github.com/amber-store/core instead of copied packages" --body "$(cat <<'EOF'
Deletes the 13 package copies (`amberignore`, `amberpack`, `cborx`, `chunkers`, `fstree`, `gc`, `inbox`, `key`, `packstore`, `reference`, `refstore`, `tarexport`, `tarextract`) and imports `github.com/amber-store/core` v0.0.5. Eight copies were already byte-identical to core; the rest differed by core's additive record write path and by three things this repo had grown, which are now in core v0.0.5 (`gc.Collector.BeginWrite`, `gc.Status` serializing with cycles, `inbox.WithGate`).

`reference.DecodeVerified` moves to `sshsign.DecodeVerifiedReference` with its test: it verifies SSH signatures, which belongs with `sshsign` here rather than in core's `reference`. Its three callers in `embedded/` are updated.

No behaviour change. Design: amber-store/core `docs/superpowers/specs/2026-09-07-amber-store-uses-core-design.md`.

Verified with `gofmt -l`, `go build ./...`, `go vet ./...`, `go test ./...`.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01GAJmL5jzzyWvFiikdwFCEn
EOF
)"
```

- [ ] **Step 2: Merge and fast-forward the user's checkout**

```bash
cd $A && N=$(gh pr list --repo amber-store/amber-store --head use-core --json number --jq '.[0].number') && gh pr merge "$N" --repo amber-store/amber-store --merge --delete-branch && cd ~/draganm/amber-store && git pull --ff-only && git fetch --prune && git log --oneline -1 && ls amberpack 2>&1 | head -1
```

Expected: the merge commit on main; `ls amberpack` reports no such directory.
