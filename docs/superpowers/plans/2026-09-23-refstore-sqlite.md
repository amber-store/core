# Reference Store on SQLite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `refstore` from Pebble to a multi-process SQLite database in WAL mode, with sqlc-generated queries, numbered schema migrations, optimistic (compare-and-swap) reference updates, and a one-time import of existing Pebble stores.

**Architecture:** `refstore` keeps its exported API and gains three conditional writes. SQL lives in `refstore/migrations/*.sql` (schema) and `refstore/queries.sql` (statements); sqlc turns them into the committed package `refstore/internal/refsdb`. `Open` applies outstanding migrations in one `BEGIN IMMEDIATE` transaction keyed by `PRAGMA user_version`, after a lock-free fast path for the already-current case. Pebble is imported only by `refstore/migrate.go`, which runs before the database is opened.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` v1.59.0 (pure Go), `database/sql`, sqlc 1.31.1 (dev tool, from the Nix shell), `golang.org/x/sys/unix` (flock, already a dependency), `github.com/cockroachdb/pebble/v2` (migration only).

**Spec:** `docs/superpowers/specs/2026-09-23-refstore-sqlite-design.md`

## Global Constraints

- The database is `<dir>/refs.sqlite`; `PRAGMA application_id` is `0x616D6272`; `PRAGMA user_version` equals the number of migrations applied (1 today).
- WAL journal mode is mandatory and verified at open. Write transactions use `BEGIN IMMEDIATE` (`_txlock=immediate`). Busy timeout 30 s.
- Version 1 schema, exactly: `CREATE TABLE refs (name BLOB NOT NULL PRIMARY KEY, record BLOB NOT NULL) WITHOUT ROWID`. Names and records are BLOBs, never NULL or TEXT; a nil slice is stored as a zero-length BLOB.
- `sync=true` → `synchronous=FULL`, `fullfsync=1`, `checkpoint_fullfsync=1`; `sync=false` → `synchronous=NORMAL`.
- Migration files are `refstore/migrations/NNNN_description.sql`, contiguous from `0001`, never edited after release.
- Existing exported API and the existing tests in `refstore/refstore_test.go` and `refstore/batch_test.go` stay unchanged.
- Opening an up-to-date store must not take the write lock.
- Only `refstore/migrate.go` (and its test) may import Pebble. No cgo.
- Generated code in `refstore/internal/refsdb` is committed and never edited by hand; regenerate with `go generate ./refstore`.
- Work on branch `refstore-sqlite`. Commit locally per task; do not push until the final review is done.
- Commit message style is `pkg: summary` (see `git log`). End every commit message with:

  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_019kMb3Ckqnion4SKma81cfN
  ```

## Review Focus

Failure modes the spec implies but does not spell out as tests; each is pinned by a test in the task named.

1. **Opening while another process holds a long write transaction** (a GC sweep will, in stage 2): `Open`, `Get` and `All` must succeed without waiting. Task 1, `TestOpenAndReadDoNotWaitForAWriter`.
2. **A writer outlasting the busy timeout**: the second writer gets an error, not a hang, and the store stays usable. Task 1, `TestSecondWriterFailsAfterBusyTimeout`.
3. **Names that are not text** (NUL, invalid UTF-8, high bytes) and **large records** (a 64 KiB signature): round trip and bytewise order. Task 1, `TestArbitraryNamesAndLargeRecords`.
4. **A store path with URI metacharacters** (space, `?`, `#`, `%`, `&`): the database must land in that directory. Task 1, `TestOpenPathWithSpecialCharacters`.
5. **A misplaced `--expect` flag** (`ref set NAME KEY --expect OLD`; urfave/cli stops parsing flags at the first positional): it must fail, never degrade into an unconditional write. Task 4, in `TestE2E_RefExpect`.

## File Structure

| File | Responsibility |
| --- | --- |
| `refstore/refstore.go` (rewrite) | `Store`, `Open`, `Put`, `PutBatch`, `Get`, `Delete`, `All`, `Wipe`, `Close` |
| `refstore/sqlite.go` (new) | constants, DSN and pragmas, `openDB`, WAL check |
| `refstore/schema.go` (new) | embedded migrations, `loadMigrations`, `migrateSchema` |
| `refstore/migrations/0001_refs.sql` (new) | version 1 schema |
| `refstore/queries.sql`, `refstore/sqlc.yaml`, `refstore/generate.go` (new) | sqlc input, `go:generate` |
| `refstore/internal/refsdb/*.go` (generated) | sqlc output |
| `refstore/cas.go` (new) | `ErrConflict`, `CompareAndSwap`, `Create`, `CompareAndDelete` |
| `refstore/migrate.go` (new) | Pebble detection, lock, import, poison marker, retire |
| `refstore/sqlite_test.go`, `refstore/sqlite_internal_test.go`, `refstore/schema_internal_test.go`, `refstore/cas_test.go`, `refstore/migrate_test.go` (new) | tests |
| `cmd/amber-store/ref.go`, `commit.go`, `ingest.go`, `e2e_test.go` | `--expect`; `putRef`/`rmRef` take an expectation |
| `flake.nix`, `.github/workflows/test.yml` | sqlc in the dev shell; `sqlc diff` in CI |
| `architecture/references.md`, `README.md` | docs |

---

### Task 1: SQLite store, sqlc queries, migration runner

**Files:**
- Create: `refstore/migrations/0001_refs.sql`, `refstore/queries.sql`, `refstore/sqlc.yaml`, `refstore/generate.go`, `refstore/sqlite.go`, `refstore/schema.go`, `refstore/sqlite_test.go`, `refstore/sqlite_internal_test.go`, `refstore/schema_internal_test.go`
- Generate: `refstore/internal/refsdb/{db.go,models.go,queries.sql.go}`
- Rewrite: `refstore/refstore.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces (unchanged): `refstore.Open(dir string, sync bool) (*Store, error)`, `(*Store).Put/PutBatch/Get/Delete/All/Wipe/Close`, `refstore.ErrNotFound`, `refstore.Record`.
- Produces (internal): `openDB(path string, syncWrites, wal bool) (*sql.DB, error)`; `blob([]byte) []byte`; `dbFile`; `busyTimeout`; `migrations []migration`; `migrateSchema(ctx, *sql.Conn, []migration) error`; `refsdb.Queries` with `GetRecord`, `PutRecord`, `CreateRecord`, `ReplaceRecord`, `DeleteRecord`, `DeleteRecordIf`, `ListRecords`, `DeleteAllRecords`.
- In this task `Open` does not yet call the Pebble migration (Task 3 adds the call).

- [ ] **Step 1: Add the driver**

```bash
go get modernc.org/sqlite@v1.59.0
```

- [ ] **Step 2: Write the SQL and the sqlc config**

`refstore/migrations/0001_refs.sql`:

```sql
-- Reference records: name bytes -> canonical CBOR record bytes, verbatim.
-- Both columns are BLOBs, never NULL or TEXT, so names order by memcmp.
CREATE TABLE refs (
    name   BLOB NOT NULL PRIMARY KEY,
    record BLOB NOT NULL
) WITHOUT ROWID;
```

`refstore/queries.sql`:

```sql
-- name: GetRecord :one
SELECT record FROM refs WHERE name = ?;

-- name: PutRecord :exec
INSERT INTO refs (name, record) VALUES (?, ?)
ON CONFLICT (name) DO UPDATE SET record = excluded.record;

-- name: CreateRecord :execrows
INSERT INTO refs (name, record) VALUES (?, ?)
ON CONFLICT (name) DO NOTHING;

-- name: ReplaceRecord :execrows
UPDATE refs SET record = sqlc.arg(record)
WHERE name = sqlc.arg(name) AND record = sqlc.arg(old_record);

-- name: DeleteRecord :execrows
DELETE FROM refs WHERE name = ?;

-- name: DeleteRecordIf :execrows
DELETE FROM refs WHERE name = sqlc.arg(name) AND record = sqlc.arg(old_record);

