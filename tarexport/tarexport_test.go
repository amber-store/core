package tarexport_test

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"

	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
	"github.com/amber-store/core/tarexport"
)

// buildStore ingests three blobs + a single-leaf directory referencing two
// regular files, returning the store and the directory root key. It encodes the
// objects directly with fstree to avoid depending on the CLI.
func TestWrite_RegularFilesAndDir(t *testing.T) {
	store, err := packstore.Open(t.TempDir(), packstore.WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	put := func(o fstree.Object) {
		t.Helper()
		if err := store.Put(o.Key, o.Bytes); err != nil {
			t.Fatal(err)
		}
	}

	// Two files: "a" -> "alpha", "b" -> "beta" (each a single Blob).
	ablob, _ := fstree.EncodeBlob([]byte("alpha"))
	bblob, _ := fstree.EncodeBlob([]byte("beta"))
	put(ablob)
	put(bblob)

	// A directory leaf with two regular-file entries (mode 0o100644).
	entries := []fstree.Entry{
		{Name: []byte("a"), Mode: 0o100644, Mtime: 1, ContentKey: ablob.Key[:]},
		{Name: []byte("b"), Mode: 0o100644, Mtime: 2, ContentKey: bblob.Key[:]},
	}
	leaf, err := fstree.EncodeDirLeaf(entries)
	if err != nil {
		t.Fatal(err)
	}
	put(leaf)

	var buf bytes.Buffer
	if err := tarexport.Write(&buf, leaf.Key, store.Get); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := map[string]string{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[h.Name] = string(data)
	}
	if got["a"] != "alpha" || got["b"] != "beta" {
		t.Fatalf("tar contents = %v, want a=alpha b=beta", got)
	}
}

func TestWrite_RejectsNonDirectoryRoot(t *testing.T) {
	store, err := packstore.Open(t.TempDir(), packstore.WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blob, _ := fstree.EncodeBlob([]byte("x"))
	if err := store.Put(blob.Key, blob.Bytes); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tarexport.Write(&buf, blob.Key, store.Get); err == nil {
		t.Fatalf("expected error exporting a non-directory root (type %v)", key.Blob)
	}
}

func TestWrite_NestedDir(t *testing.T) {
	store, err := packstore.Open(t.TempDir(), packstore.WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	put := func(o fstree.Object) {
		t.Helper()
		if err := store.Put(o.Key, o.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	fblob, _ := fstree.EncodeBlob([]byte("deep"))
	put(fblob)
	child, err := fstree.EncodeDirLeaf([]fstree.Entry{
		{Name: []byte("f.txt"), Mode: 0o100644, Mtime: 1, ContentKey: fblob.Key[:]},
	})
	if err != nil {
		t.Fatal(err)
	}
	put(child)
	root, err := fstree.EncodeDirLeaf([]fstree.Entry{
		{Name: []byte("d"), Mode: 0o040755, Mtime: 2, ContentKey: child.Key[:]}, // S_IFDIR
	})
	if err != nil {
		t.Fatal(err)
	}
	put(root)

	var buf bytes.Buffer
	if err := tarexport.Write(&buf, root.Key, store.Get); err != nil {
		t.Fatalf("Write: %v", err)
	}

	names := map[string]string{}
	types := map[string]byte{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		names[h.Name] = string(data)
		types[h.Name] = h.Typeflag
	}
	if types["d/"] != tar.TypeDir {
		t.Errorf("expected a TypeDir header for d/, got types=%v", types)
	}
	if names["d/f.txt"] != "deep" {
		t.Errorf("nested file content = %q, want deep (names=%v)", names["d/f.txt"], names)
	}
}

func TestWrite_RejectsUnsafeEntryName(t *testing.T) {
	store, err := packstore.Open(t.TempDir(), packstore.WithSync(false))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blob, _ := fstree.EncodeBlob([]byte("x"))
	if err := store.Put(blob.Key, blob.Bytes); err != nil {
		t.Fatal(err)
	}
	leaf, err := fstree.EncodeDirLeaf([]fstree.Entry{
		{Name: []byte(".."), Mode: 0o100644, Mtime: 1, ContentKey: blob.Key[:]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(leaf.Key, leaf.Bytes); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tarexport.Write(&buf, leaf.Key, store.Get); err == nil {
		t.Fatalf("expected error for entry named %q", "..")
	}
}
