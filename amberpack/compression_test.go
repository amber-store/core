package amberpack

import (
	"errors"
	"testing"
)

func TestCompressionValidate(t *testing.T) {
	valid := []Compression{
		{},
		{Algorithm: None},
		{Algorithm: Zstd},
		{Algorithm: Zstd, Level: 1},
		{Algorithm: Zstd, Level: 22},
		{Algorithm: LZ4},
		{Algorithm: LZ4, Level: 1},
		{Algorithm: LZ4, Level: 12},
	}
	for _, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: unexpected error %v", c, err)
		}
	}
	invalid := []Compression{
		{Algorithm: None, Level: 1},
		{Algorithm: Zstd, Level: -1},
		{Algorithm: Zstd, Level: 23},
		{Algorithm: LZ4, Level: -1},
		{Algorithm: LZ4, Level: 13},
		{Algorithm: Algorithm(3)},
		{Algorithm: Algorithm(255), Level: 1},
	}
	for _, c := range invalid {
		if err := c.Validate(); !errors.Is(err, ErrInvalidCompression) {
			t.Errorf("%+v: err = %v, want ErrInvalidCompression", c, err)
		}
	}
}

func TestCompressionString(t *testing.T) {
	cases := map[string]Compression{
		"none":    {},
		"zstd":    {Algorithm: Zstd},
		"zstd:19": {Algorithm: Zstd, Level: 19},
		"lz4":     {Algorithm: LZ4},
		"lz4:9":   {Algorithm: LZ4, Level: 9},
	}
	for want, c := range cases {
		if got := c.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", c, got, want)
		}
		back, err := ParseCompression(want)
		if err != nil || back != c {
			t.Errorf("ParseCompression(%q) = %+v, %v; want %+v", want, back, err, c)
		}
	}
	if got := Algorithm(7).String(); got != "algorithm(7)" {
		t.Errorf("unknown algorithm prints %q", got)
	}
}

func TestParseCompressionAcceptsAnExplicitZeroLevel(t *testing.T) {
	for in, want := range map[string]Compression{
		"zstd:0": {Algorithm: Zstd},
		"lz4:0":  {Algorithm: LZ4},
		"none:0": {},
		"zstd:1": {Algorithm: Zstd, Level: 1},
		"lz4:12": {Algorithm: LZ4, Level: 12},
	} {
		got, err := ParseCompression(in)
		if err != nil || got != want {
			t.Errorf("ParseCompression(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
}

func TestParseCompressionRejectsSloppyText(t *testing.T) {
	for _, in := range []string{
		"", " ", "gzip", "ZSTD", "Zstd", " zstd", "zstd ", "zstd:", "zstd:x",
		"zstd:+3", "zstd:03", "zstd:-1", "zstd:3 ", "zstd:23", "zstd:3:4",
		"lz4:13", "lz4:-0", "none:1", ":3", "lz4hc",
	} {
		if c, err := ParseCompression(in); !errors.Is(err, ErrInvalidCompression) {
			t.Errorf("ParseCompression(%q) = %+v, %v; want ErrInvalidCompression", in, c, err)
		}
	}
}
