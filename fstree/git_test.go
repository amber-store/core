package fstree_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/gitobj"
	"github.com/amber-store/core/key"
)

func gitObject(t *testing.T, kind gitobj.Kind, body string) fstree.Object {
	t.Helper()
	data := gitobj.Encode(kind, []byte(body))
	k, err := gitobj.KeyOf(data)
	if err != nil {
		t.Fatal(err)
	}
	return fstree.Object{Key: k, Bytes: data}
}

func oid(t *testing.T, k key.Key) string {
	t.Helper()
	sum, ok := k.GitSHA1Sum()
	if !ok {
		t.Fatalf("%s is not a git key", k)
	}
	return fmt.Sprintf("%x", sum)
}

func treeEntry(t *testing.T, mode, name string, k key.Key) string {
	t.Helper()
	sum, ok := k.GitSHA1Sum()
	if !ok {
		t.Fatalf("%s is not a git key", k)
	}
	return mode + " " + name + "\x00" + string(sum[:])
}

// gitRepo is one commit on top of another, the tip's tree holding a file, a
// subdirectory with a file of its own, and a submodule whose commit is not here.
type gitRepo struct {
	blob, subBlob, sub, tree, parent, tip, tag fstree.Object
}

func (r gitRepo) all() []fstree.Object {
	return []fstree.Object{r.blob, r.subBlob, r.sub, r.tree, r.parent, r.tip, r.tag}
}

func newGitRepo(t *testing.T) gitRepo {
	t.Helper()
	var r gitRepo
	r.blob = gitObject(t, gitobj.Blob, "hello\n")
	r.subBlob = gitObject(t, gitobj.Blob, "below\n")
	r.sub = gitObject(t, gitobj.Tree, treeEntry(t, "100644", "f", r.subBlob.Key))
	submodule := gitObject(t, gitobj.Commit, "tree "+oid(t, r.sub.Key)+"\n") // never stored
	r.tree = gitObject(t, gitobj.Tree,
		treeEntry(t, "100644", "a", r.blob.Key)+treeEntry(t, "40000", "dir", r.sub.Key)+treeEntry(t, "160000", "mod", submodule.Key))
	r.parent = gitObject(t, gitobj.Commit, "tree "+oid(t, r.sub.Key)+"\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n\nfirst\n")
	r.tip = gitObject(t, gitobj.Commit, "tree "+oid(t, r.tree.Key)+"\nparent "+oid(t, r.parent.Key)+"\nauthor a <a@b> 2 +0000\ncommitter a <a@b> 2 +0000\n\nsecond\n")
	r.tag = gitObject(t, gitobj.Tag, "object "+oid(t, r.tip.Key)+"\ntype commit\ntag v1\n\nm\n")
	return r
}

func keysOf(objs ...fstree.Object) []key.Key {
	out := make([]key.Key, len(objs))
	for i, o := range objs {
		out[i] = o.Key
	}
	return out
}

func sortedKeys(ks []key.Key) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.String()
	}
	slices.Sort(out)
	return out
}

// countingGet is mapGetter that remembers what it was asked for.
type countingGet struct {
	get  func(key.Key) ([]byte, error)
	mu   sync.Mutex
	read map[key.Key]int
}

func newCountingGet(objs ...fstree.Object) *countingGet {
	return &countingGet{get: mapGetter(objs...), read: map[key.Key]int{}}
}

func (c *countingGet) Get(k key.Key) ([]byte, error) {
	c.mu.Lock()
	c.read[k]++
	c.mu.Unlock()
	return c.get(k)
}

func TestChildKeys_GitObjects(t *testing.T) {
	r := newGitRepo(t)
	for _, c := range []struct {
		name string
		obj  fstree.Object
		want []key.Key
	}{
		{"blob", r.blob, nil},
		{"tree: no submodule", r.tree, keysOf(r.blob, r.sub)},
		{"commit: tree, then parents", r.tip, keysOf(r.tree, r.parent)},
		{"tag", r.tag, keysOf(r.tip)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fstree.ChildKeys(c.obj.Key, c.obj.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("ChildKeys = %v, want %v", got, c.want)
			}
		})
	}
}

