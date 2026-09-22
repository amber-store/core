package fstree_test

import (
	"errors"
	"testing"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

// commitObj builds a commit of tree with the given parents.
func commitObj(t *testing.T, msg string, tree key.Key, parents ...key.Key) fstree.Object {
	t.Helper()
	id := commit.Identity{Name: "Ann", Email: "ann@example.com", When: 1, TZOffset: 60}
	c := commit.Commit{Tree: tree, Parents: parents, Author: id, Committer: id, Message: msg}
	k, b, err := c.Object()
	if err != nil {
		t.Fatal(err)
	}
	return fstree.Object{Key: k, Bytes: b}
}

// history builds two trees and a diamond of commits over them:
//
//	first(treeA) <- left(treeB), right(treeA) <- tip(treeB), a merge.
//
// It returns every object, and the named ones.
func history(t *testing.T) (all []fstree.Object, first, tip fstree.Object) {
	t.Helper()
	treeA := completeTree(t)
	rootA := treeA[len(treeA)-1]
	blobC, err := fstree.EncodeBlob([]byte("gamma"))
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := fstree.EncodeDirLeaf([]fstree.Entry{{Name: []byte("c.txt"), Mode: 0o100644, ContentKey: blobC.Key[:]}})
	if err != nil {
		t.Fatal(err)
	}
	first = commitObj(t, "first", rootA.Key)
	left := commitObj(t, "left", rootB.Key, first.Key)
	right := commitObj(t, "right", rootA.Key, first.Key)
	tip = commitObj(t, "merge", rootB.Key, left.Key, right.Key)
	all = append(append([]fstree.Object{}, treeA...), blobC, rootB, first, left, right, tip)
	return all, first, tip
}

func TestChildKeysCommit(t *testing.T) {
	tree, err := fstree.EncodeDirLeaf(nil)
	if err != nil {
		t.Fatal(err)
	}
	p1 := commitObj(t, "p1", tree.Key)
	p2 := commitObj(t, "p2", tree.Key)
	c := commitObj(t, "merge", tree.Key, p2.Key, p1.Key)
	kids, err := fstree.ChildKeys(c.Key, c.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want := []key.Key{tree.Key, p2.Key, p1.Key} // the tree, then parents in recorded order
	if len(kids) != len(want) {
		t.Fatalf("children = %v, want %v", kids, want)
	}
	for i := range want {
		if kids[i] != want[i] {
			t.Errorf("child %d = %s, want %s", i, kids[i], want[i])
		}
	}
	if _, err := fstree.ChildKeys(c.Key, []byte("not a commit")); err == nil {
		t.Error("ChildKeys accepted garbage under a Commit key")
	}
}

func TestReachableKeysFollowsHistory(t *testing.T) {
	all, _, tip := history(t)
	keys, err := fstree.ReachableKeys(tip.Key, mapGetter(all...))
	if err != nil {
		t.Fatal(err)
	}
	if keys[0] != tip.Key {
		t.Errorf("keys[0] = %s, want the tip", keys[0])
	}
	got := map[key.Key]int{}
	for _, k := range keys {
		got[k]++
	}
	for _, o := range all {
		if got[o.Key] != 1 {
			t.Errorf("%s (%v) listed %d times, want once", o.Key, o.Key.Type(), got[o.Key])
		}
	}
	if len(keys) != len(all) {
		t.Errorf("%d keys reachable, want %d", len(keys), len(all))
	}
}

func TestCheckCompleteFollowsHistory(t *testing.T) {
	all, first, tip := history(t)
	visited, err := fstree.CheckComplete(tip.Key, mapGetter(all...), mapHas(all...), 4)
	if err != nil {
		t.Fatalf("CheckComplete: %v", err)
	}
	if len(visited) != len(all) {
		t.Errorf("visited %d keys, want %d", len(visited), len(all))
	}

	without := func(drop key.Key) []fstree.Object {
		var out []fstree.Object
		for _, o := range all {
			if o.Key != drop {
				out = append(out, o)
			}
		}
		return out
	}
	// An ancestor commit is gone: the history is incomplete.
	rest := without(first.Key)
	if _, err := fstree.CheckComplete(tip.Key, mapGetter(rest...), mapHas(rest...), 4); err == nil {
		t.Error("CheckComplete accepted a history with a missing ancestor")
	}
	// A blob only the first commit's tree holds is gone: also incomplete.
	blobB := all[1]
	rest = without(blobB.Key)
	_, err = fstree.CheckComplete(tip.Key, mapGetter(rest...), mapHas(rest...), 4)
	var missing *fstree.MissingObjectError
	if !errors.As(err, &missing) || missing.Key != blobB.Key {
		t.Errorf("err = %v, want MissingObjectError for %s", err, blobB.Key)
	}
}
