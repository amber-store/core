package gc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/gitobj"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

func storeGit(t *testing.T, st *packstore.Store, kind gitobj.Kind, body string) key.Key {
	t.Helper()
	data := gitobj.Encode(kind, []byte(body))
	k, err := gitobj.KeyOf(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(k, data); err != nil {
		t.Fatal(err)
	}
	return k
}

func gitEntry(t *testing.T, mode, name string, k key.Key) string {
	t.Helper()
	sum, ok := k.GitSHA1Sum()
	if !ok {
		t.Fatalf("%s is not a git key", k)
	}
	return mode + " " + name + "\x00" + string(sum[:])
}

// gitRun runs git in dir with the machine's configuration out of the way, and
// returns what it printed.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=C", "GIT_COMMITTER_EMAIL=c@example.com", "GIT_COMMITTER_DATE=1700000000 +0000",
	)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// newRealRepo makes a repository with what a store has to walk: history with a
// merge, an annotated tag and a tag of a tag, nested directories, an executable,
// a symlink, a submodule whose commit is not in the repository, and objects no
// branch reaches. It returns the repository's directory.
func newRealRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) string { t.Helper(); return gitRun(t, dir, args...) }
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("README", "readme\n", 0o644)
	write("src/a/deep.txt", "deep\n", 0o644)
	write("run.sh", "#!/bin/sh\n", 0o755)
	if err := os.Symlink("README", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "first")
	git("tag", "-a", "-m", "annotated", "v1")

	git("checkout", "-q", "-b", "side")
	write("side.txt", "side\n", 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "side")
	git("checkout", "-q", "main")

	write("src/a/deep.txt", "deeper\n", 0o644)
	git("update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,module")
	git("add", "src")
	git("commit", "-q", "-m", "second", "-m", "parent 2222222222222222222222222222222222222222\n")
	git("merge", "-q", "--no-ff", "-m", "merge", "side")
	git("tag", "-a", "-m", "of a tag", "v2", "v1")

	// Objects no reference reaches: a blob, and a commit with a tree of its own.
	dangling := strings.TrimSpace(pipeGit(t, dir, "only here\n", "hash-object", "-w", "--stdin"))
	tree := strings.TrimSpace(pipeGit(t, dir, "100644 blob "+dangling+"\tonly-here\n", "mktree"))
	pipeGit(t, dir, "dangling\n", "commit-tree", tree)
	return dir
}

func pipeGit(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=C", "GIT_COMMITTER_EMAIL=c@example.com",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// pushAll stores every object of a repository, loose or packed, reachable or
// not, through the verifying write path, and returns their keys by git id.
func pushAll(t *testing.T, st *packstore.Store, dir string) map[string]key.Key {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "--batch-all-objects", "--batch")
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]gitobj.Kind{"blob": gitobj.Blob, "tree": gitobj.Tree, "commit": gitobj.Commit, "tag": gitobj.Tag}
	objects := map[string]key.Key{}
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("cat-file said %q", line)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			t.Fatal(err)
		}
		content := make([]byte, size+1) // and the newline after it
		if _, err := io.ReadFull(r, content); err != nil {
			t.Fatal(err)
		}
		data := gitobj.Encode(kinds[f[1]], content[:size])
		k, err := gitobj.KeyOf(data)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(k[:key.GitSHA1Size]); got != f[0] {
			t.Fatalf("%s %s: the store's SHA-1 of it is %s", f[1], f[0], got)
		}
		if err := st.PutVerified(k, data); err != nil {
			t.Fatalf("%s %s: %v", f[1], f[0], err)
		}
		objects[f[0]] = k
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return objects
}

// revList returns the ids of what git itself reaches from rev. A tag's own
// object is listed apart from what it points at.
func revList(t *testing.T, dir, rev string) []string {
	t.Helper()
	ids := map[string]bool{}
	for _, line := range strings.Split(gitRun(t, dir, "rev-list", "--objects", rev), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			ids[f[0]] = true
		}
	}
	// rev-list may start below a tag: walk down the chain by hand.
	for cur := gitRun(t, dir, "rev-parse", rev); gitRun(t, dir, "cat-file", "-t", cur) == "tag"; cur = tagTarget(t, dir, cur) {
		ids[cur] = true
	}
	return slices.Sorted(func(yield func(string) bool) {
		for id := range ids {
			if !yield(id) {
				return
			}
		}
	})
}

func tagTarget(t *testing.T, dir, id string) string {
	t.Helper()
	first, _, _ := strings.Cut(gitRun(t, dir, "cat-file", "tag", id), "\n")
	return strings.TrimPrefix(first, "object ")
}

