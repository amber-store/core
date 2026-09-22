package key

import "testing"

func TestType_IsValid(t *testing.T) {
	for ty := Type(0); ty <= 5; ty++ {
		if !ty.IsValid() {
			t.Errorf("Type(%d) should be valid", uint8(ty))
		}
	}
	for _, ty := range []Type{6, 7, 15, 16, 255} {
		if ty.IsValid() {
			t.Errorf("Type(%d) should be invalid", uint8(ty))
		}
	}
}

func TestType_String(t *testing.T) {
	cases := map[Type]string{
		Blob:     "Blob",
		FileNode: "FileNode",
		DirLeaf:  "DirLeaf",
		DirNode:  "DirNode",
		XattrSet: "XattrSet",
		Commit:   "Commit",
		Type(7):  "Type(7)",
	}
	for ty, want := range cases {
		if got := ty.String(); got != want {
			t.Errorf("Type(%d).String() = %q, want %q", uint8(ty), got, want)
		}
	}
}
