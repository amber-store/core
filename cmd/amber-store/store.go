package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/amber-store/core/gc"
	"github.com/amber-store/core/packstore"
	"github.com/amber-store/core/refstore"
	"github.com/urfave/cli/v2"
)

// openStore opens (creating as needed) the store directory named by the
// --store flag or $AMBER_STORE: <dir>/packstore holds the objects,
// <dir>/refs the references DB. Stores are single-owner: never open one
// directory from two live processes.
func openStore(c *cli.Context) (*packstore.Store, *refstore.Store, error) {
	dir := c.String("store")
	if dir == "" {
		return nil, nil, fmt.Errorf("no store directory: set --store or $AMBER_STORE")
	}
	objects, err := packstore.Open(filepath.Join(dir, "packstore"),
		packstore.WithSync(true), packstore.WithSegmentSize(c.Int64("segment-size")))
	if err != nil {
		return nil, nil, err
	}
	refs, err := refstore.Open(filepath.Join(dir, "refs"), true)
	if err != nil {
		objects.Close()
		return nil, nil, err
	}
	return objects, refs, nil
}

// closeStore closes both halves, keeping every error.
func closeStore(objects *packstore.Store, refs *refstore.Store) error {
	return errors.Join(refs.Close(), objects.Close())
}

// openCollector opens the collector next to an already-open store pair;
// <dir>/closures holds the closure files. Close it before closeStore.
func openCollector(c *cli.Context, objects *packstore.Store, refs *refstore.Store, opts gc.Options) (*gc.Collector, error) {
	return gc.Open(filepath.Join(c.String("store"), "closures"), objects, refs, opts)
}
