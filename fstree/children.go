package fstree

import (
	"fmt"

	"github.com/amber-store/core/gitobj"
	"github.com/amber-store/core/key"
)

// ChildKeys returns the keys directly referenced by the object with key k
// and serialized bytes data, in encounter order. Blob and XattrSet objects
// are leaves and have no children. A Commit's children are its tree, the
// further terms of a conflicted tree, then its parents, all in recorded order
// — so every walk built on ChildKeys follows history and keeps every side of
// a conflict. A DirLeaf's children are its entries' content keys whatever
// their type, so a directory entry that holds a Commit is followed too. A git
// object's children are what git follows from it (gitobj.Links).
func ChildKeys(k key.Key, data []byte) ([]key.Key, error) {
	switch k.Type() {
	case key.Blob, key.XattrSet:
		return nil, nil
	case key.FileNode:
		children, err := DecodeFileNode(data)
		if err != nil {
			return nil, fmt.Errorf("fstree: decoding FileNode %s: %w", k, err)
		}
		return children, nil
	case key.DirNode:
		pairs, err := DecodeDirNode(data)
		if err != nil {
			return nil, fmt.Errorf("fstree: decoding DirNode %s: %w", k, err)
		}
		out := make([]key.Key, 0, len(pairs))
		for _, p := range pairs {
			ck, err := key.Parse(p.ChildKey)
			if err != nil {
				return nil, fmt.Errorf("fstree: child key in DirNode %s: %w", k, err)
			}
			out = append(out, ck)
		}
		return out, nil
	case key.DirLeaf:
		entries, err := DecodeDirLeaf(data)
		if err != nil {
			return nil, fmt.Errorf("fstree: decoding DirLeaf %s: %w", k, err)
		}
		var out []key.Key
		for _, ent := range entries {
			if len(ent.ContentKey) > 0 {
				ck, err := key.Parse(ent.ContentKey)
				if err != nil {
					return nil, fmt.Errorf("fstree: %q: content key: %w", ent.Name, err)
				}
				out = append(out, ck)
			}
			if len(ent.XattrsKey) > 0 {
				xk, err := key.Parse(ent.XattrsKey)
				if err != nil {
					return nil, fmt.Errorf("fstree: %q: xattrs key: %w", ent.Name, err)
				}
				out = append(out, xk)
			}
		}
		return out, nil
	case key.Commit:
		c, err := decodeCommit(k, data) // and hold k to the key rule: see there
		if err != nil {
			return nil, err
		}
		trees := c.Trees()
		out := make([]key.Key, 0, len(trees)+len(c.Parents))
		out = append(out, trees...)
		return append(out, c.Parents...), nil
	case key.GitSHA1:
		links, err := gitLinks(k, data)
		if err != nil {
			return nil, err
		}
		out := make([]key.Key, len(links))
		for i, l := range links {
			out[i] = l.Key
		}
		return out, nil
	default:
		return nil, fmt.Errorf("fstree: unknown object type %s", k.Type())
	}
}

// Link is one object's reference to another, with what a walk may skip: a Leaf
// has no children, so it is counted without being read.
type Link = gitobj.Link

// LinkTo is the link to a root, which no object refers to: all that is known
// of it is what its key says. A Blob or XattrSet key says it is a leaf. A git
// key says nothing of the kind: it holds a SHA-1 alone, and a blob and a commit
// look the same.
func LinkTo(k key.Key) Link {
	return Link{Key: k, Leaf: k.Type() == key.Blob || k.Type() == key.XattrSet}
}

// Links returns what ChildKeys returns, with each child's Leaf set. A child's
// own key tells for the store's own types. For a git object the referring object
// tells: a tree entry's mode or a tag's type line says which targets are blobs.
//
// That is a claim, and two links can disagree: one tree entry may call an object
// a file and another call it a directory. A walk must therefore not prune on a
// key it has seen only through a Leaf link, or it passes over what the other
// link reaches. See visits.
func Links(k key.Key, data []byte) ([]Link, error) {
	if k.Type() == key.GitSHA1 {
		return gitLinks(k, data)
	}
	children, err := ChildKeys(k, data)
	if err != nil {
		return nil, err
	}
	out := make([]Link, len(children))
	for i, c := range children {
		out[i] = LinkTo(c)
	}
	return out, nil
}

func gitLinks(k key.Key, data []byte) ([]Link, error) {
	links, err := gitobj.Links(data)
	if err != nil {
		return nil, fmt.Errorf("fstree: decoding git object %s: %w", k, err)
	}
	return links, nil
}

// visits is what a walk has seen: every key it has met, and for each whether it
// has been sent to be read. A key met through a Leaf link alone is present and
// false, so that a later link that does not say Leaf still gets it read.
type visits map[key.Key]bool

// visit records a link. isNew says the key had not been met before; do says
// there is something to do for this link: a test for presence if it is a leaf, a
// read if not.
func (v visits) visit(l Link) (isNew, do bool) {
	read, met := v[l.Key]
	if met && (l.Leaf || read) {
		return false, false
	}
	v[l.Key] = !l.Leaf
	return !met, true
}
