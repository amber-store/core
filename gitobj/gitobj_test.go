package gitobj

import (
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/amber-store/core/key"
)

// The ids below are what git 2.50 gives these objects.
const (
	oidEmptyBlob = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	oidHello     = "ce013625030ba8dba906f756967f9e9ca394464a"
	oidEmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	oidSubTree   = "84866613e022547daff138abd5b650c9cb9a9a66"
	oidTree      = "49dff37cdebcd729bcfa301ce7b8b5b8e57da82e"
	oidCommit    = "15681690adcc0ebdfbf7a6ddc67a410536538e08"
	oidTag       = "2a9fcefefd79daa213f6c0e533b8468f56eb59ab"

	commitBody = "tree " + oidTree + "\nauthor a <a@b> 1700000000 +0000\ncommitter a <a@b> 1700000000 +0000\n\none\n"
	tagBody    = "object " + oidCommit + "\ntype commit\ntag v1\ntagger a <a@b> 1700000000 +0000\n\nx\n"
)

func raw(t *testing.T, oid string) string {
	t.Helper()
	b, err := hex.DecodeString(oid)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gitKey(t *testing.T, oid string) key.Key {
	t.Helper()
	var sum [key.GitSHA1Size]byte
	copy(sum[:], raw(t, oid))
	return key.NewGitSHA1(sum)
}

func treeBody(t *testing.T) string {
	return "40000 dir\x00" + raw(t, oidSubTree) + "100644 file\x00" + raw(t, oidHello)
}

func TestEncode(t *testing.T) {
	if got := string(Encode(Blob, []byte("hello\n"))); got != "blob 6\x00hello\n" {
		t.Errorf("Encode = %q", got)
	}
	if got := string(Encode(Tree, nil)); got != "tree 0\x00" {
		t.Errorf("Encode = %q", got)
	}
}

func TestKeyOf_MatchesGit(t *testing.T) {
	cases := []struct {
		name string
		kind Kind
		body string
		oid  string
	}{
		{"empty blob", Blob, "", oidEmptyBlob},
		{"blob", Blob, "hello\n", oidHello},
		{"empty tree", Tree, "", oidEmptyTree},
		{"tree", Tree, treeBody(t), oidTree},
		{"commit", Commit, commitBody, oidCommit},
		{"tag", Tag, tagBody, oidTag},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := KeyOf(Encode(c.kind, []byte(c.body)))
			if err != nil {
				t.Fatal(err)
			}
			if want := gitKey(t, c.oid); got != want {
				t.Errorf("KeyOf = %s, want %s", got, want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		data string
		kind Kind
		body string
	}{
		{"blob 0\x00", Blob, ""},
		{"blob 3\x00a\x00b", Blob, "a\x00b"},
		{"tree 0\x00", Tree, ""},
		{"commit 1\x00x", Commit, "x"},
		{"tag 1\x00x", Tag, "x"},
	} {
		kind, body, err := Parse([]byte(c.data))
		if err != nil {
			t.Errorf("Parse(%q): %v", c.data, err)
			continue
		}
		if kind != c.kind || string(body) != c.body {
			t.Errorf("Parse(%q) = %v, %q", c.data, kind, body)
		}
	}
}

func TestParse_Malformed(t *testing.T) {
	for name, data := range map[string]string{
		"empty":             "",
		"no space":          "blob",
		"no NUL":            "blob 3",
		"unknown kind":      "file 0\x00",
		"capital kind":      "Blob 0\x00",
		"empty kind":        " 0\x00",
		"empty size":        "blob \x00",
		"leading zero":      "blob 01\x00x",
		"two zeros":         "blob 00\x00",
		"sign":              "blob +1\x00x",
		"letter in size":    "blob 1a\x00x",
		"space in size":     "blob  1\x00x",
		"size too large":    "blob 2\x00x",
		"size too small":    "blob 1\x00xy",
		"size overflows":    "blob 18446744073709551616\x00",
		"size overflows 64": "blob 99999999999999999999999\x00",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Parse([]byte(data)); !errors.Is(err, ErrMalformed) {
				t.Errorf("Parse(%q) = %v, want ErrMalformed", data, err)
			}
		})
	}
}

func links(t *testing.T, kind Kind, body string) []Link {
	t.Helper()
	got, err := Links(Encode(kind, []byte(body)))
	if err != nil {
		t.Fatalf("Links(%v, %q): %v", kind, body, err)
	}
	return got
}

func wantLinks(t *testing.T, got []Link, want ...Link) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("Links = %v, want %v", got, want)
	}
}