-- name: ListRecords :many
SELECT name, record FROM refs ORDER BY name;

-- name: DeleteAllRecords :exec
DELETE FROM refs;
```

`refstore/sqlc.yaml`:

```yaml
version: "2"
sql:
  - engine: "sqlite"
    schema: "migrations"
    queries: "queries.sql"
    gen:
      go:
        package: "refsdb"
        out: "internal/refsdb"
        omit_sqlc_version: true
        emit_empty_slices: true
```

`refstore/generate.go`:

```go
package refstore

// The queries in queries.sql are compiled against the schema in migrations/
// into internal/refsdb. sqlc comes from the Nix dev shell; the output is
// committed, so building needs no sqlc.
//go:generate sqlc generate
```

- [ ] **Step 3: Generate**

Run: `go generate ./refstore && ls refstore/internal/refsdb`
Expected: `db.go models.go queries.sql.go`, and `cd refstore && sqlc diff` prints nothing.

- [ ] **Step 4: Write the failing tests**

`refstore/sqlite_test.go`:

```go
package refstore_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amber-store/core/refstore"
)

const childDirEnv = "REFSTORE_TEST_CHILD_DIR"

// TestMain doubles as the second process of
// TestSecondProcessSharesTheStore: with childDirEnv set, the test binary
// opens that store, checks the parent's record, writes its own and exits.
func TestMain(m *testing.M) {
	if dir := os.Getenv(childDirEnv); dir != "" {
		if err := childProcess(dir); err != nil {
			fmt.Fprintln(os.Stderr, "child:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func childProcess(dir string) error {
	s, err := refstore.Open(dir, false)
	if err != nil {
		return err
	}
	got, err := s.Get("from-parent")
	if err != nil {
		return fmt.Errorf("reading the parent's record: %w", err)
	}
	if string(got) != "p" {
		return fmt.Errorf("from-parent = %q, want p", got)
	}
	if err := s.Put("from-child", []byte("c")); err != nil {
		return err
	}
	return s.Close()
}

func TestSecondProcessSharesTheStore(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir) // stays open for the child's whole life
	if err := s.Put("from-parent", []byte("p")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childDirEnv+"="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
	got, err := s.Get("from-child")
	if err != nil || string(got) != "c" {
		t.Fatalf("from-child = %q, %v; want c", got, err)
	}
}

func TestPutNilRecordReadsBackEmpty(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.Put("nil", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("nil")
	if err != nil || len(got) != 0 {
		t.Fatalf("Get = %q, %v; want empty", got, err)
	}
}

func TestTwoHandlesShareOneStore(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir), open(t, dir)
	if err := a.Put("x", []byte("1")); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get("x")
	if err != nil || string(got) != "1" {
		t.Fatalf("second handle Get = %q, %v", got, err)
	}
	if err := b.Delete("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get("x"); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("first handle after the second's delete: %v, want ErrNotFound", err)
	}
}

func TestTwoHandlesKeepBatchesAtomic(t *testing.T) {
	dir := t.TempDir()
	handles := []*refstore.Store{open(t, dir), open(t, dir)}
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			s := handles[worker%2]
			for generation := range 50 {
				value := []byte(fmt.Sprintf("%d/%d", worker, generation))
				if err := s.PutBatch([]refstore.Record{{Name: "a", Data: value}, {Name: "b", Data: value}}); err != nil {
					t.Error(err)
					return
				}
				all, err := handles[(worker+1)%2].All()
				if err != nil {
					t.Error(err)
					return
				}
				if len(all) != 2 || !bytes.Equal(all[0].Data, all[1].Data) {
					t.Error("partial batch became visible")
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestArbitraryNamesAndLargeRecords(t *testing.T) {
	s := open(t, t.TempDir())
	big := bytes.Repeat([]byte{0xab}, 100<<10)
	names := []string{"a", "a\x00b", "\xff\xfe", "b", ""}
	for _, n := range names {
		if err := s.Put(n, big); err != nil {
			t.Fatalf("Put(%q): %v", n, err)
		}
	}
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "a", "a\x00b", "b", "\xff\xfe"}
	if len(all) != len(want) {
		t.Fatalf("All returned %d records, want %d", len(all), len(want))
	}
	for i, n := range want {
		if all[i].Name != n {
			t.Errorf("all[%d].Name = %q, want %q", i, all[i].Name, n)
		}
		if !bytes.Equal(all[i].Data, big) {
			t.Errorf("all[%d]: the record did not round-trip", i)
		}
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "we ird?#%41&=dir")
	s, err := refstore.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "refs.sqlite")); err != nil {
		t.Fatalf("the database is not inside the store directory: %v", err)
	}
	got, err := open(t, dir).Get("k")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get after reopen = %q, %v", got, err)
	}
}

func TestOpenRejectsForeignDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "refs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other (x)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := refstore.Open(dir, false); err == nil || !strings.Contains(err.Error(), "not a reference store") {
		t.Fatalf("Open(foreign database) = %v, want a not-a-reference-store error", err)
	}
}

func TestOpenRejectsNonDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "refs.sqlite"), bytes.Repeat([]byte("garbage "), 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := refstore.Open(dir, false); err == nil {
		t.Fatal("Open(garbage file) succeeded")
	}
}
```

`refstore/sqlite_internal_test.go`:

```go
package refstore

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openT(t *testing.T, dir string, sync bool) *Store {
	t.Helper()
	s, err := Open(dir, sync)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func shortBusyTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := busyTimeout
	busyTimeout = d
	t.Cleanup(func() { busyTimeout = old })
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestOpensInWALMode(t *testing.T) {
	s := openT(t, t.TempDir(), false)
	if got := pragma(t, s.db, "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", got)
	}
}

func TestSyncFlagSelectsDurabilityPragmas(t *testing.T) {
	for _, tc := range []struct {
		sync                          bool
		synchronous, full, checkpoint string
	}{
		{true, "2", "1", "1"},  // FULL
		{false, "1", "0", "0"}, // NORMAL
	} {
		s := openT(t, t.TempDir(), tc.sync)
		if got := pragma(t, s.db, "synchronous"); got != tc.synchronous {
			t.Errorf("sync=%v: synchronous = %s, want %s", tc.sync, got, tc.synchronous)
		}
		if got := pragma(t, s.db, "fullfsync"); got != tc.full {
			t.Errorf("sync=%v: fullfsync = %s, want %s", tc.sync, got, tc.full)
		}
		if got := pragma(t, s.db, "checkpoint_fullfsync"); got != tc.checkpoint {
			t.Errorf("sync=%v: checkpoint_fullfsync = %s, want %s", tc.sync, got, tc.checkpoint)
		}
	}
}

func TestFileFormatPins(t *testing.T) {
	s := openT(t, t.TempDir(), false)
	if got := pragma(t, s.db, "application_id"); got != "1634558578" {
		t.Errorf("application_id = %s, want 1634558578 (0x616D6272)", got)
	}
	if got := pragma(t, s.db, "user_version"); got != "1" {
		t.Errorf("user_version = %s, want 1", got)
	}
	var ddl string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name = 'refs'").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	const want = "CREATE TABLE refs ( name BLOB NOT NULL PRIMARY KEY, record BLOB NOT NULL ) WITHOUT ROWID"
	if got := strings.Join(strings.Fields(ddl), " "); got != want {
		t.Errorf("schema = %q\nwant     %q", got, want)
	}
}

// A reader holding a cursor open must not block a writer, and must keep its
// snapshot: that is WAL. In rollback-journal mode the Put would wait out the
// busy timeout and fail.
func TestWriterIsNotBlockedByAnOpenReader(t *testing.T) {
	shortBusyTimeout(t, 2*time.Second)
	dir := t.TempDir()
	reader, writer := openT(t, dir, false), openT(t, dir, false)
	for _, n := range []string{"a", "b"} {
		if err := reader.Put(n, []byte(n)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := reader.db.Query("SELECT name FROM refs ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no first row")
	}
	if err := writer.Put("c", []byte("c")); err != nil {
		t.Fatalf("a writer was blocked by an open reader: %v", err)
	}
	seen := 1
	for rows.Next() {
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("the reader saw %d rows, want its snapshot of 2", seen)
	}
}

// holdWriteLock starts a write transaction on s and keeps it open until the
// returned function is called.
func holdWriteLock(t *testing.T, s *Store) (release func()) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO refs (name, record) VALUES (x'68656c64', x'')"); err != nil {
		t.Fatal(err)
	}
	return func() { tx.Rollback() }
}

