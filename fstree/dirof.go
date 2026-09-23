package fstree

import (
	"fmt"

	"github.com/amber-store/core/commit"
	"github.com/amber-store/core/key"
)

// A Commit key may stand wherever a directory key can: as the root handed to
// a reader, and as the content key of an S_IFDIR directory entry. It reads as
// the directory the commit records — its tree; for a conflicted commit, the
// first side. LookupEntry, ListEntries and CollectEntries pass through it,
// and so does everything built on them. A commit's tree is never a commit, so
// one step always suffices.

// DirOf returns the directory object k stands for: k itself when it is a
// DirLeaf or DirNode, the tree of the commit when it is a Commit. get fetches
// the bytes stored under a key.
func DirOf(k key.Key, get func(key.Key) ([]byte, error)) (key.Key, error) {
	switch k.Type() {
	case key.DirLeaf, key.DirNode:
		return k, nil
	case key.Commit:
		data, err := get(k)
		if err != nil {
			return key.Key{}, fmt.Errorf("fstree: reading %s: %w", k, err)
		}
		return commitTree(k, data)
	default:
		return key.Key{}, fmt.Errorf("fstree: %s is not a directory object (type %v)", k, k.Type())
	}
}

// commitTree is the tree of the commit with key k and bytes data.
func commitTree(k key.Key, data []byte) (key.Key, error) {
	c, err := commit.Decode(data)
	if err != nil {
		return key.Key{}, fmt.Errorf("fstree: decoding Commit %s: %w", k, err)
	}
	return c.Tree, nil
}
