// Package gitobj reads git objects as the store holds them: the exact bytes git
// takes the SHA-1 of, "<kind> <size>\x00<content>", under a key.GitSHA1 key. It
// knows the framing, the hash, and the links one object holds to others, which
// is all the store needs: it does not check an object the way git fsck does.
// See architecture/git-objects.md.
package gitobj

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/amber-store/core/key"
	"github.com/pjbgf/sha1cd"
)

var (
	// ErrMalformed means a payload is not a git object the store can walk.
	ErrMalformed = errors.New("gitobj: malformed object")
	// ErrMismatch means a payload does not hash to the key it was given under.
	ErrMismatch = errors.New("gitobj: payload does not match its key")
	// ErrCollision means a payload carries the marks of a SHA-1 collision attack.
	// Its SHA-1 says nothing about it, so it must not be stored under one.
	ErrCollision = errors.New("gitobj: SHA-1 collision attack detected")
)

// Kind is what git calls an object's type.
type Kind uint8

const (
	Blob Kind = iota
	Tree
	Commit
	Tag
)

// String returns the name git frames an object with.
func (k Kind) String() string {
	switch k {
	case Blob:
		return "blob"
	case Tree:
		return "tree"
	case Commit:
		return "commit"
	case Tag:
		return "tag"
	default:
		return "kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// kindOf is the inverse of String for the four kinds there are.
func kindOf(name []byte) (Kind, bool) {
	switch string(name) {
	case "blob":
		return Blob, true
	case "tree":
		return Tree, true
	case "commit":
		return Commit, true
	case "tag":
		return Tag, true
	}
	return 0, false
}

// Encode frames content as an object of the given kind: the bytes a store
// holds, and the bytes git takes the SHA-1 of.
func Encode(kind Kind, content []byte) []byte {
	out := make([]byte, 0, len(kind.String())+22+len(content))
	out = append(out, kind.String()...)
	out = append(out, ' ')
	out = strconv.AppendUint(out, uint64(len(content)), 10)
	out = append(out, 0)
	return append(out, content...)
}

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// Parse splits a payload into its kind and content, holding the frame to what
// git reads: a known kind, one space, a decimal size without leading zeros, a
// NUL, and exactly that many bytes. The content is a part of data.
func Parse(data []byte) (Kind, []byte, error) {
	sp := bytes.IndexByte(data, ' ')
	if sp < 0 {
		return 0, nil, malformed("no space after the kind")
	}
	kind, ok := kindOf(data[:sp])
	if !ok {
		return 0, nil, malformed("unknown kind")
	}
	rest := data[sp+1:]
	nul := bytes.IndexByte(rest, 0)
	if nul < 0 {
		return 0, nil, malformed("no NUL after the size")
	}
	size, ok := parseSize(rest[:nul])
	if !ok {
		return 0, nil, malformed("bad size")
	}
	content := rest[nul+1:]
	if size != uint64(len(content)) {
		return 0, nil, malformed("size %d, content of %d bytes", size, len(content))
	}
	return kind, content, nil
}

// parseSize reads a decimal number with no leading zero and no sign.
func parseSize(b []byte) (uint64, bool) {
	if len(b) == 0 || (b[0] == '0' && len(b) > 1) {
		return 0, false
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

// KeyOf returns the key of a payload: its SHA-1, as NewGitSHA1 lays it out. It
// does not look at the frame. A payload that carries the marks of a collision
// attack has no key, and gets ErrCollision.
func KeyOf(data []byte) (key.Key, error) {
	sum, collided := sha1cd.Sum(data)
	if collided {
		return key.Key{}, ErrCollision
	}
	return key.NewGitSHA1(sum), nil
}

// Verify reports whether data is the object k names: not a collision, hashing
// to k, framed correctly, and with links that can be read. An object that
// passes can always be walked.
func Verify(k key.Key, data []byte) error {
	want, err := KeyOf(data)
	if err != nil {
		return err
	}
	if want != k {
		return fmt.Errorf("%w: payload hashes to %s, not %s", ErrMismatch, want, k)
	}
	_, err = Links(data)
	return err
}

// Link is one object's reference to another. Leaf says what the referring
// object knows of the target: it is a blob, which has no links, so a walk may
// count it without reading it. Git's own walks trust an entry's mode the same
// way.
type Link struct {
	Key  key.Key
	Leaf bool
}

// Links returns the objects a payload refers to, in the order it names them:
// a commit's tree and then its parents, a tree's entries, a tag's target. A
// blob has none. A tree entry that holds a submodule's commit is no link, since
// that commit lives in another repository; git does not need it here either.
func Links(data []byte) ([]Link, error) {
	kind, content, err := Parse(data)
	if err != nil {
		return nil, err
	}
	switch kind {
	case Tree:
		return treeLinks(content)
	case Commit:
		return commitLinks(content)
	case Tag:
		return tagLinks(content)
	}
	return nil, nil
}

// idLine reads "<prefix><40 hex digits>\n" from the start of b and returns the
// key it names and what follows. Hex digits of either case are read, as git
// does.
func idLine(b []byte, prefix string) (key.Key, []byte, error) {
	n := len(prefix)
	if !bytes.HasPrefix(b, []byte(prefix)) {
		return key.Key{}, nil, malformed("no %q line", prefix[:n-1])
	}
	hexLen := 2 * key.GitSHA1Size
	if len(b) < n+hexLen+1 || b[n+hexLen] != '\n' {
		return key.Key{}, nil, malformed("bad %q line", prefix[:n-1])
	}
	var sum [key.GitSHA1Size]byte
	if _, err := hex.Decode(sum[:], b[n:n+hexLen]); err != nil {
		return key.Key{}, nil, malformed("bad %q line", prefix[:n-1])
	}
	return key.NewGitSHA1(sum), b[n+hexLen+1:], nil
}

// commitLinks reads the lines git reads a commit's links from: a tree line
// first, the parent lines right after it. What comes later is not looked at: a
// merge tag or a message may well say "parent".
func commitLinks(b []byte) ([]Link, error) {
	tree, rest, err := idLine(b, "tree ")
	if err != nil {
		return nil, err
	}
	out := []Link{{Key: tree}}
	for bytes.HasPrefix(rest, []byte("parent ")) {
		var parent key.Key
		if parent, rest, err = idLine(rest, "parent "); err != nil {
			return nil, err
		}
		out = append(out, Link{Key: parent})
	}
	return out, nil
}

// tagLinks reads a tag's target from its first two lines.
func tagLinks(b []byte) ([]Link, error) {
	target, rest, err := idLine(b, "object ")
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(rest, []byte("type ")) {
		return nil, malformed(`no "type" line`)
	}
	rest = rest[len("type "):]
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 {
		return nil, malformed(`bad "type" line`)
	}
	kind, ok := kindOf(rest[:nl])
	if !ok {
		return nil, malformed(`bad "type" line`)
	}
	return []Link{{Key: target, Leaf: kind == Blob}}, nil
}

// The file-type bits of a tree entry's mode.
const (
	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeRegular  = 0o100000
	modeSymlink  = 0o120000
)

// treeLinks reads a tree's entries, each "<octal mode> <name>\x00<20 bytes>".
// What a mode makes an entry follows git's canon_mode: the file-type bits say
// directory, or regular file or symlink (both blobs), and whatever else is
// there, git takes for a gitlink. The rest of the mode, how the entries are
// sorted and what the names are is git fsck's business, and old repositories
// hold trees it would complain of.
func treeLinks(b []byte) ([]Link, error) {
	var out []Link
	for len(b) > 0 {
		sp := bytes.IndexByte(b, ' ')
		if sp <= 0 {
			return nil, malformed("tree entry without a mode")
		}
		var mode uint32
		for _, c := range b[:sp] {
			if c < '0' || c > '7' {
				return nil, malformed("tree entry with a mode that is not octal")
			}
			mode = mode<<3 | uint32(c-'0')
		}
		b = b[sp+1:]
		nul := bytes.IndexByte(b, 0)
		if nul < 0 {
			return nil, malformed("tree entry without an end to its name")
		}
		if nul == 0 {
			return nil, malformed("tree entry with an empty name")
		}
		b = b[nul+1:]
		if len(b) < key.GitSHA1Size {
			return nil, malformed("tree entry with a short id")
		}
		var sum [key.GitSHA1Size]byte
		copy(sum[:], b)
		b = b[key.GitSHA1Size:]
		switch mode & modeTypeMask {
		case modeDir:
			out = append(out, Link{Key: key.NewGitSHA1(sum)})
		case modeRegular, modeSymlink:
			out = append(out, Link{Key: key.NewGitSHA1(sum), Leaf: true})
		}
	}
	return out, nil
}