func TestLinks_Blob(t *testing.T) {
	// Content that reads like a commit is still a blob's.
	wantLinks(t, links(t, Blob, commitBody))
}

func TestLinks_Commit(t *testing.T) {
	wantLinks(t, links(t, Commit, commitBody), Link{Key: gitKey(t, oidTree)})
}

func TestLinks_CommitParentsInOrder(t *testing.T) {
	body := "tree " + oidTree + "\nparent " + oidCommit + "\nparent " + oidTag + "\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n\nm\n"
	wantLinks(t, links(t, Commit, body),
		Link{Key: gitKey(t, oidTree)}, Link{Key: gitKey(t, oidCommit)}, Link{Key: gitKey(t, oidTag)})
}

// Headers after the author are not links, whatever they look like: a merge tag
// embeds "object" and "type" lines, a message may say "parent".
func TestLinks_CommitOnlyLeadingHeadersAreLinks(t *testing.T) {
	body := "tree " + oidTree + "\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n" +
		"mergetag object " + oidTag + "\n type commit\n tag v1\ngpgsig -----BEGIN-----\n xyz\n\n" +
		"parent " + oidCommit + "\n"
	wantLinks(t, links(t, Commit, body), Link{Key: gitKey(t, oidTree)})
}

func TestLinks_CommitNeedsNoAuthorAndNoLowercase(t *testing.T) {
	// git reads uppercase hex. A commit without an author it calls bogus, and the
	// walk has no reason to: all it wants is the tree.
	wantLinks(t, links(t, Commit, "tree "+oidTree+"\n"), Link{Key: gitKey(t, oidTree)})
	wantLinks(t, links(t, Commit, "tree "+strings.ToUpper(oidTree)+"\n"), Link{Key: gitKey(t, oidTree)})
}

// git takes a "parent " line for one only if it can hold an id: what is too short
// to is not a parent and not an error, and git reads the commit (git 2.50,
// parse_commit_buffer). What is long enough has to be a parent.
func TestLinks_CommitShortFinalParentLineIsNoParent(t *testing.T) {
	tree := Link{Key: gitKey(t, oidTree)}
	for name, tail := range map[string]string{
		"a few digits":     "parent abc\n",
		"no id":            "parent \n",
		"no newline":       "parent " + oidCommit,
		"no newline, cut":  "parent " + oidCommit[:20],
		"the word, no end": "parent ",
	} {
		t.Run(name, func(t *testing.T) {
			wantLinks(t, links(t, Commit, "tree "+oidTree+"\n"+tail), tree)
		})
	}
	// After a real parent as well.
	parent := Link{Key: gitKey(t, oidCommit)}
	wantLinks(t, links(t, Commit, "tree "+oidTree+"\nparent "+oidCommit+"\nparent abc\n"), tree, parent)
}

// git copies a tag's type line and compares it as a C string (parse_tag_buffer).
func TestLinks_TagTypeEndsAtANUL(t *testing.T) {
	wantLinks(t, links(t, Tag, "object "+oidCommit+"\ntype commit\x00junk\ntag v\n\nm\n"), Link{Key: gitKey(t, oidCommit)})
	wantLinks(t, links(t, Tag, "object "+oidHello+"\ntype blob\x00junk\ntag v\n\nm\n"), Link{Key: gitKey(t, oidHello), Leaf: true})
}

func TestLinks_Tree(t *testing.T) {
	wantLinks(t, links(t, Tree, treeBody(t)),
		Link{Key: gitKey(t, oidSubTree)}, Link{Key: gitKey(t, oidHello), Leaf: true})
}

func TestLinks_EmptyTree(t *testing.T) {
	wantLinks(t, links(t, Tree, ""))
}

// A mode decides what an entry is the way git's canon_mode does.
func TestLinks_TreeModes(t *testing.T) {
	blob, dir := Link{Key: gitKey(t, oidHello), Leaf: true}, Link{Key: gitKey(t, oidHello)}
	for _, c := range []struct {
		mode string
		want []Link
	}{
		{"100644", []Link{blob}},
		{"100755", []Link{blob}},
		{"100664", []Link{blob}}, // old repositories
		{"120000", []Link{blob}}, // a symlink's target is a blob
		{"40000", []Link{dir}},
		{"040000", []Link{dir}}, // old tools padded it
		{"160000", nil},         // a submodule's commit, in another repository
		{"0", nil},              // anything else git takes for a gitlink as well
		{"60000", nil},
		{"7", nil},
	} {
		t.Run(c.mode, func(t *testing.T) {
			wantLinks(t, links(t, Tree, c.mode+" x\x00"+raw(t, oidHello)), c.want...)
		})
	}
}

