package refstore

import (
	"context"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
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
		fsys := fstest.MapFS{"m": &fstest.MapFile{Mode: fs.ModeDir | 0o755}}
		for _, f := range files {
			fsys["m/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		if _, err := loadMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: loadMigrations accepted %v", name, files)
		}
	}
	good := fstest.MapFS{
		"m/0002_b.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
		"m/0001_a.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}
	set, err := loadMigrations(good, "m")
	if err != nil || len(set) != 2 || set[0].name != "0001_a.sql" || set[1].sql != "SELECT 2;" {
		t.Errorf("loadMigrations(good) = %+v, %v", set, err)
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
	conn.Close()
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
	conn.Close()
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

// A negative version used to panic inside the write transaction, and the
// unwinding then blocked forever with the write lock held.
func TestNegativeSchemaVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = -1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		s, err := Open(dir, false)
		if err == nil {
			s.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a version") {
			t.Fatalf("Open(schema version -1) = %v, want it refused", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Open hangs on a negative schema version")
	}
}