func TestChildKeys_MalformedGitObject(t *testing.T) {
	r := newGitRepo(t)
	if _, err := fstree.ChildKeys(r.tip.Key, []byte("not a git object")); !errors.Is(err, gitobj.ErrMalformed) {
		t.Errorf("ChildKeys = %v, want ErrMalformed", err)
	}
}

// Links is ChildKeys with what the walk may skip reading.
func TestLinks_SaysWhichChildrenAreLeaves(t *testing.T) {
	r := newGitRepo(t)
	links, err := fstree.Links(r.tree.Key, r.tree.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want := []fstree.Link{{Key: r.blob.Key, Leaf: true}, {Key: r.sub.Key}}
	if !slices.Equal(links, want) {
		t.Errorf("Links = %v, want %v", links, want)
	}

	mustBlob, err := fstree.EncodeBlob([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := fstree.EncodeDirLeaf(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := fstree.EncodeDirLeaf([]fstree.Entry{
		{Name: []byte("d"), Mode: 0o040755, ContentKey: dir.Key[:]},
		{Name: []byte("f"), Mode: 0o100644, ContentKey: mustBlob.Key[:]},
	})
	if err != nil {
		t.Fatal(err)
	}
	links, err = fstree.Links(leaf.Key, leaf.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	want = []fstree.Link{{Key: dir.Key}, {Key: mustBlob.Key, Leaf: true}}
	if !slices.Equal(links, want) {
		t.Errorf("Links = %v, want %v", links, want)
	}
}

func TestLinkTo_AGitRootHasToBeRead(t *testing.T) {
	r := newGitRepo(t)
	if l := fstree.LinkTo(r.blob.Key); l.Leaf {
		t.Error("a git key alone said it is a leaf: it cannot tell a blob from a commit")
	}
	blob, err := key.New(key.Blob, 1, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if l := fstree.LinkTo(blob); !l.Leaf {
		t.Error("a Blob key is a leaf")
	}
}

func TestReachableKeys_Git(t *testing.T) {
	r := newGitRepo(t)
	get := newCountingGet(r.all()...)
	got, err := fstree.ReachableKeys(r.tag.Key, get.Get)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != r.tag.Key {
		t.Errorf("root is not first: %v", got)
	}
	if w := sortedKeys(keysOf(r.all()...)); !slices.Equal(sortedKeys(got), w) {
		t.Errorf("ReachableKeys = %v, want %v", sortedKeys(got), w)
	}
	for _, leaf := range []key.Key{r.blob.Key, r.subBlob.Key} {
		if get.read[leaf] != 0 {
			t.Errorf("blob %s was read: its tree entry says what it is", leaf)
		}
	}
}

func TestReachableKeys_GitRootIsRead(t *testing.T) {
	r := newGitRepo(t)
	get := newCountingGet(r.blob)
	got, err := fstree.ReachableKeys(r.blob.Key, get.Get)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, keysOf(r.blob)) {
		t.Errorf("ReachableKeys = %v", got)
	}
	if get.read[r.blob.Key] != 1 {
		t.Errorf("a root blob was read %d times", get.read[r.blob.Key])
	}
}

func TestReachableKeys_ABlobRootIsNotRead(t *testing.T) {
	b, err := fstree.EncodeBlob([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := fstree.ReachableKeys(b.Key, func(key.Key) ([]byte, error) {
		return nil, errors.New("must not be read")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []key.Key{b.Key}) {
		t.Errorf("ReachableKeys = %v", got)
	}
}

func TestCheckComplete_Git(t *testing.T) {
	r := newGitRepo(t)
	get := newCountingGet(r.all()...)
	visited, err := fstree.CheckComplete(r.tag.Key, get.Get, mapHas(r.all()...), 4)
	if err != nil {
		t.Fatal(err)
	}
	if visited[0] != r.tag.Key {
		t.Errorf("root is not first: %v", visited)
	}
	if w := sortedKeys(keysOf(r.all()...)); !slices.Equal(sortedKeys(visited), w) {
		t.Errorf("CheckComplete = %v, want %v", sortedKeys(visited), w)
	}
	for _, leaf := range []key.Key{r.blob.Key, r.subBlob.Key} {
		if get.read[leaf] != 0 {
			t.Errorf("blob %s was read: has says all that is needed", leaf)
		}
	}
}

func TestCheckComplete_GitMissingObject(t *testing.T) {
	r := newGitRepo(t)
	without := func(gone fstree.Object) []fstree.Object {
		return slices.DeleteFunc(r.all(), func(o fstree.Object) bool { return o.Key == gone.Key })
	}
	// A blob: the walk knows it for a leaf and names it.
	rest := without(r.subBlob)
	_, err := fstree.CheckComplete(r.tag.Key, mapGetter(rest...), mapHas(rest...), 4)
	var missing *fstree.MissingObjectError
	if !errors.As(err, &missing) || missing.Key != r.subBlob.Key {
		t.Errorf("missing blob: err = %v", err)
	}
	// A parent, a subtree, the tree: anything else is a failed read.
	for _, gone := range []fstree.Object{r.parent, r.sub, r.tree, r.tip} {
		rest := without(gone)
		if _, err := fstree.CheckComplete(r.tag.Key, mapGetter(rest...), mapHas(rest...), 4); err == nil {
			t.Errorf("object %s missing: CheckComplete passed", gone.Key)
		}
	}
}

// One entry calls an object a file and another calls the same object a
// directory. A walk that took the first word for it and pruned on the second
// would pass over what lies below, and a sweep would lose it while git reads it
// through the directory entry.
func TestGitWalks_ASecondLinkCanStillMakeAnObjectInterior(t *testing.T) {
	r := newGitRepo(t)
	for _, c := range []struct {
		name string
		body string
	}{
		{"file first", treeEntry(t, "100644", "a", r.sub.Key) + treeEntry(t, "40000", "b", r.sub.Key)},
		{"directory first", treeEntry(t, "40000", "a", r.sub.Key) + treeEntry(t, "100644", "b", r.sub.Key)},
	} {
		t.Run(c.name, func(t *testing.T) {
			top := gitObject(t, gitobj.Tree, c.body)
			objs := []fstree.Object{top, r.sub, r.subBlob}
			want := sortedKeys(keysOf(objs...))

			got, err := fstree.ReachableKeys(top.Key, mapGetter(objs...))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(sortedKeys(got), want) {
				t.Errorf("ReachableKeys = %v, want %v", sortedKeys(got), want)
			}

			visited, err := fstree.CheckComplete(top.Key, mapGetter(objs...), mapHas(objs...), 4)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(sortedKeys(visited), want) {
				t.Errorf("CheckComplete = %v, want %v", sortedKeys(visited), want)
			}

			// And it notices when what lies below is gone.
			rest := []fstree.Object{top, r.sub}
			var missing *fstree.MissingObjectError
			if _, err := fstree.CheckComplete(top.Key, mapGetter(rest...), mapHas(rest...), 4); !errors.As(err, &missing) || missing.Key != r.subBlob.Key {
				t.Errorf("CheckComplete with the blob gone = %v", err)
			}
		})
	}
}

// The same interior object named at two levels is read once.
func TestGitWalks_ReadEachObjectOnce(t *testing.T) {
	r := newGitRepo(t)
	top := gitObject(t, gitobj.Tree, treeEntry(t, "40000", "a", r.sub.Key)+treeEntry(t, "40000", "b", r.sub.Key))
	objs := []fstree.Object{top, r.sub, r.subBlob}

	get := newCountingGet(objs...)
	if _, err := fstree.ReachableKeys(top.Key, get.Get); err != nil {
		t.Fatal(err)
	}
	if get.read[r.sub.Key] != 1 {
		t.Errorf("ReachableKeys read the subtree %d times", get.read[r.sub.Key])
	}

	get = newCountingGet(objs...)
	if _, err := fstree.CheckComplete(top.Key, get.Get, mapHas(objs...), 4); err != nil {
		t.Fatal(err)
	}
	if get.read[r.sub.Key] != 1 {
		t.Errorf("CheckComplete read the subtree %d times", get.read[r.sub.Key])
	}
}

// File operations read directories and file content, and a git key is neither.
func TestFileOperationsRefuseGitKeys(t *testing.T) {
	r := newGitRepo(t)
	if _, err := fstree.DirOf(r.tree.Key, mapGetter(r.all()...)); err == nil {
		t.Error("DirOf took a git tree for a directory")
	}
	if _, err := fstree.DirOf(r.tip.Key, mapGetter(r.all()...)); err == nil {
		t.Error("DirOf took a git commit for a directory")
	}
}