// A name holds any byte but NUL, spaces and newlines among them.
func TestLinks_TreeNames(t *testing.T) {
	wantLinks(t, links(t, Tree, "100644 a b\nc\x00"+raw(t, oidHello)), Link{Key: gitKey(t, oidHello), Leaf: true})
}

func TestLinks_Tag(t *testing.T) {
	for _, c := range []struct {
		target string
		leaf   bool
	}{
		{"commit", false},
		{"tree", false},
		{"tag", false},
		{"blob", true},
	} {
		t.Run(c.target, func(t *testing.T) {
			body := "object " + oidHello + "\ntype " + c.target + "\ntag v\n\nm\n"
			wantLinks(t, links(t, Tag, body), Link{Key: gitKey(t, oidHello), Leaf: c.leaf})
		})
	}
}

func TestLinks_Malformed(t *testing.T) {
	h := raw(t, oidHello)
	for _, c := range []struct {
		name string
		kind Kind
		body string
	}{
		{"commit empty", Commit, ""},
		{"commit without tree", Commit, "author a <a@b> 1 +0000\n"},
		{"commit tree short", Commit, "tree " + oidTree[:39] + "\n"},
		{"commit tree not hex", Commit, "tree " + strings.Repeat("z", 40) + "\n"},
		{"commit tree no newline", Commit, "tree " + oidTree},
		{"commit tree line long", Commit, "tree " + oidTree + "0\n"},
		{"commit parent not hex", Commit, "tree " + oidTree + "\nparent " + strings.Repeat("z", 40) + "\n"},
		{"commit parent long enough, not one", Commit, "tree " + oidTree + "\nparent abc\nauthor a <a@b> 1 +0000\ncommitter a <a@b> 1 +0000\n\nm\n"},
		{"commit parent line long", Commit, "tree " + oidTree + "\nparent " + oidCommit + "0\n"},
		{"tree no space", Tree, "100644"},
		{"tree empty mode", Tree, " x\x00" + h},
		{"tree mode not octal", Tree, "100648 x\x00" + h},
		{"tree mode not digits", Tree, "abc x\x00" + h},
		{"tree no NUL", Tree, "100644 x"},
		{"tree empty name", Tree, "100644 \x00" + h},
		{"tree id short", Tree, "100644 x\x00" + h[:19]},
		{"tree trailing", Tree, "100644 x\x00" + h + "1"},
		{"tag empty", Tag, ""},
		{"tag without object", Tag, "type commit\n"},
		{"tag object short", Tag, "object abc\ntype commit\n"},
		{"tag object not hex", Tag, "object " + strings.Repeat("z", 40) + "\ntype commit\n"},
		{"tag object no newline", Tag, "object " + oidCommit},
		{"tag without type", Tag, "object " + oidCommit + "\ntag v\n"},
		{"tag type unknown", Tag, "object " + oidCommit + "\ntype file\n"},
		{"tag type no newline", Tag, "object " + oidCommit + "\ntype commit"},
		{"tag type empty before the NUL", Tag, "object " + oidCommit + "\ntype \x00commit\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Links(Encode(c.kind, []byte(c.body))); !errors.Is(err, ErrMalformed) {
				t.Errorf("Links = %v, want ErrMalformed", err)
			}
		})
	}
	if _, err := Links([]byte("not an object")); !errors.Is(err, ErrMalformed) {
		t.Errorf("Links of an unframed payload = %v, want ErrMalformed", err)
	}
}

func TestVerify(t *testing.T) {
	data := Encode(Commit, []byte(commitBody))
	if err := Verify(gitKey(t, oidCommit), data); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerify_WrongKey(t *testing.T) {
	err := Verify(gitKey(t, oidHello), Encode(Commit, []byte(commitBody)))
	if !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify = %v, want ErrMismatch", err)
	}
}