func TestOpenAndReadDoNotWaitForAWriter(t *testing.T) {
	shortBusyTimeout(t, 5*time.Second)
	dir := t.TempDir()
	busy := openT(t, dir, false)
	if err := busy.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, busy)
	defer release()

	start := time.Now()
	other := openT(t, dir, false)
	if got, err := other.Get("a"); err != nil || string(got) != "1" {
		t.Fatalf("Get during another writer's transaction = %q, %v", got, err)
	}
	if all, err := other.All(); err != nil || len(all) != 1 {
		t.Fatalf("All during another writer's transaction = %d records, %v", len(all), err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Open+Get+All took %v while a writer was active; they must not wait for it", d)
	}
}

func TestSecondWriterFailsAfterBusyTimeout(t *testing.T) {
	shortBusyTimeout(t, 300*time.Millisecond)
	dir := t.TempDir()
	busy, other := openT(t, dir, false), openT(t, dir, false)
	release := holdWriteLock(t, busy)

	start := time.Now()
	err := other.Put("b", []byte("2"))
	if err == nil {
		t.Fatal("Put succeeded while another connection held the write lock")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Put took %v to give up, want about the busy timeout", d)
	}
	release()
	if err := other.Put("b", []byte("2")); err != nil {
		t.Fatalf("Put after the writer finished: %v", err)
	}
}

func rawDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	dsn, err := dataSourceName(filepath.Join(dir, dbFile), false, true)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
```

`refstore/schema_internal_test.go`:

```go
package refstore

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedMigrationsAreContiguous(t *testing.T) {
	if len(migrations) == 0 {
		t.Fatal("no embedded migrations")
	}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migrations[%d] = %s (version %d), want version %d", i, m.name, m.version, i+1)
		}
	}
}

func TestLoadMigrationsRejectsBadSets(t *testing.T) {
	for name, files := range map[string][]string{
		"gap":       {"0001_a.sql", "0003_c.sql"},
		"starts>1":  {"0002_b.sql"},
		"duplicate": {"0001_a.sql", "0001_b.sql"},
		"bad name":  {"0001_a.sql", "second.sql"},
		"short":     {"1_a.sql"},
		"empty":     {},
	} {
		fsys := fstest.MapFS{"m": &fstest.MapFile{Mode: 0o755 | (1 << 31)}}
		for _, f := range files {
			fsys["m/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		if _, err := loadMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: loadMigrations accepted %v", name, files)
		}
	}
}

func withSecond(sql string) []migration {
	return append(slices.Clone(migrations), migration{version: len(migrations) + 1, name: "9999_test.sql", sql: sql})
}

func TestMigrationsUpgradeAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openT(t, dir, false)
	if err := s.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, dir)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	next := withSecond("ALTER TABLE refs ADD COLUMN note BLOB;\nUPDATE refs SET note = x'6e';\n")
	for range 2 { // the second run must find nothing to do
		if err := migrateSchema(ctx, conn, next); err != nil {
			t.Fatal(err)
		}
	}
	if got := pragma(t, db, "user_version"); got != "2" {
		t.Fatalf("user_version = %s, want 2", got)
	}
	var note, record []byte
	if err := db.QueryRow("SELECT note, record FROM refs WHERE name = x'61'").Scan(&note, &record); err != nil {
		t.Fatal(err)
	}
	if string(note) != "n" || string(record) != "1" {
		t.Fatalf("after upgrade: note = %q, record = %q; want n, 1", note, record)
	}
}

func TestFailedMigrationChangesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openT(t, dir, false)
	if err := s.Put("a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, dir)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bad := withSecond("ALTER TABLE refs ADD COLUMN note BLOB;\nINSERT INTO nowhere VALUES (1);\n")
	if err := migrateSchema(ctx, conn, bad); err == nil || !strings.Contains(err.Error(), "9999_test.sql") {
		t.Fatalf("migrateSchema(bad) = %v, want an error naming the migration", err)
	}
	if got := pragma(t, db, "user_version"); got != "1" {
		t.Errorf("user_version = %s after a failed migration, want 1", got)
	}
	if _, err := db.Exec("SELECT note FROM refs"); err == nil {
		t.Error("the failed migration's first statement was not rolled back")
	}
	if got, err := s.Get("a"); err != nil || string(got) != "1" {
		t.Errorf("Get after a failed migration = %q, %v", got, err)
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, false); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open(schema version 2) = %v, want a newer-version error", err)
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test ./refstore`
Expected: build failure (`undefined: busyTimeout`, `dataSourceName`, `migrations`, …).

- [ ] **Step 6: Implement**

`refstore/sqlite.go`:

```go
package refstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver, registered as "sqlite"
)

const (
	// dbFile is the database's name inside the store directory.
	dbFile = "refs.sqlite"
	// applicationID marks the file as a reference store: "ambr".
	applicationID = 0x616d6272
	// maxConns caps the connection pool; each connection is a file handle.
	maxConns = 8
)

// busyTimeout bounds how long a write waits for another connection's —
// usually another process's — write transaction before failing with SQLite's
// busy error. A variable so tests can shorten it.
var busyTimeout = 30 * time.Second

// dataSourceName builds the driver DSN for the database at path. The pragmas
// apply to every connection of the pool. Transactions begin IMMEDIATE, so a
// write never has to upgrade a read lock. wal=false selects a rollback
// journal, used only for the single-file database the Pebble import builds.
func dataSourceName(path string, syncWrites, wal bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	journal := "DELETE"
	if wal {
		journal = "WAL"
	}
	params := []string{
		"_txlock=immediate",
		fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeout.Milliseconds()),
		"_pragma=journal_mode(" + journal + ")",
	}
	if syncWrites {
		// fullfsync: on macOS a plain fsync does not reach the platter;
		// Go's File.Sync, which the packstore uses, already does the full flush.
		params = append(params, "_pragma=synchronous(FULL)", "_pragma=fullfsync(1)", "_pragma=checkpoint_fullfsync(1)")
	} else {
		params = append(params, "_pragma=synchronous(NORMAL)")
	}
	// A file: URI, so that '?', '#', '%' and spaces in the path are escaped.
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String() + "?" + strings.Join(params, "&"), nil
}

// openDB opens the database at path, verifies WAL mode when asked for, and
// brings the schema up to date.
func openDB(path string, syncWrites, wal bool) (*sql.DB, error) {
	dsn, err := dataSourceName(path, syncWrites, wal)
	if err != nil {
		return nil, fmt.Errorf("refstore: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("refstore: opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	if err := prepare(db, path, wal); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func prepare(db *sql.DB, path string, wal bool) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("refstore: opening sqlite %s: %w", path, err)
	}
	defer conn.Close()
	if wal {
		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			return fmt.Errorf("refstore: %s: reading the journal mode: %w", path, err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("refstore: %s: journal mode is %q, want wal; the filesystem must support SQLite's shared-memory WAL index", path, mode)
		}
	}
	if err := migrateSchema(ctx, conn, migrations); err != nil {
		return fmt.Errorf("%w (%s)", err, path)
	}
	return nil
}

// blob maps a nil slice to an empty one: the driver binds nil as NULL, and
// names and records are always BLOBs.
func blob(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
```

`refstore/schema.go`:

```go
package refstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// A migration is one released schema step. Files are named
// NNNN_description.sql, numbered contiguously from 0001, hold plain SQL (one
// or more statements) and never change once released. PRAGMA user_version
// records how many have been applied; there is no other bookkeeping, so other
// implementations can run the same files by the same rule.
type migration struct {
	version int
	name    string
	sql     string
}

//go:embed migrations/*.sql
var embedded embed.FS

var migrations = func() []migration {
	set, err := loadMigrations(embedded, "migrations")
	if err != nil {
		panic(err)
	}
	return set
}()

func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("refstore: reading migrations: %w", err)
	}
	var set []migration
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if e.IsDir() || !ok || err != nil || len(prefix) != 4 || version < 1 || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("refstore: migration %q is not named NNNN_description.sql", name)
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("refstore: reading migration %s: %w", name, err)
		}
		set = append(set, migration{version: version, name: name, sql: string(body)})
	}
	if len(set) == 0 {
		return nil, errors.New("refstore: no migrations")
	}
	slices.SortStableFunc(set, func(a, b migration) int { return a.version - b.version })
	for i, m := range set {
		if m.version != i+1 {
			return nil, fmt.Errorf("refstore: migrations are not contiguous from 0001: %s is in position %d", m.name, i+1)
		}
	}
	return set, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type schemaState struct{ appID, version, objects int64 }

