// Package refstore persists reference records in a Pebble DB: name bytes →
// CBOR record bytes, stored verbatim. It is a dumb KV layer; record
// validation belongs to the daemon and the reference package.
package refstore

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/cockroachdb/pebble/v2"
)

// ErrNotFound is returned by Get and Delete for an absent name.
var ErrNotFound = errors.New("refstore: reference not found")

// Store is a Pebble-backed name→record map. It is safe for concurrent use.
// Readers (Get, All) are lock-free; writes (Put, Delete) are serialized via
// writeMu so that Delete's existence check is linearizable against other
// writers.
type Store struct {
	db        *pebble.DB
	writeOpts *pebble.WriteOptions
	writeMu   sync.Mutex
}

// discardLogger silences pebble's internal logging.
type discardLogger struct{}

func (discardLogger) Infof(string, ...any)  {}
func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf("refstore: pebble fatal: "+format, args...))
}

// Open opens (creating if missing) the refs DB at dir. sync selects the
// write durability, matching the daemon's --sync flag.
func Open(dir string, sync bool) (*Store, error) {
	db, err := pebble.Open(dir, &pebble.Options{Logger: discardLogger{}})
	if err != nil {
		return nil, fmt.Errorf("refstore: opening pebble: %w", err)
	}
	wo := pebble.Sync
	if !sync {
		wo = pebble.NoSync
	}
	return &Store{db: db, writeOpts: wo}, nil
}

// Put stores record under name, overwriting unconditionally.
func (s *Store) Put(name string, record []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.Set([]byte(name), record, s.writeOpts)
}

// PutBatch publishes records atomically, using the configured write durability.
// When names repeat, the last record wins. An empty batch changes nothing.
func (s *Store) PutBatch(records []Record) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	b := s.db.NewBatch()
	defer b.Close()
	for _, record := range records {
		if err := b.Set([]byte(record.Name), record.Data, nil); err != nil {
			return err
		}
	}
	return b.Commit(s.writeOpts)
}

// Get returns the record stored under name, or ErrNotFound.
func (s *Store) Get(name string) ([]byte, error) {
	v, closer, err := s.db.Get([]byte(name))
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := slices.Clone(v)
	if err := closer.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes name, or returns ErrNotFound if absent. The existence check
// and the delete are serialized against other writes via writeMu, so
// concurrent Deletes of the same name report ErrNotFound to all but one
// caller.
func (s *Store) Delete(name string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.Get(name); err != nil {
		return err
	}
	return s.db.Delete([]byte(name), s.writeOpts)
}

// Record is one (name, record-bytes) pair from All.
type Record struct {
	Name string
	Data []byte
}

// All returns every record in lexicographic name order.
func (s *Store) All() ([]Record, error) {
	it, err := s.db.NewIter(&pebble.IterOptions{})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var recs []Record
	for it.First(); it.Valid(); it.Next() {
		recs = append(recs, Record{
			Name: string(it.Key()),
			Data: slices.Clone(it.Value()),
		})
	}
	if err := it.Error(); err != nil {
		return nil, err
	}
	return recs, nil
}

// Wipe deletes every record (the store-wipe operation). Iterate-and-delete
// rather than a range tombstone: names are arbitrary bytes so no literal
// upper bound covers the whole keyspace, and ref counts are small.
func (s *Store) Wipe() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	it, err := s.db.NewIter(&pebble.IterOptions{})
	if err != nil {
		return err
	}
	b := s.db.NewBatch()
	defer b.Close()
	for it.First(); it.Valid(); it.Next() {
		if err := b.Delete(slices.Clone(it.Key()), nil); err != nil {
			it.Close()
			return err
		}
	}
	if err := it.Error(); err != nil {
		it.Close()
		return err
	}
	if err := it.Close(); err != nil {
		return err
	}
	return b.Commit(s.writeOpts)
}

// Close closes the DB.
func (s *Store) Close() error {
	return s.db.Close()
}