func TestVerify_NotAGitKey(t *testing.T) {
	data := Encode(Blob, nil)
	k, err := key.New(key.Blob, uint64(len(data)), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(k, data); !errors.Is(err, ErrMismatch) {
		t.Errorf("Verify = %v, want ErrMismatch", err)
	}
}

// The hash is right, and the payload is not a git object at all: it has no frame.
func TestVerify_RefusesWhatHasNoFrame(t *testing.T) {
	for name, data := range map[string][]byte{
		"not framed":   []byte("hello"),
		"size wrong":   []byte("blob 9\x00x"),
		"unknown kind": []byte("file 1\x00x"),
	} {
		t.Run(name, func(t *testing.T) {
			k, err := KeyOf(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := Verify(k, data); !errors.Is(err, ErrMalformed) {
				t.Errorf("Verify = %v, want ErrMalformed", err)
			}
		})
	}
}

// What an object holds is for whoever reads it. git never parses an object it
// only reaches as a file, and a repository may well hold one that would not parse:
// the walk that does read it refuses it then, and nothing short of that may.
func TestVerify_LeavesTheContentToTheWalks(t *testing.T) {
	for name, data := range map[string][]byte{
		"commit is junk":  Encode(Commit, []byte("junk")),
		"tree is junk":    Encode(Tree, []byte("junk")),
		"tag is junk":     Encode(Tag, []byte("junk")),
		"tag type is odd": Encode(Tag, []byte("object "+oidCommit+"\ntype file\n")),
	} {
		t.Run(name, func(t *testing.T) {
			k, err := KeyOf(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := Verify(k, data); err != nil {
				t.Errorf("Verify = %v", err)
			}
			if _, err := Links(data); !errors.Is(err, ErrMalformed) {
				t.Errorf("Links = %v, want ErrMalformed", err)
			}
		})
	}
}

// The first 320 bytes of shattered-1.pdf: the prefix and the two blocks of the
// first public SHA-1 collision, enough for the detection to fire. Plain SHA-1
// gives this f92d74e3874587aaf443d1db961d4e26dde13e9c.
const shatteredPrefix = "" +
	"255044462d312e330a25e2e3cfd30a0a0a312030206f626a0a3c3c2f57696474" +
	"682032203020522f4865696768742033203020522f547970652034203020522f" +
	"537562747970652035203020522f46696c7465722036203020522f436f6c6f72" +
	"53706163652037203020522f4c656e6774682038203020522f42697473506572" +
	"436f6d706f6e656e7420383e3e0a73747265616d0affd8fffe00245348412d31" +
	"20697320646561642121212121852fec092339759c39b1a1c63c4c97e1fffe01" +
	"7346dc9166b67e118f029ab621b2560ff9ca67cca8c7f85ba84c79030c2b3de2" +
	"18f86db3a90901d5df45c14f26fedfb3dc38e96ac22fe7bd728f0e45bce046d2" +
	"3c570feb141398bb552ef5a0a82be331fea48037b8b5d71f0e332edf93ac3500" +
	"eb4ddc0decc1a864790c782c76215660dd309791d06bd0af3f98cda4bc4629b1"

func TestCollisionIsRefused(t *testing.T) {
	data, err := hex.DecodeString(shatteredPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := KeyOf(data); !errors.Is(err, ErrCollision) {
		t.Errorf("KeyOf = %v, want ErrCollision", err)
	}
	// Checked before anything else, whatever key is claimed.
	if err := Verify(gitKey(t, "f92d74e3874587aaf443d1db961d4e26dde13e9c"), data); !errors.Is(err, ErrCollision) {
		t.Errorf("Verify = %v, want ErrCollision", err)
	}
}

// Whatever arrives, reading it fails cleanly, and what it does read is a key that
// validates. A panic here would stop a collection.
func FuzzLinks(f *testing.F) {
	h := strings.Repeat("ab", key.GitSHA1Size)
	raw := strings.Repeat("\xab", key.GitSHA1Size)
	for _, seed := range []string{
		"", " ", "blob 0\x00", "tree 0\x00", "blob 99999999999999999999\x00",
		string(Encode(Commit, []byte("tree "+h+"\nparent "+h+"\nauthor a\n\nm"))),
		string(Encode(Tree, []byte("100644 f\x00"+raw+"40000 d\x00"+raw+"160000 m\x00"+raw))),
		string(Encode(Tag, []byte("object "+h+"\ntype blob\n"))),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		links, err := Links(data)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Links: %v", err)
			}
			return
		}
		for _, l := range links {
			if err := l.Key.Validate(); err != nil {
				t.Fatalf("link %s: %v", l.Key, err)
			}
			if l.Key.Type() != key.GitSHA1 {
				t.Fatalf("link %s is not a git key", l.Key)
			}
		}
	})
}