func readSchemaState(ctx context.Context, q rowQuerier) (schemaState, error) {
	var st schemaState
	for _, read := range []struct {
		query string
		dest  *int64
	}{
		{"PRAGMA application_id", &st.appID},
		{"PRAGMA user_version", &st.version},
		{"SELECT count(*) FROM sqlite_master", &st.objects},
	} {
		if err := q.QueryRowContext(ctx, read.query).Scan(read.dest); err != nil {
			return st, fmt.Errorf("refstore: reading the schema state: %w", err)
		}
	}
	return st, nil
}

// migrateSchema brings the database on conn up to the newest migration in
// set. An up-to-date database is recognized from a plain read, without the
// write lock, so opening a store never waits for another process's write
// transaction. Otherwise everything happens inside one IMMEDIATE transaction:
// a crash leaves the old version, and of two processes racing to initialize
// or upgrade a store the second finds nothing left to do.
func migrateSchema(ctx context.Context, conn *sql.Conn, set []migration) (err error) {
	latest := int64(len(set))
	st, err := readSchemaState(ctx, conn)
	if err != nil {
		return err
	}
	if st.appID == applicationID && st.version == latest {
		return nil
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("refstore: schema: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if st, err = readSchemaState(ctx, tx); err != nil {
		return err
	}
	switch {
	case st.appID == 0 && st.version == 0 && st.objects == 0: // a fresh database
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", applicationID)); err != nil {
			return fmt.Errorf("refstore: schema: %w", err)
		}
	case st.appID != applicationID:
		return fmt.Errorf("refstore: not a reference store: application_id is %#x, want %#x", st.appID, applicationID)
	}
	if st.version > latest {
		return fmt.Errorf("refstore: schema version %d is newer than this release understands (%d)", st.version, latest)
	}
	for _, m := range set[st.version:] {
		if _, err = tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("refstore: migration %s: %w", m.name, err)
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", latest)); err != nil {
		return fmt.Errorf("refstore: schema: %w", err)
	}
	return tx.Commit()
}
```

`refstore/refstore.go` (full replacement):

```go
// Package refstore persists reference records in a SQLite database: name
// bytes → CBOR record bytes, stored verbatim. It is a dumb KV layer; record
// validation belongs to the daemon and the reference package.
//
// The database is <dir>/refs.sqlite in WAL mode, so any number of processes
// may hold a store open at once: readers work from a snapshot and never
// block, and writers — in this process or another — queue behind one another
// (SQLite runs one write transaction at a time; a second writer waits up to
// busyTimeout). The file format is shared with other implementations; see
// architecture/references.md. Queries are generated by sqlc from queries.sql,
// and the schema is built by the numbered files in migrations/.
package refstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/amber-store/core/refstore/internal/refsdb"
)

// ErrNotFound is returned by Get, Delete and the compare forms for an absent name.
var ErrNotFound = errors.New("refstore: reference not found")

// Store is a SQLite-backed name→record map. It is safe for concurrent use,
// by goroutines and by processes. Readers (Get, All) never block; writes are
// serialized in-process by writeMu, so they queue on the mutex rather than
// polling SQLite's file lock, and across processes by SQLite itself.
type Store struct {
	db      *sql.DB
	q       *refsdb.Queries
	writeMu sync.Mutex
}

// Open opens (creating if missing) the refs database in dir. sync selects
// the write durability, matching the daemon's --sync flag.
func Open(dir string, sync bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("refstore: creating %s: %w", dir, err)
	}
	db, err := openDB(filepath.Join(dir, dbFile), sync, true)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, q: refsdb.New(db)}, nil
}

// Put stores record under name, overwriting unconditionally.
func (s *Store) Put(name string, record []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.q.PutRecord(context.Background(), refsdb.PutRecordParams{Name: blob([]byte(name)), Record: blob(record)})
}