func idsOf(t *testing.T, keys []key.Key) []string {
	t.Helper()
	out := make([]string, len(keys))
	for i, k := range keys {
		sum, ok := k.GitSHA1Sum()
		if !ok {
			t.Fatalf("%s is not a git key", k)
		}
		out[i] = fmt.Sprintf("%x", sum)
	}
	slices.Sort(out)
	return out
}

// What the store reaches from a branch or a tag is, to the object, what git
// itself reaches from it.
func TestGitWalksAgreeWithGit(t *testing.T) {
	dir := newRealRepo(t)
	ts := newTestStore(t, 1<<20)
	objects := pushAll(t, ts.objects, dir)
	for _, rev := range []string{"main", "side", "v1", "v2", "HEAD~1"} {
		t.Run(rev, func(t *testing.T) {
			id := gitRun(t, dir, "rev-parse", rev)
			root, ok := objects[id]
			if !ok {
				t.Fatalf("%s (%s) is not in the store", rev, id)
			}
			want := revList(t, dir, rev)
			got, err := fstree.ReachableKeys(root, ts.objects.Get)
			if err != nil {
				t.Fatal(err)
			}
			if g := idsOf(t, got); !slices.Equal(g, want) {
				t.Errorf("ReachableKeys: %d objects, git says %d:\n got %v\nwant %v", len(g), len(want), g, want)
			}
			visited, err := fstree.CheckComplete(root, ts.objects.Get, ts.objects.Has, 4)
			if err != nil {
				t.Fatal(err)
			}
			if g := idsOf(t, visited); !slices.Equal(g, want) {
				t.Errorf("CheckComplete: %d objects, git says %d:\n got %v\nwant %v", len(g), len(want), g, want)
			}
		})
	}
}

// Pushing a branch is a reference to its head. Everything git reaches from the
// head outlives a forced cycle, what git does not reach goes, and the objects
// that are left can be read back as git wrote them.
func TestGitBranchStaysLiveThroughACycle(t *testing.T) {
	dir := newRealRepo(t)
	ts := newTestStore(t, 2<<10)
	c := ts.openCollector(t, Options{Grace: time.Hour})
	objects := pushAll(t, ts.objects, dir)
	head := gitRun(t, dir, "rev-parse", "main")
	putTestRef(t, c, ts.refs, "refs/heads/main", objects[head])

	live := revList(t, dir, "main")
	var dead []key.Key
	for id, k := range objects {
		if !slices.Contains(live, id) {
			dead = append(dead, k)
		}
	}
	if len(dead) == 0 {
		t.Fatal("the repository has no object main does not reach")
	}

	backdatePacks(t, ts)
	stats, err := c.Run(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Reaped) == 0 {
		t.Fatalf("nothing reaped: %+v", stats)
	}
	for _, id := range live {
		data, err := ts.objects.Get(objects[id])
		if err != nil {
			t.Fatalf("object %s, which main reaches: %v", id, err)
		}
		if err := gitobj.Verify(objects[id], data); err != nil {
			t.Fatalf("object %s: %v", id, err)
		}
	}
	if countGone(t, ts.objects, dead) == 0 {
		t.Error("no object was collected that main does not reach")
	}
	if err := ts.objects.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Dropping the branch makes it all garbage. The first cycle seals what the
	// last one copied, which the grace period then has to be told is old.
	rmTestRef(t, c, ts.refs, "refs/heads/main", objects[head])
	if _, err := c.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	backdatePacks(t, ts)
	if _, err := c.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range live {
		if countGone(t, ts.objects, []key.Key{objects[id]}) != 1 {
			t.Errorf("object %s outlived the branch", id)
		}
	}
}

func TestWhyWalksGitObjects(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	blob := storeGit(t, ts.objects, gitobj.Blob, "hello\n")
	other := storeGit(t, ts.objects, gitobj.Blob, "not in the tree\n")
	tree := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "f", blob))
	tip := storeGit(t, ts.objects, gitobj.Commit, "tree "+oidOf(t, tree)+"\n")
	putTestRef(t, c, ts.refs, "main", tip)
	names, err := c.Why(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"main"}) {
		t.Errorf("Why(blob) = %v, want [main]", names)
	}
	if names, err = c.Why(other); err != nil || len(names) != 0 {
		t.Errorf("Why(other) = %v, %v", names, err)
	}
}

func oidOf(t *testing.T, k key.Key) string {
	t.Helper()
	sum, ok := k.GitSHA1Sum()
	if !ok {
		t.Fatalf("%s is not a git key", k)
	}
	return fmt.Sprintf("%x", sum)
}

