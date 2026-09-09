package chunkers

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func splitLens(t *testing.T, in []byte, opts *ByteOpts) []int {
	t.Helper()
	var lens []int
	if err := SplitBytes(bytes.NewReader(in), opts, func(c []byte) error { lens = append(lens, len(c)); return nil }); err != nil {
		t.Fatal(err)
	}
	return lens
}

func seeded(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestDefaultSizes(t *testing.T) {
	if DefaultMinSize != 32<<10 || DefaultNormalSize != 512<<10 || DefaultMaxSize != 1<<20 {
		t.Fatalf("defaults = %d/%d/%d, want 32 KiB/512 KiB/1 MiB", DefaultMinSize, DefaultNormalSize, DefaultMaxSize)
	}
}

func TestSplitBytes_NilOptsUseDefaultSizes(t *testing.T) {
	in := seeded(8 << 20)
	lens := splitLens(t, in, nil)
	if len(lens) < 4 {
		t.Fatalf("8 MiB split into only %d chunks", len(lens))
	}
	for i, n := range lens {
		if n > DefaultMaxSize || (i < len(lens)-1 && n < DefaultMinSize) {
			t.Fatalf("chunk %d has %d bytes, want %d..%d", i, n, DefaultMinSize, DefaultMaxSize)
		}
	}
	explicit := splitLens(t, in, &ByteOpts{MinSize: DefaultMinSize, NormalSize: DefaultNormalSize, MaxSize: DefaultMaxSize})
	if !equalInts(lens, explicit) {
		t.Fatalf("nil opts chunk differently from the explicit defaults:\n%v\n%v", lens, explicit)
	}
}

func TestSplitBytes_ZeroFieldsFallBackToDefaults(t *testing.T) {
	in := seeded(4 << 20)
	partial := splitLens(t, in, &ByteOpts{MinSize: 16 << 10})
	full := splitLens(t, in, &ByteOpts{MinSize: 16 << 10, NormalSize: DefaultNormalSize, MaxSize: DefaultMaxSize})
	if !equalInts(partial, full) {
		t.Fatalf("zero fields did not fall back to the defaults:\n%v\n%v", partial, full)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
