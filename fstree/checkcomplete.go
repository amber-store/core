package fstree

import (
	"fmt"
	"runtime"

	"github.com/amber-store/core/key"
	"golang.org/x/sync/errgroup"
)

// MissingObjectError reports a leaf object found absent by CheckComplete.
type MissingObjectError struct {
	Key key.Key
}

func (e *MissingObjectError) Error() string {
	return fmt.Sprintf("fstree: object %s is missing", e.Key)
}

// CheckComplete verifies that every object reachable from root exists and
// returns the visited keys — root first, then discovery order, each once.
// The tree is walked breadth-first, checking each level's objects with up
// to jobs concurrent lookups (jobs <= 0 means GOMAXPROCS): interior nodes
// are read with get — a failed read surfaces as the wrapped get error — and
// leaves (Blob and XattrSet objects, and git blobs a tree or tag says are blobs:
// see Links) are tested with has — an absent leaf surfaces as a
// *MissingObjectError. On error the visited list is nil.
func CheckComplete(root key.Key, get func(key.Key) ([]byte, error), has func(key.Key) (bool, error), jobs int) ([]key.Key, error) {
	if jobs <= 0 {
		jobs = runtime.GOMAXPROCS(0)
	}
	seen := visits{}
	first := LinkTo(root)
	seen.visit(first)
	visited := []key.Key{root}
	frontier := []Link{first}
	for len(frontier) > 0 {
		children := make([][]Link, len(frontier))
		g := new(errgroup.Group)
		g.SetLimit(jobs)
		for i, l := range frontier {
			g.Go(func() error {
				if l.Leaf {
					ok, err := has(l.Key)
					if err != nil {
						return err
					}
					if !ok {
						return &MissingObjectError{Key: l.Key}
					}
					return nil
				}
				data, err := get(l.Key)
				if err != nil {
					return fmt.Errorf("fstree: reading %s: %w", l.Key, err)
				}
				children[i], err = Links(l.Key, data)
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		var next []Link
		for _, links := range children {
			for _, l := range links {
				isNew, do := seen.visit(l)
				if isNew {
					visited = append(visited, l.Key)
				}
				if do {
					next = append(next, l)
				}
			}
		}
		frontier = next
	}
	return visited, nil
}
