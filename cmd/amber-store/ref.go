package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/amber-store/core/gc"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/reference"
	"github.com/amber-store/core/refstore"
	"github.com/urfave/cli/v2"
)

func refCommand() *cli.Command {
	return &cli.Command{
		Name:  "ref",
		Usage: "manage references: named pointers to root keys",
		Subcommands: []*cli.Command{
			{
				Name:   "list",
				Usage:  "list every reference: name, key, creation time, creator",
				Action: runRefList,
			},
			{
				Name:      "get",
				Usage:     "print the key a reference points at",
				ArgsUsage: "NAME",
				Action:    runRefGet,
			},
			{
				Name:      "set",
				Usage:     "create or overwrite reference NAME pointing at KEY",
				ArgsUsage: "NAME KEY",
				Action:    runRefSet,
			},
			{
				Name:      "rm",
				Usage:     "delete reference NAME",
				ArgsUsage: "NAME",
				Action:    runRefRm,
			},
		},
	}
}

func runRefList(c *cli.Context) error {
	if c.NArg() != 0 {
		return fmt.Errorf("ref list takes no arguments, got %d", c.NArg())
	}
	objects, refs, err := openStore(c)
	if err != nil {
		return err
	}
	defer closeStore(objects, refs)
	records, err := refs.All()
	if err != nil {
		return err
	}
	for _, r := range records {
		rec, err := reference.Decode(r.Data)
		if err != nil {
			return fmt.Errorf("reference %q: %w", r.Name, err)
		}
		k, err := key.Parse(rec.Key)
		if err != nil {
			return fmt.Errorf("reference %q: stored key: %w", r.Name, err)
		}
		line := fmt.Sprintf("%s %s %s", rec.Name, k, time.Unix(0, rec.CreatedAt).UTC().Format(time.RFC3339))
		if rec.User != "" {
			line += " " + rec.User
		}
		if _, err := fmt.Fprintln(c.App.Writer, line); err != nil {
			return err
		}
	}
	return nil
}

func runRefGet(c *cli.Context) error {
	if c.NArg() != 1 {
		return fmt.Errorf("ref get requires exactly one NAME argument, got %d", c.NArg())
	}
	objects, refs, err := openStore(c)
	if err != nil {
		return err
	}
	defer closeStore(objects, refs)
	k, _, err := resolveSpec(refs, "ref:"+c.Args().First())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.App.Writer, k.String())
	return err
}

func runRefSet(c *cli.Context) error {
	if c.NArg() != 2 {
		return fmt.Errorf("ref set requires NAME KEY arguments, got %d", c.NArg())
	}
	name := c.Args().Get(0)
	k, err := parseHexKey(c.Args().Get(1))
	if err != nil {
		return err
	}
	rec := reference.Reference{
		Name:      name,
		Key:       k[:],
		CreatedAt: time.Now().UnixNano(),
	}
	raw, err := rec.Encode()
	if err != nil {
		return err
	}
	objects, refs, err := openStore(c)
	if err != nil {
		return err
	}
	coll, err := openCollector(c, objects, refs, gc.Options{})
	if err != nil {
		closeStore(objects, refs)
		return err
	}
	err = putRef(coll, refs, name, k, raw)
	return errors.Join(err, coll.Close(), closeStore(objects, refs))
}

func runRefRm(c *cli.Context) error {
	if c.NArg() != 1 {
		return fmt.Errorf("ref rm requires exactly one NAME argument, got %d", c.NArg())
	}
	objects, refs, err := openStore(c)
	if err != nil {
		return err
	}
	coll, err := openCollector(c, objects, refs, gc.Options{})
	if err != nil {
		closeStore(objects, refs)
		return err
	}
	err = rmRef(coll, refs, c.Args().First())
	return errors.Join(err, coll.Close(), closeStore(objects, refs))
}

// putRef writes a reference under the collector's removal lock: the closure
// is reused or walked — a missing object fails the write, naming it — the
// record is stored, and an overwritten root is released. This is the
// optimistic reference PUT: on a 404 the caller re-sends the missing
// objects and retries.
//
// Calls for one name must be serialized by the caller (the one-shot CLI
// is); the read-old → prepare → put → release sequence is not atomic
// against a concurrent writer of the same name.
func putRef(coll *gc.Collector, refs *refstore.Store, name string, root key.Key, raw []byte) error {
	var old *key.Key
	if prev, err := refs.Get(name); err == nil {
		prevRef, err := reference.Decode(prev)
		if err != nil {
			return fmt.Errorf("existing reference %q: %w", name, err)
		}
		k, err := key.Parse(prevRef.Key)
		if err != nil {
			return fmt.Errorf("existing reference %q: %w", name, err)
		}
		old = &k
	} else if !errors.Is(err, refstore.ErrNotFound) {
		return err
	}
	commit, abort, err := coll.PrepareRef(root)
	if err != nil {
		return err
	}
	if err := refs.Put(name, raw); err != nil {
		abort()
		return err
	}
	commit()
	if old != nil {
		return coll.ReleaseRef(*old)
	}
	return nil
}

// rmRef deletes a reference and releases its root: the tails leave the
// union; the closure file goes if no other name shares the root. No walk.
func rmRef(coll *gc.Collector, refs *refstore.Store, name string) error {
	prev, err := refs.Get(name)
	if err != nil {
		return err
	}
	ref, err := reference.Decode(prev)
	if err != nil {
		return fmt.Errorf("reference %q: %w", name, err)
	}
	root, err := key.Parse(ref.Key)
	if err != nil {
		return fmt.Errorf("reference %q: %w", name, err)
	}
	if err := refs.Delete(name); err != nil {
		return err
	}
	return coll.ReleaseRef(root)
}
