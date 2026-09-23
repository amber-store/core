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
