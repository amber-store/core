package main

import (
	"bytes"
	"testing"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/key"
)

func TestParseIdentity(t *testing.T) {
	cases := []struct {
		in, name, email string
		wantErr         bool
	}{
		{in: "Ann Example <ann@example.com>", name: "Ann Example", email: "ann@example.com"},
		{in: "  Ann  <ann@example.com>  ", name: "Ann", email: "ann@example.com"},
		{in: "Ann", name: "Ann"},
		{in: "Ann <>", name: "Ann"},
		{in: "A <b> C <c@d>", name: "A <b> C", email: "c@d"},
		{in: "<ann@example.com>", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseIdentity(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseIdentity(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got.Name != tc.name || got.Email != tc.email {
			t.Errorf("parseIdentity(%q) = %q, %q; want %q, %q", tc.in, got.Name, got.Email, tc.name, tc.email)
		}
	}
}

func TestRenderCommit(t *testing.T) {
	tree, _ := key.New(key.DirLeaf, 1, []byte{0x80})
	parent, _ := key.NewFromHash(key.Commit, 7, [32]byte{1})
	self, _ := key.NewFromHash(key.Commit, 9, [32]byte{2})
	c := commit.Commit{
		Tree:      tree,
		Parents:   []key.Key{parent},
		Author:    commit.Identity{Name: "Ann", Email: "ann@example.com", When: 1767323045_000000000, TZOffset: 60},
		Committer: commit.Identity{Name: "Bob", When: 1767323045_000000000, TZOffset: -300},
		Message:   "subject\n\nbody\n",
		Signature: []byte("sig"),
	}
	var buf bytes.Buffer
	if err := renderCommit(&buf, self, c); err != nil {
		t.Fatal(err)
	}
	want := "commit " + self.String() + "\n" +
		"tree " + tree.String() + "\n" +
		"parent " + parent.String() + "\n" +
		"author Ann <ann@example.com> 2026-01-02T04:04:05+01:00\n" +
		"committer Bob 2026-01-01T22:04:05-05:00\n" +
		"signature 3 bytes\n" +
		"\n" +
		"    subject\n" +
		"    \n" +
		"    body\n"
	if got := buf.String(); got != want {
		t.Errorf("renderCommit:\n got: %q\nwant: %q", got, want)
	}
}
