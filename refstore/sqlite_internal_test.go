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