// PutBatch publishes records atomically, using the configured write durability.
// When names repeat, the last record wins. An empty batch changes nothing.
func (s *Store) PutBatch(records []Record) (err error) {
	if len(records) == 0 {
		return nil
	}
	ctx := context.Background()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	q := s.q.WithTx(tx)
	for _, r := range records {
		if err = q.PutRecord(ctx, refsdb.PutRecordParams{Name: blob([]byte(r.Name)), Record: blob(r.Data)}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get returns the record stored under name, or ErrNotFound.
func (s *Store) Get(name string) ([]byte, error) {
	record, err := s.q.GetRecord(context.Background(), blob([]byte(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return blob(record), nil
}

// Delete removes name, or returns ErrNotFound if absent. The check is the
// statement's own row count, so concurrent Deletes of the same name — from
// any process — report ErrNotFound to all but one caller.
func (s *Store) Delete(name string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.q.DeleteRecord(context.Background(), blob([]byte(name)))
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Record is one (name, record-bytes) pair from All.
type Record struct {
	Name string
	Data []byte
}

// All returns every record in lexicographic name order, from one snapshot.
func (s *Store) All() ([]Record, error) {
	rows, err := s.q.ListRecords(context.Background())
	if err != nil {
		return nil, err
	}
	recs := make([]Record, len(rows))
	for i, r := range rows {
		recs[i] = Record{Name: string(r.Name), Data: blob(r.Record)}
	}
	return recs, nil
}

// Wipe deletes every record (the store-wipe operation).
func (s *Store) Wipe() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.q.DeleteAllRecords(context.Background())
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}
```

- [ ] **Step 7: Run the tests**

Run: `go mod tidy && go test ./refstore && go test -race ./refstore && go vet ./refstore/...`
Expected: all `ok`. `TestAllEmpty` expects `len(recs) == 0`, which the empty slice satisfies.

- [ ] **Step 8: Run everything that uses the store**

Run: `go test ./gc ./cmd/...`
Expected: `ok` (Pebble is now unused; `go mod tidy` has dropped it, and Task 3 brings it back).

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum refstore
git commit -m "refstore: SQLite in WAL mode; sqlc queries; numbered schema migrations"
```

---

### Task 2: optimistic updates

**Files:**
- Create: `refstore/cas.go`, `refstore/cas_test.go`

**Interfaces:**
- Consumes: `refsdb.Queries.GetRecord`, `ReplaceRecord`, `CreateRecord`, `DeleteRecordIf`; `blob`; `reference.Decode`.
- Produces: `refstore.ErrConflict`; `(*Store).CompareAndSwap(name string, old key.Key, record []byte) error`; `(*Store).Create(name string, record []byte) error`; `(*Store).CompareAndDelete(name string, old key.Key) error`.

- [ ] **Step 1: Write the failing test**

`refstore/cas_test.go`:

```go
package refstore_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/amber-store/core/key"
	"github.com/amber-store/core/reference"
	"github.com/amber-store/core/refstore"
)

// blobKey is the key of a Blob holding s.
func blobKey(t *testing.T, s string) key.Key {
	t.Helper()
	k, err := key.New(key.Blob, uint64(len(s)), []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// record is an encoded reference called name pointing at k.
func record(t *testing.T, name string, k key.Key, createdAt int64) []byte {
	t.Helper()
	raw, err := reference.Reference{Name: name, Key: k[:], CreatedAt: createdAt}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCompareAndSwap(t *testing.T) {
	s := open(t, t.TempDir())
	k1, k2, k3 := blobKey(t, "one"), blobKey(t, "two"), blobKey(t, "three")

	if err := s.CompareAndSwap("r", k1, record(t, "r", k2, 1)); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("swap of an absent reference = %v, want ErrNotFound", err)
	}
	at1 := record(t, "r", k1, 1)
	if err := s.Put("r", at1); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwap("r", k3, record(t, "r", k2, 2)); !errors.Is(err, refstore.ErrConflict) {
		t.Fatalf("swap from the wrong key = %v, want ErrConflict", err)
	}
	if got, _ := s.Get("r"); !bytes.Equal(got, at1) {
		t.Fatal("a refused swap changed the record")
	}
	at2 := record(t, "r", k2, 2)
	if err := s.CompareAndSwap("r", k1, at2); err != nil {
		t.Fatalf("swap from the current key: %v", err)
	}
	if got, _ := s.Get("r"); !bytes.Equal(got, at2) {
		t.Fatal("the swap did not store the new record")
	}
	// The expectation is the key, not the record: a re-put of the same key
	// with a newer timestamp does not invalidate it.
	if err := s.Put("r", record(t, "r", k2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndSwap("r", k2, record(t, "r", k3, 4)); err != nil {
		t.Fatalf("swap after a same-key re-put: %v", err)
	}
}

func TestCreate(t *testing.T) {
	s := open(t, t.TempDir())
	k1, k2 := blobKey(t, "one"), blobKey(t, "two")
	first := record(t, "r", k1, 1)
	if err := s.Create("r", first); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("r", record(t, "r", k2, 2)); !errors.Is(err, refstore.ErrConflict) {
		t.Fatalf("second Create = %v, want ErrConflict", err)
	}
	if got, _ := s.Get("r"); !bytes.Equal(got, first) {
		t.Fatal("a refused Create changed the record")
	}
}

func TestCompareAndDelete(t *testing.T) {
	s := open(t, t.TempDir())
	k1, k2 := blobKey(t, "one"), blobKey(t, "two")
	if err := s.CompareAndDelete("r", k1); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("delete of an absent reference = %v, want ErrNotFound", err)
	}
	if err := s.Put("r", record(t, "r", k1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.CompareAndDelete("r", k2); !errors.Is(err, refstore.ErrConflict) {
		t.Fatalf("delete from the wrong key = %v, want ErrConflict", err)
	}
	if _, err := s.Get("r"); err != nil {
		t.Fatalf("a refused delete removed the reference: %v", err)
	}
	if err := s.CompareAndDelete("r", k1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("r"); !errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("Get after the delete = %v, want ErrNotFound", err)
	}
}

func TestCompareFormsRejectAnUndecodableCurrentRecord(t *testing.T) {
	s := open(t, t.TempDir())
	k1 := blobKey(t, "one")
	if err := s.Put("r", []byte("not cbor")); err != nil {
		t.Fatal(err)
	}
	err := s.CompareAndSwap("r", k1, record(t, "r", k1, 1))
	if err == nil || errors.Is(err, refstore.ErrConflict) || errors.Is(err, refstore.ErrNotFound) {
		t.Fatalf("swap over an undecodable record = %v, want a decode error", err)
	}
	if err := s.CompareAndDelete("r", k1); err == nil || errors.Is(err, refstore.ErrConflict) {
		t.Fatalf("delete of an undecodable record = %v, want a decode error", err)
	}
	if got, _ := s.Get("r"); string(got) != "not cbor" {
		t.Fatal("the undecodable record was changed")
	}
}

// Of many writers moving a reference away from the same expected key, across
// two handles, exactly one wins.
func TestConcurrentSwapsHaveOneWinner(t *testing.T) {
	dir := t.TempDir()
	handles := []*refstore.Store{open(t, dir), open(t, dir)}
	start := blobKey(t, "start")
	if err := handles[0].Put("r", record(t, "r", start, 0)); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errs := make([]error, n)
	targets := make([]key.Key, n)
	var wg sync.WaitGroup
	for i := range n {
		targets[i] = blobKey(t, string(rune('a'+i)))
		wg.Go(func() {
			errs[i] = handles[i%2].CompareAndSwap("r", start, record(t, "r", targets[i], int64(i+1)))
		})
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner == -1:
			winner = i
		case err == nil:
			t.Fatalf("swaps %d and %d both succeeded", winner, i)
		case !errors.Is(err, refstore.ErrConflict):
			t.Errorf("swap %d: %v, want nil or ErrConflict", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no swap succeeded")
	}
	got, err := handles[0].Get("r")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := reference.Decode(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ref.Key, targets[winner][:]) {
		t.Fatal("the stored reference is not the winner's")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./refstore -run 'Compare|Create|Swaps'`
Expected: build failure, `s.CompareAndSwap undefined`.

- [ ] **Step 3: Implement**

`refstore/cas.go`:

```go
package refstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/amber-store/core/key"
	"github.com/amber-store/core/reference"
	"github.com/amber-store/core/refstore/internal/refsdb"
)

// ErrConflict is returned by the optimistic writes when the reference is not
// in the state the caller expected: for Create it exists, for the compare
// forms it points at another key. Nothing was changed; the caller re-reads
// and decides.
var ErrConflict = errors.New("refstore: reference is not at the expected key")

// CompareAndSwap stores record under name only if the reference currently
// points at old — the move from one key to the next that must not overwrite
// somebody else's move. It returns ErrNotFound if the reference does not
// exist and ErrConflict if it points elsewhere. The expectation is the key,
// not the record bytes: a rewrite that kept the key does not invalidate it.
// record is stored verbatim, as by Put.
func (s *Store) CompareAndSwap(name string, old key.Key, record []byte) error {
	return s.ifAt(name, old, func(ctx context.Context, q *refsdb.Queries, current []byte) (int64, error) {
		return q.ReplaceRecord(ctx, refsdb.ReplaceRecordParams{Record: blob(record), Name: blob([]byte(name)), OldRecord: current})
	})
}

// CompareAndDelete removes name only if it currently points at old, with
// CompareAndSwap's errors.
func (s *Store) CompareAndDelete(name string, old key.Key) error {
	return s.ifAt(name, old, func(ctx context.Context, q *refsdb.Queries, current []byte) (int64, error) {
		return q.DeleteRecordIf(ctx, refsdb.DeleteRecordIfParams{Name: blob([]byte(name)), OldRecord: current})
	})
}

// Create stores record under name only if no such reference exists, and
// returns ErrConflict if one does.
func (s *Store) Create(name string, record []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.q.CreateRecord(context.Background(), refsdb.CreateRecordParams{Name: blob([]byte(name)), Record: blob(record)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// ifAt runs change inside one write transaction after checking that name
// exists and points at old. The key lives inside the record, so the current
// record is decoded; one that does not decode is an error, never a match.
// change is guarded by the bytes just read and reports the rows it touched.
func (s *Store) ifAt(name string, old key.Key, change func(ctx context.Context, q *refsdb.Queries, current []byte) (int64, error)) (err error) {
	ctx := context.Background()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	q := s.q.WithTx(tx)
	current, err := q.GetRecord(ctx, blob([]byte(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	ref, err := reference.Decode(current)
	if err != nil {
		return fmt.Errorf("refstore: current record of %q: %w", name, err)
	}
	if !bytes.Equal(ref.Key, old[:]) {
		return ErrConflict
	}
	n, err := change(ctx, q, blob(current))
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./refstore && go test -race ./refstore && go vet ./refstore/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add refstore/cas.go refstore/cas_test.go
git commit -m "refstore: optimistic updates: CompareAndSwap, Create, CompareAndDelete"
```

---

### Task 3: import existing Pebble stores

**Files:**
- Create: `refstore/migrate.go`, `refstore/migrate_test.go`
- Modify: `refstore/refstore.go` (`Open` calls `migrateLegacy`), `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `openDB(path, syncWrites, wal)`, `dbFile`, `blob`, `refsdb.New`, `refsdb.PutRecordParams`.
- Produces: `migrateLegacy(dir string) error`, called by `Open` after `MkdirAll` and before `openDB`.

- [ ] **Step 1: Write the failing test**

`refstore/migrate_test.go`:

```go
package refstore_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/amber-store/core/refstore"
	"github.com/cockroachdb/pebble/v2"
)

type quietLogger struct{}

func (quietLogger) Infof(string, ...any)  {}
func (quietLogger) Errorf(string, ...any) {}
func (quietLogger) Fatalf(string, ...any) {}

// legacyStore writes a Pebble reference store the way the previous release
// did. flushed records end up in an sstable, the rest only in the
// write-ahead log.
func legacyStore(t *testing.T, dir string, flushed, logged map[string]string) {
	t.Helper()
	db, err := pebble.Open(dir, &pebble.Options{Logger: quietLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range flushed {
		if err := db.Set([]byte(k), []byte(v), pebble.Sync); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	for k, v := range logged {
		if err := db.Set([]byte(k), []byte(v), pebble.Sync); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func wantRecords(t *testing.T, s *refstore.Store, want map[string]string) {
	t.Helper()
	all, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range all {
		got[r.Name] = string(r.Data)
	}
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("records = %v, want %v", got, want)
		}
	}
}

func TestMigratesLegacyPebbleStore(t *testing.T) {
	dir := t.TempDir()
	legacyStore(t, dir, map[string]string{"a/flushed": "1", "": "empty-name"}, map[string]string{"b/logged": "2"})
	want := map[string]string{"a/flushed": "1", "": "empty-name", "b/logged": "2"}

	s, err := refstore.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wantRecords(t, s, want)
	if err := s.Put("c/new", []byte("3")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want["c/new"] = "3"

	if exists(t, filepath.Join(dir, "LOCK")) {
		t.Error("Pebble's LOCK file is still in the store directory")
	}
	if !exists(t, filepath.Join(dir, "pebble-migrated", "LOCK")) {
		t.Error("the Pebble files were not kept in pebble-migrated/")
	}
	if !exists(t, filepath.Join(dir, "marker.format-version.999999.999")) {
		t.Error("the poison marker is missing")
	}
	wantRecords(t, open(t, dir), want) // a second open imports nothing again
}

func TestMigratedStoreRefusesOldPebbleBinaries(t *testing.T) {
	dir := t.TempDir()
	legacyStore(t, dir, map[string]string{"a": "1"}, nil)
	open(t, dir).Close()
	db, err := pebble.Open(dir, &pebble.Options{Logger: quietLogger{}})
	if err == nil {
		db.Close()
		t.Fatal("a Pebble-based binary can still open the migrated directory; it would see no references")
	}
}

func TestEmptyLegacyStoreMigrates(t *testing.T) {
	dir := t.TempDir()
	legacyStore(t, dir, nil, nil)
	wantRecords(t, open(t, dir), map[string]string{})
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A crash after the import committed but before the Pebble files were moved
// leaves both side by side. The next open must finish the cleanup without
// importing again, or it would overwrite newer data with the old.
func TestInterruptedCleanupDoesNotReimport(t *testing.T) {
	dir := t.TempDir()
	legacyStore(t, dir, map[string]string{"a": "old"}, nil)
	backup := t.TempDir()
	copyDir(t, dir, backup)

	s, err := refstore.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("a", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	copyDir(t, backup, dir) // the Pebble files are back, next to refs.sqlite

	wantRecords(t, open(t, dir), map[string]string{"a": "new"})
	if exists(t, filepath.Join(dir, "LOCK")) {
		t.Error("the resumed cleanup left Pebble's LOCK file behind")
	}
}

// An old binary that tries a migrated store creates LOCK before it fails on
// the poison marker.
func TestStrayPebbleLockIsRemoved(t *testing.T) {
	dir := t.TempDir()
	legacyStore(t, dir, map[string]string{"a": "1"}, nil)
	open(t, dir).Close()
	if err := os.WriteFile(filepath.Join(dir, "LOCK"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wantRecords(t, open(t, dir), map[string]string{"a": "1"})
	if exists(t, filepath.Join(dir, "LOCK")) {
		t.Error("the stray LOCK file was not removed")
	}
}

func TestFilesThatAreNotPebblesStayPut(t *testing.T) {
	dir := t.TempDir()
	legacyStore(t, dir, map[string]string{"a": "1"}, nil)
	if err := os.WriteFile(filepath.Join(dir, "NOTES.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	open(t, dir).Close()
	if !exists(t, filepath.Join(dir, "NOTES.txt")) {
		t.Error("a file that is not Pebble's was moved away")
	}
}

func TestConcurrentOpensMigrateOnce(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{"a": "1", "b": "2"}
	legacyStore(t, dir, want, nil)
	const n = 4
	stores := make([]*refstore.Store, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { stores[i], errs[i] = refstore.Open(dir, false) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		t.Cleanup(func() { stores[i].Close() })
		wantRecords(t, stores[i], want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go get github.com/cockroachdb/pebble/v2@v2.1.6 && go test ./refstore -run 'Migrat|Legacy|Pebble|Cleanup|Stray'`
Expected: FAIL — the legacy records are not visible (`records = map[], want …`).

- [ ] **Step 3: Implement**

`refstore/migrate.go`:

```go
package refstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/amber-store/core/refstore/internal/refsdb"
	"github.com/cockroachdb/pebble/v2"
	"golang.org/x/sys/unix"
)

// Releases before the SQLite store kept references in a Pebble DB in the same
// directory. Open imports such a store once. This file is the only importer
// of Pebble and goes away with migration support.
const (
	// legacyLock exists in every Pebble directory, so one stat per Open
	// decides whether there is anything to do.
	legacyLock = "LOCK"
	// legacyManifest prefixes the marker naming Pebble's current manifest:
	// its presence means a Pebble store, not just debris.
	legacyManifest = "marker.manifest."
	// legacyDir keeps the Pebble files after the import, as a backup.
	legacyDir = "pebble-migrated"
	// migrateLock serializes concurrent opens of a store being migrated.
	migrateLock = "migrate.lock"
	// poisonFile makes Pebble refuse the directory ("unknown format major
	// version 999"). Without it a binary that predates this change would
	// create an empty Pebble store here, report no references, and a gc run
	// from it would reap every object.
	poisonFile = "marker.format-version.999999.999"
)

// discardLogger silences pebble's internal logging.
type discardLogger struct{}

func (discardLogger) Infof(string, ...any)  {}
func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf("refstore: pebble fatal: "+format, args...))
}

// migrateLegacy imports a Pebble reference store found in dir and retires
// its files. The rename of the finished database to refs.sqlite is the
// commit point: before it a crash leaves the Pebble store untouched, after
// it the import is never repeated, so later writes are never overwritten.
func migrateLegacy(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, legacyLock)); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	unlock, err := lockMigration(dir)
	if err != nil {
		return err
	}
	defer unlock()

	// Read the directory only now: another process may have finished the
	// migration while this one waited for the lock.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	var files []string
	legacy := false
	for _, e := range entries {
		if !e.IsDir() && isPebbleFile(e.Name()) {
			files = append(files, e.Name())
			legacy = legacy || strings.HasPrefix(e.Name(), legacyManifest)
		}
	}
	if !legacy {
		// Debris: a Pebble-based binary tried the migrated store, created
		// its lock file and failed on the poison marker.
		if err := os.Remove(filepath.Join(dir, legacyLock)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("refstore: %w", err)
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, dbFile)); errors.Is(err, fs.ErrNotExist) {
		if err := importPebble(dir); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	return retireLegacy(dir, files)
}

func isPebbleFile(name string) bool {
	switch {
	case name == poisonFile:
		return false
	case name == legacyLock, name == "CURRENT":
		return true
	}
	for _, prefix := range []string{"MANIFEST-", "OPTIONS-", "marker.", "temporary."} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, suffix := range []string{".log", ".sst", ".dbtmp"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func lockMigration(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, migrateLock), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("refstore: migration lock: %w", err)
	}
	for {
		if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != unix.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("refstore: migration lock: %w", err)
	}
	return func() { f.Close() }, nil // closing the file releases the lock
}

// importPebble copies every record into a new database and renames it into
// place. The copy is built with a rollback journal and full syncs, so once
// closed it is a single durable file that a rename can publish. Pebble opens
// read-only, which still takes its directory lock: a store in use by an old
// binary fails the migration instead of being copied mid-flight.
func importPebble(dir string) (err error) {
	pdb, err := pebble.Open(dir, &pebble.Options{ReadOnly: true, Logger: discardLogger{}})
	if err != nil {
		return fmt.Errorf("refstore: opening the Pebble store in %s to migrate it: %w", dir, err)
	}
	defer pdb.Close()

	tmp := filepath.Join(dir, dbFile+".tmp")
	for _, stale := range []string{tmp, tmp + "-journal"} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("refstore: %w", err)
		}
	}
	db, err := openDB(tmp, true, false)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("refstore: migrating: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	q := refsdb.New(tx)
	it, err := pdb.NewIter(&pebble.IterOptions{})
	if err != nil {
		return fmt.Errorf("refstore: migrating: %w", err)
	}
	for it.First(); it.Valid(); it.Next() {
		if err = q.PutRecord(ctx, refsdb.PutRecordParams{Name: blob(slices.Clone(it.Key())), Record: blob(slices.Clone(it.Value()))}); err != nil {
			it.Close()
			return fmt.Errorf("refstore: migrating: %w", err)
		}
	}
	if err = errors.Join(it.Error(), it.Close()); err != nil {
		return fmt.Errorf("refstore: reading the Pebble store: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("refstore: migrating: %w", err)
	}
	if err = db.Close(); err != nil {
		return fmt.Errorf("refstore: migrating: %w", err)
	}
	if err = os.Rename(tmp, filepath.Join(dir, dbFile)); err != nil {
		return fmt.Errorf("refstore: migrating: %w", err)
	}
	return syncDir(dir)
}

// retireLegacy writes the poison marker and moves Pebble's files aside: data
// files first, the manifest markers next, LOCK last. A crash leaves LOCK —
// and, until the very end, a manifest marker — in place, so the next Open
// comes back here and finishes.
func retireLegacy(dir string, files []string) error {
	if err := os.WriteFile(filepath.Join(dir, poisonFile), nil, 0o644); err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	aside := filepath.Join(dir, legacyDir)
	if err := os.MkdirAll(aside, 0o755); err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	rank := func(name string) int {
		switch {
		case name == legacyLock:
			return 2
		case strings.HasPrefix(name, legacyManifest):
			return 1
		}
		return 0
	}
	slices.SortStableFunc(files, func(a, b string) int { return rank(a) - rank(b) })
	for _, name := range files {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(aside, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("refstore: retiring the Pebble store: %w", err)
		}
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("refstore: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("refstore: syncing %s: %w", dir, err)
	}
	return nil
}
```

In `refstore/refstore.go`, `Open` becomes:

```go
func Open(dir string, sync bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("refstore: creating %s: %w", dir, err)
	}
	if err := migrateLegacy(dir); err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(dir, dbFile), sync, true)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, q: refsdb.New(db)}, nil
}
```

and its doc comment gains: "A Pebble store left by an earlier release is imported first; see migrate.go."

- [ ] **Step 4: Run the tests**

Run: `go mod tidy && go test ./refstore && go test -race ./refstore && go vet ./refstore/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum refstore
git commit -m "refstore: import Pebble stores on first open; poison the directory for old binaries"
```

---

### Task 4: `--expect` on `ref set` and `ref rm`

**Files:**
- Modify: `cmd/amber-store/ref.go`, `cmd/amber-store/commit.go:154`, `cmd/amber-store/ingest.go:127`
- Test: `cmd/amber-store/e2e_test.go`

**Interfaces:**
- Consumes: `refstore.CompareAndSwap`, `Create`, `CompareAndDelete`, `ErrConflict`, `ErrNotFound`.
- Produces: `type expectation struct{ conditional, absent bool; key key.Key }`; `parseExpect(s string, allowNone bool) (expectation, error)`; `putRef(coll, refs, name, root, raw, exp expectation) error`; `rmRef(coll, refs, name, exp expectation) error`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/amber-store/e2e_test.go`:

```go
func TestE2E_RefExpect(t *testing.T) {
	store := t.TempDir()
	ingest := func(extra string) string {
		src := t.TempDir()
		writeFixture(t, src)
		if err := os.WriteFile(filepath.Join(src, "extra.txt"), []byte(extra), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := runApp(t, "--store", store, "ingest", "--no-progress", src)
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		return strings.TrimSpace(out)
	}
	root1, root2 := ingest("one"), ingest("two")
	ref := func(args ...string) (string, error) {
		out, err := runApp(t, append([]string{"--store", store, "ref"}, args...)...)
		return strings.TrimSpace(out), err
	}
	wantAt := func(want string) {
		t.Helper()
		if got, err := ref("get", "r"); err != nil || got != want {
			t.Fatalf("ref get r = %q, %v; want %s", got, err, want)
		}
	}

	if _, err := ref("set", "--expect", "none", "r", root1); err != nil {
		t.Fatalf("create with --expect none: %v", err)
	}
	if _, err := ref("set", "--expect", "none", "r", root2); err == nil {
		t.Fatal("--expect none overwrote an existing reference")
	}
	wantAt(root1)
	if _, err := ref("set", "--expect", root2, "r", root2); err == nil {
		t.Fatal("a stale --expect moved the reference")
	}
	wantAt(root1)
	// A flag after the positionals is not parsed as a flag; it must fail
	// rather than turn into an unconditional set.
	if _, err := ref("set", "r", root2, "--expect", root2); err == nil {
		t.Fatal("a misplaced --expect was accepted")
	}
	wantAt(root1)
	if _, err := ref("set", "--expect", root1, "r", root2); err != nil {
		t.Fatalf("set with the right --expect: %v", err)
	}
	wantAt(root2)

	if _, err := ref("rm", "--expect", "none", "r"); err == nil {
		t.Fatal("ref rm accepted --expect none")
	}
	if _, err := ref("rm", "--expect", root1, "r"); err == nil {
		t.Fatal("a stale --expect deleted the reference")
	}
	wantAt(root2)
	if _, err := ref("rm", "--expect", root2, "r"); err != nil {
		t.Fatalf("rm with the right --expect: %v", err)
	}
	if _, err := ref("get", "r"); err == nil {
		t.Fatal("the reference survived ref rm")
	}
	if _, err := ref("set", "--expect", root1, "gone", root1); err == nil {
		t.Fatal("--expect KEY created a reference that did not exist")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/amber-store -run TestE2E_RefExpect`
Expected: FAIL with `flag provided but not defined: -expect`.

- [ ] **Step 3: Implement**

In `cmd/amber-store/ref.go`, give `set` and `rm` the flag:

```go
			{
				Name:      "set",
				Usage:     "create or overwrite reference NAME pointing at KEY",
				ArgsUsage: "NAME KEY",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "expect", Usage: "only if NAME currently points at `OLD`; 'none': only if NAME does not exist"},
				},
				Action: runRefSet,
			},
			{
				Name:      "rm",
				Usage:     "delete reference NAME",
				ArgsUsage: "NAME",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "expect", Usage: "only if NAME currently points at `OLD`"},
				},
				Action: runRefRm,
			},
```

Add below `refCommand`:

```go
// expectation is the precondition of an optimistic reference write: the
// reference must not have moved since the caller last looked.
type expectation struct {
	conditional bool    // false: write unconditionally
	absent      bool    // the reference must not exist
	key         key.Key // otherwise it must point here
}

func parseExpect(s string, allowNone bool) (expectation, error) {
	switch {
	case s == "":
		return expectation{}, nil
	case s == "none" && allowNone:
		return expectation{conditional: true, absent: true}, nil
	case s == "none":
		return expectation{}, errors.New("--expect none: a delete cannot expect the reference to be absent")
	}
	k, err := parseHexKey(s)
	if err != nil {
		return expectation{}, fmt.Errorf("--expect: %w", err)
	}
	return expectation{conditional: true, key: k}, nil
}

// explain turns the store's sentinel errors into what the user expected.
func (e expectation) explain(name string, err error) error {
	switch {
	case errors.Is(err, refstore.ErrConflict) && e.absent:
		return fmt.Errorf("reference %q already exists: %w", name, err)
	case errors.Is(err, refstore.ErrConflict):
		return fmt.Errorf("reference %q does not point at %s: %w", name, e.key, err)
	case errors.Is(err, refstore.ErrNotFound) && e.conditional:
		return fmt.Errorf("reference %q does not exist, expected it at %s: %w", name, e.key, err)
	}
	return err
}
```

`runRefSet` parses the flag before opening the store and passes it on:

```go
	exp, err := parseExpect(c.String("expect"), true)
	if err != nil {
		return err
	}
	...
	err = putRef(coll, refs, name, k, raw, exp)
```

`runRefRm` likewise with `parseExpect(c.String("expect"), false)` and `rmRef(coll, refs, c.Args().First(), exp)`.

`putRef` takes `exp expectation` and replaces its `refs.Put` call:

```go
	var putErr error
	switch {
	case !exp.conditional:
		putErr = refs.Put(name, raw)
	case exp.absent:
		putErr = refs.Create(name, raw)
	default:
		putErr = refs.CompareAndSwap(name, exp.key, raw)
	}
	if putErr != nil {
		abort()
		return exp.explain(name, putErr)
	}
```

`rmRef` takes `exp expectation` and replaces its `refs.Delete` call:

```go
	if exp.conditional {
		err = refs.CompareAndDelete(name, exp.key)
	} else {
		err = refs.Delete(name)
	}
	if err != nil {
		return exp.explain(name, err)
	}
```

Its earlier `refs.Get` stays (it supplies the root for `ReleaseRef`), wrapped as `return exp.explain(name, err)` so a conditional delete of an absent name reads well. Update `putRef`'s comment: calls for one name no longer need serializing when an expectation is given.

In `cmd/amber-store/commit.go` and `cmd/amber-store/ingest.go`, pass `expectation{}` as the new last argument of `putRef`.

- [ ] **Step 4: Run the tests**

Run: `go test ./cmd/... && go vet ./cmd/...`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add cmd/amber-store
git commit -m "amber-store: ref set/rm --expect for optimistic reference updates"
```

---

### Task 5: tooling and documentation

**Files:**
- Modify: `flake.nix`, `.github/workflows/test.yml`, `architecture/references.md`, `README.md`

- [ ] **Step 1: sqlc in the dev shell and in CI**

`flake.nix`: the package list becomes

```nix
          # nodejs builds the embedded admin SPA (go generate ./cmd/amber-store);
          # sqlc regenerates refstore/internal/refsdb (go generate ./refstore)
          packages = with pkgs; [ go nodejs python3 sqlc ];
```

`.github/workflows/test.yml`: after the `setup-node` step add

```yaml
      - uses: sqlc-dev/setup-sqlc@v4
        with:
          sqlc-version: '1.31.1'
      - run: sqlc diff
        working-directory: refstore
```

- [ ] **Step 2: `architecture/references.md`**

Replace the **Mutability** paragraph and the **Storage** and **CLI** sections with the text below (the record tables above them are unchanged):

```markdown
**Mutability:** references are overwritable; a plain put for an existing name
replaces the record unconditionally. There is no history. A writer that must
not overwrite somebody else's move uses the **optimistic** forms instead:
move the reference only if it still points at the key the writer last saw,
create it only if it does not exist, delete it only if it still points at a
given key. The comparison is on the pointed-to key, not on the whole record.
A failed expectation changes nothing and is reported to the caller, who
re-reads and decides.

## Storage

References live in a SQLite database (the `refstore` package), conventionally
`<store-dir>/refs/refs.sqlite` next to the object store. The file is the
interchange format: every implementation reads and writes the same database.

| Property | Value |
| --- | --- |
| `PRAGMA application_id` | `0x616D6272` (`"ambr"`) |
| `PRAGMA user_version` | number of schema migrations applied; `1` today |
| Journal mode | WAL, mandatory |
| Schema at version 1 | `CREATE TABLE refs (name BLOB NOT NULL PRIMARY KEY, record BLOB NOT NULL) WITHOUT ROWID` |

`name` is the reference name's bytes and `record` the CBOR record verbatim;
both are BLOBs, never NULL or TEXT, so listing (`ORDER BY name`) is bytewise
lexicographic. An implementation refuses a file whose application id differs
or whose version is newer than it knows.

The schema is built by numbered SQL files (`refstore/migrations/`), applied in
order inside one `BEGIN IMMEDIATE` transaction that ends by setting
`user_version`; a fresh database starts at version 0. Files never change once
released, and a change must stay compatible with the release before it, since
a process that opened the store earlier keeps running its old queries.

WAL mode makes the store multi-process: any number of processes may hold it
open, readers work from a snapshot and never block, and write transactions
run one at a time — a second writer waits (30 s busy timeout). A batch is one
transaction. Write durability follows the store's sync flag:
`synchronous=FULL` with `fullfsync` and `checkpoint_fullfsync` on, or
`synchronous=NORMAL` without.

**Stores written before this format** kept references in a Pebble DB in the
same directory. The first open imports them into `refs.sqlite`, moves the
Pebble files to `refs/pebble-migrated/` (a backup the operator may delete)
and leaves `marker.format-version.999999.999` behind. That marker makes
Pebble refuse the directory, so a binary that predates the change fails
loudly instead of creating an empty store — from which a `gc run` would reap
every object. `refs/migrate.lock` serializes concurrent first opens.

## CLI

```sh
amber-store ingest --ref NAME DIR    # ingest and name the root
amber-store ref set NAME KEY         # name an existing key
amber-store ref set --expect OLD NAME KEY   # only if NAME still points at OLD
amber-store ref set --expect none NAME KEY  # only if NAME does not exist
amber-store ref list                 # name, key, date, user
amber-store ref get NAME             # print the key NAME points at
amber-store ref rm NAME              # delete the name; objects stay
amber-store ref rm --expect OLD NAME # only if NAME still points at OLD
amber-store ls ref:NAME[@PATH]       # any KEY[/PATH] argument accepts this
```

`--expect` goes before the positional arguments.
```

- [ ] **Step 3: `README.md`**

Line 78 becomes:

```markdown
| `refstore` | SQLite-backed (WAL, multi-process) name → record map for references, with optimistic updates. |
```

and after the `ref set NAME KEY` line of the CLI listing add:

```sh
amber-store --store ./store ref set --expect OLD NAME KEY  # move NAME only if it still points at OLD ('none': only create)
```

- [ ] **Step 4: Verify as CI does**

Run:

```bash
go generate ./refstore && git diff --exit-code refstore/internal
(cd refstore && sqlc diff)
go test ./...
go test -race ./packstore ./refstore ./gc
go vet ./...
gofmt -l .
```

Expected: no diff, no sqlc output, every package `ok`, no vet or gofmt output.

- [ ] **Step 5: Commit**

```bash
git add flake.nix .github/workflows/test.yml architecture/references.md README.md
git commit -m "docs, ci: the SQLite reference store; sqlc in the dev shell and a drift check"
```
