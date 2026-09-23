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
	// Unconditional, and a no-op once committed: a panic must not leave the
	// write lock held, which would wedge every writer in every process.
	defer tx.Rollback()
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
