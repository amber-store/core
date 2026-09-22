package gc

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

// storeDirTree stores storeTree's file under a one-entry directory and
// returns the directory root (a commit's tree must be a directory) and
// every key.
func storeDirTree(t *testing.T, st *packstore.Store, seed string, n int) (key.Key, []key.Key) {
	t.Helper()
	file, all := storeTree(t, st, seed, n)
	leaf, err := fstree.EncodeDirLeaf([]fstree.Entry{{Name: []byte("f"), Mode: 0o100644, ContentKey: file[:]}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(leaf.Key, leaf.Bytes); err != nil {
		t.Fatal(err)
	}
	return leaf.Key, append(all, leaf.Key)
}

// newCommit builds a commit of tree without storing it.
func newCommit(t *testing.T, tree key.Key, parents ...key.Key) (key.Key, []byte) {
	t.Helper()
	id := commit.Identity{Name: "Ann", When: 1}
	k, b, err := commit.Commit{Tree: tree, Parents: parents, Author: id, Committer: id, Message: "m"}.Object()
	if err != nil {
		t.Fatal(err)
	}
	return k, b
}

func storeCommit(t *testing.T, st *packstore.Store, tree key.Key, parents ...key.Key) key.Key {
	t.Helper()
	k, b := newCommit(t, tree, parents...)
	if err := st.Put(k, b); err != nil {
		t.Fatal(err)
	}
	return k
}

func countGone(t *testing.T, st *packstore.Store, keys []key.Key) int {
	t.Helper()
	gone := 0
	for _, k := range keys {
		if _, err := st.Get(k); errors.Is(err, packstore.ErrNotFound) {
			gone++
		}
	}
	return gone
}

func TestCommitHistoryStaysLive(t *testing.T) {
	ts := newTestStore(t, 4<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	treeOld, keysOld := storeDirTree(t, ts.objects, "old", 40)
	treeNew, keysNew := storeDirTree(t, ts.objects, "new", 40)
	_, keysDead := storeDirTree(t, ts.objects, "dead", 40) // never referenced
	first := storeCommit(t, ts.objects, treeOld)
	tip := storeCommit(t, ts.objects, treeNew, first)
	putTestRef(t, c, ts.refs, "main", tip)

	// Why walks through the commits: the old tree is held by the branch.
	names, err := c.Why(keysOld[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"main"}) {
		t.Errorf("Why(old blob) = %v, want [main]", names)
	}

	backdatePacks(t, ts)
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) == 0 {
		t.Fatalf("nothing reaped: %+v", stats)
	}
	live := append(append(slices.Clone(keysOld), keysNew...), first, tip)
	for _, k := range live {
		if _, err := ts.objects.Get(k); err != nil {
			t.Fatalf("key %s reachable from the branch: %v", k, err)
		}
	}
	if countGone(t, ts.objects, keysDead) == 0 {
		t.Error("no unreferenced key was collected")
	}

	// Dropping the branch makes the whole history garbage.
	rmTestRef(t, c, ts.refs, "main", tip)
	backdatePacks(t, ts)
	if _, err := c.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if countGone(t, ts.objects, keysOld) == 0 {
		t.Error("no key of the old commit's tree was collected after the branch went")
	}
}

func TestPrepareRefMissingAncestorFails(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	tree, _ := storeDirTree(t, ts.objects, "tree", 4)
	absent, _ := newCommit(t, tree) // built, never stored
	tip := storeCommit(t, ts.objects, tree, absent)
	if _, _, err := c.PrepareRef(tip); err == nil {
		t.Fatal("PrepareRef accepted a commit whose parent is missing")
	}
}
