package refstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite" // the pure-Go SQLite driver, registered as "sqlite"
)

const (
	// dbFile is the database's name inside the store directory.
	dbFile = "refs.sqlite"
	// applicationID marks the file as a reference store: "ambr".
	applicationID = 0x616d6272
	// maxConns caps the connection pool; each connection is a file handle.
	maxConns = 8
	// sqliteBusy is SQLite's primary result code SQLITE_BUSY.
	sqliteBusy = 5
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
	conn, err := connect(ctx, db)
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

// connect takes the pool's first connection, retrying while SQLite reports
// the database busy. The driver applies the DSN's pragmas as it connects, and
// switching a database into WAL mode — which the first open of a new or a
// freshly imported store does — takes an exclusive lock for which SQLite does
// not run the busy handler: of several processes opening such a store at
// once, all but one fail immediately. They retry here, for as long as a
// writer would wait. Once the file is in WAL mode the pragma changes nothing
// and never waits.
func connect(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	deadline := time.Now().Add(busyTimeout)
	for delay := time.Millisecond; ; delay = min(2*delay, 50*time.Millisecond) {
		conn, err := db.Conn(ctx)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return conn, err
		}
		time.Sleep(delay)
	}
}

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqliteBusy
}

// blob maps a nil slice to an empty one: the driver binds nil as NULL, and
// names and records are always BLOBs.
func blob(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}
