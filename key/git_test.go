package key

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// The object id git gives an empty blob, and the key that stands for it.
const (
	emptyBlobOID = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391"
	emptyBlobKey = emptyBlobOID + "00000000000000000000" + "00" + "60"
)

func emptyBlobSum(t *testing.T) (sum [GitSHA1Size]byte) {
	t.Helper()
	b, err := hex.DecodeString(emptyBlobOID)
	if err != nil {
		t.Fatal(err)
	}
	copy(sum[:], b)
	return sum
}

func TestNewGitSHA1_Layout(t *testing.T) {
	k := NewGitSHA1(emptyBlobSum(t))
	if got := k.String(); got != emptyBlobKey {
		t.Fatalf("key = %s, want %s", got, emptyBlobKey)
	}
	if err := k.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestGitKey_Accessors(t *testing.T) {
	sum := emptyBlobSum(t)
	k := NewGitSHA1(sum)
	if k.Type() != GitSHA1 {
		t.Errorf("Type = %v", k.Type())
	}
	if k.Length() != 0 || k.LengthSize() != 1 {
		t.Errorf("Length = %d (%d bytes), want 0 in 1 byte", k.Length(), k.LengthSize())
	}
	// A git key's hash is the SHA-1 alone, in digest order: not the 30 bytes
	// a BLAKE3 key's hash takes, and not reversed.
	if got := k.Hash(); !bytes.Equal(got, sum[:]) {
		t.Errorf("Hash = %x, want %x", got, sum)
	}
	got, ok := k.GitSHA1Sum()
	if !ok || got != sum {
		t.Errorf("GitSHA1Sum = %x, %v, want %x", got, ok, sum)
	}
	blob, err := New(Blob, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := blob.GitSHA1Sum(); ok {
		t.Error("GitSHA1Sum says a Blob key holds a SHA-1")
	}
}

func TestGitKey_HashIsACopy(t *testing.T) {
	k := NewGitSHA1(emptyBlobSum(t))
	want := k
	k.Hash()[0] ^= 0xff
	if k != want {
		t.Error("changing what Hash returned changed the key")
	}
}

func TestGitKey_ParseRoundTrip(t *testing.T) {
	k := NewGitSHA1(emptyBlobSum(t))
	got, err := Parse(k[:])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != k {
		t.Fatalf("Parse = %s, want %s", got, k)
	}
}

// Only one key may stand for one SHA-1: anything but the zero padding and the
// zero length would let the same git object sit under many keys.
func TestGitKey_ValidateRefusesEverythingButTheLayout(t *testing.T) {
	good := NewGitSHA1(emptyBlobSum(t))
	cases := []struct {
		name string
		edit func(k *Key)
		want error
	}{
		{"padding, first byte", func(k *Key) { k[GitSHA1Size] = 1 }, ErrNonCanonicalGitKey},
		{"padding, last byte", func(k *Key) { k[headerAt-2] = 1 }, ErrNonCanonicalGitKey},
		{"a length", func(k *Key) { k[headerAt-1] = 5 }, ErrNonCanonicalGitKey},
		{"two length bytes, a value", func(k *Key) { k[headerAt] |= 1; k[headerAt-1] = 5 }, ErrNonCanonicalGitKey},
		{"two length bytes, zero", func(k *Key) { k[headerAt] |= 1 }, ErrNonCanonicalLength},
		{"reserved bit", func(k *Key) { k[headerAt] |= 0x08 }, ErrReservedBitSet},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := good
			c.edit(&k)
			if err := k.Validate(); !errors.Is(err, c.want) {
				t.Errorf("Validate = %v, want %v", err, c.want)
			}
			if _, err := Parse(k[:]); !errors.Is(err, c.want) {
				t.Errorf("Parse = %v, want %v", err, c.want)
			}
		})
	}
}

// The padding rule is the git type's alone: a BLAKE3 key has hash bytes there.
func TestGitKey_RuleLeavesOtherTypesAlone(t *testing.T) {
	var full [Size]byte
	for i := range full {
		full[i] = 0xab
	}
	for _, ty := range []Type{Blob, FileNode, DirLeaf, DirNode, XattrSet, Commit} {
		k, err := NewFromHash(ty, 0, full)
		if err != nil {
			t.Fatal(err)
		}
		if err := k.Validate(); err != nil {
			t.Errorf("%v: Validate: %v", ty, err)
		}
	}
}

// The BLAKE3 constructors cannot make a git key: it would look canonical and
// name no git object at all.
func TestGitKey_NotFromTheBlake3Constructors(t *testing.T) {
	if _, err := New(GitSHA1, 0, []byte("blob 0\x00")); !errors.Is(err, ErrNotBlake3) {
		t.Errorf("New: %v, want ErrNotBlake3", err)
	}
	if _, err := NewFromHash(GitSHA1, 0, [Size]byte{}); !errors.Is(err, ErrNotBlake3) {
		t.Errorf("NewFromHash: %v, want ErrNotBlake3", err)
	}
}
