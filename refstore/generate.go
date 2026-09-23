package refstore

// The queries in queries.sql are compiled against the schema in migrations/
// into internal/refsdb. sqlc comes from the Nix dev shell; the output is
// committed, so building needs no sqlc.
//go:generate sqlc generate
