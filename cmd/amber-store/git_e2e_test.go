package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/amber-store/core/gitobj"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

// storeGitBranch writes a small history straight into the store, as whatever
// pushes a repository would, and returns the head and the blob it holds.
func storeGitBranch(t *testing.T, store string) (head, blob key.Key) {
	t.Helper()
	objects, err := packstore.Open(filepath.Join(store, "packstore"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(kind gitobj.Kind, body string) key.Key {
		t.Helper()
		data := gitobj.Encode(kind, []byte(body))
		k, err := gitobj.KeyOf(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := objects.PutVerified(k, data); err != nil {
			t.Fatal(err)
		}
		return k
	}
	blob = put(gitobj.Blob, "hello\n")
	sum, _ := blob.GitSHA1Sum()
	tree := put(gitobj.Tree, "100644 f\x00"+string(sum[:]))
	treeID := tree.String()[:2*key.GitSHA1Size]
	head = put(gitobj.Commit, "tree "+treeID+"\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n\nm\n")
	if err := objects.Close(); err != nil {
		t.Fatal(err)
	}
	return head, blob
}

func TestE2E_GitBranch(t *testing.T) {
	store := t.TempDir()
	seg := []string{"--store", store, "--segment-size", "4096"}
	run := func(args ...string) (string, error) {
		t.Helper()
		return runApp(t, append(seg, args...)...)
	}
	head, blob := storeGitBranch(t, store)

	// A key reads as the git id, and a reference takes it like any other.
	if _, err := run("ref", "set", "refs/heads/main", head.String()); err != nil {
		t.Fatalf("ref set: %v", err)
	}
	out, err := run("gc", "why", blob.String())
	if err != nil {
		t.Fatalf("gc why: %v", err)
	}
	if !strings.Contains(out, "refs/heads/main") {
		t.Errorf("gc why = %q, want the branch", out)
	}
	if out, err := run("gc", "status"); err != nil || !strings.Contains(out, "live") {
		t.Errorf("gc status = %q, %v", out, err)
	}

	// The store's own trees live in the same store and are not disturbed.
	src := t.TempDir()
	writeFixture(t, src)
	if _, err := run("ingest", "--no-progress", "--ref", "files", src); err != nil {
		t.Fatalf("ingest next to git objects: %v", err)
	}
	if _, err := run("gc", "run", "--grace", "0s", "--garbage", "0"); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if _, err := run("restore", "ref:files", t.TempDir()); err != nil {
		t.Fatalf("restore after a cycle: %v", err)
	}
	if out, err := run("gc", "why", blob.String()); err != nil || !strings.Contains(out, "refs/heads/main") {
		t.Errorf("gc why after a cycle = %q, %v", out, err)
	}

	// File operations read the store's directories, which a git key is not. They
	// say so.
	for name, args := range map[string][]string{
		"ls":          {"ls", head.String()},
		"restore":     {"restore", head.String(), t.TempDir()},
		"export":      {"export", "-o", filepath.Join(t.TempDir(), "out.tar"), head.String()},
		"commit show": {"commit", "show", head.String()},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := run(args...)
			if err == nil {
				t.Fatalf("%v took a git commit: %q", args, out)
			}
			if !strings.Contains(err.Error(), "GitSHA1") {
				t.Errorf("%v: the error does not say what the key is: %v", args, err)
			}
		})
	}

	// Without the branch it is garbage.
	if _, err := run("ref", "rm", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if out, err := run("gc", "why", blob.String()); err != nil || !strings.Contains(out, "unreferenced") {
		t.Errorf("gc why without the branch = %q, %v", out, err)
	}
}

func TestE2E_RefSetChecksGitCompleteness(t *testing.T) {
	store := t.TempDir()
	head, _ := storeGitBranch(t, store)
	objects, err := packstore.Open(filepath.Join(store, "packstore"))
	if err != nil {
		t.Fatal(err)
	}
	// A commit whose parent was never pushed, as from a shallow clone.
	body := "tree " + strings.Repeat("0", 2*key.GitSHA1Size) + "\nparent " + head.String()[:2*key.GitSHA1Size] + "\n"
	data := gitobj.Encode(gitobj.Commit, []byte(body))
	shallow, err := gitobj.KeyOf(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.PutVerified(shallow, data); err != nil {
		t.Fatal(err)
	}
	if err := objects.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runApp(t, "--store", store, "ref", "set", "partial", shallow.String()); err == nil {
		t.Error("ref set accepted a commit whose tree is not in the store")
	}
}