// A blob's tree entry says what it is, so the mark does not read it: a cycle
// costs what the trees and commits cost, not every version of every file. The
// payload here is no git object at all, and reading it would fail the mark.
func TestMarkDoesNotReadGitBlobs(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	unreadable := []byte("not a git object")
	k, err := gitobj.KeyOf(unreadable)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.objects.Put(k, unreadable); err != nil {
		t.Fatal(err)
	}
	tree := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "f", k))
	tip := storeGit(t, ts.objects, gitobj.Commit, "tree "+oidOf(t, tree)+"\n")
	dead, err := c.UnreachableFrom(context.Background(), []key.Key{tip}, []key.Key{k})
	if err != nil {
		t.Fatalf("the mark read a blob its tree entry named: %v", err)
	}
	if len(dead) != 0 {
		t.Errorf("a blob the tree names is not marked: %v", dead)
	}
}

// One entry calls an object a file and another calls it a directory. Whichever
// the mark meets first, what lies below it stays: git reads through the
// directory entry.
func TestMarkFollowsAnObjectAlsoNamedAsAFile(t *testing.T) {
	for _, c := range []struct {
		name         string
		first, other string
	}{
		{"file sorted first", "100644", "40000"},
		{"directory sorted first", "40000", "100644"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestStore(t, 1<<20)
			coll := ts.openCollector(t, Options{})
			below := storeGit(t, ts.objects, gitobj.Blob, "below\n")
			sub := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "f", below))
			top := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, c.first, "a", sub)+gitEntry(t, c.other, "b", sub))
			dead, err := coll.UnreachableFrom(context.Background(), []key.Key{top}, []key.Key{sub, below})
			if err != nil {
				t.Fatal(err)
			}
			if len(dead) != 0 {
				t.Errorf("objects below a directory entry are not marked: %v", dead)
			}
		})
	}
	// The same across two roots: the first marks it a file, the second walks it.
	t.Run("across roots", func(t *testing.T) {
		ts := newTestStore(t, 1<<20)
		coll := ts.openCollector(t, Options{})
		below := storeGit(t, ts.objects, gitobj.Blob, "below\n")
		sub := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "f", below))
		asFile := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "a", sub))
		asDir := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "40000", "b", sub))
		dead, err := coll.UnreachableFrom(context.Background(), []key.Key{asFile, asDir}, []key.Key{below})
		if err != nil {
			t.Fatal(err)
		}
		if len(dead) != 0 {
			t.Errorf("objects below a directory entry are not marked: %v", dead)
		}
	})
}

// A shared interior object is read once however many links name it.
func TestMarkReadsAGitTreeOnce(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	blob := storeGit(t, ts.objects, gitobj.Blob, "x\n")
	sub := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "f", blob))
	top := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "40000", "a", sub)+gitEntry(t, "40000", "b", sub))
	live, err := c.markLive(context.Background(), []key.Key{top, top})
	if err != nil {
		t.Fatal(err)
	}
	if live.Marked() != 3 {
		t.Errorf("Marked = %d, want 3", live.Marked())
	}
}

func TestPrepareRefGitMissingObjectFails(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	absent := gitobj.Encode(gitobj.Blob, []byte("never stored\n"))
	k, err := gitobj.KeyOf(absent)
	if err != nil {
		t.Fatal(err)
	}
	tree := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "100644", "f", k))
	tip := storeGit(t, ts.objects, gitobj.Commit, "tree "+oidOf(t, tree)+"\n")
	if _, _, err := c.PrepareRef(tip); err == nil {
		t.Fatal("PrepareRef accepted a branch with a missing blob")
	} else if !strings.Contains(err.Error(), k.String()) {
		t.Errorf("the error does not name the object: %v", err)
	}
}

// A submodule's commit is in another repository: a branch that holds one is
// complete without it, as it is for git.
func TestPrepareRefGitSubmoduleIsNotRequired(t *testing.T) {
	ts := newTestStore(t, 1<<20)
	c := ts.openCollector(t, Options{})
	var module [key.GitSHA1Size]byte
	module[0] = 0x11
	tree := storeGit(t, ts.objects, gitobj.Tree, gitEntry(t, "160000", "mod", key.NewGitSHA1(module)))
	tip := storeGit(t, ts.objects, gitobj.Commit, "tree "+oidOf(t, tree)+"\n")
	commit, _, err := c.PrepareRef(tip)
	if err != nil {
		t.Fatalf("PrepareRef: %v", err)
	}
	commit()
}
