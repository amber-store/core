package amberpack

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Algorithm names a compression algorithm. Its values are the codec ids a
// record carries in its flags byte, so they never change.
type Algorithm uint8

const (
	None Algorithm = 0 // the payload is stored as it is
	Zstd Algorithm = 1 // the payload is one zstd frame
	LZ4  Algorithm = 2 // the payload is one LZ4 block
)

// The highest level each algorithm takes.
const (
	maxZstdLevel = 22
	maxLZ4Level  = 12
)

func (a Algorithm) String() string {
	switch a {
	case None:
		return "none"
	case Zstd:
		return "zstd"
	case LZ4:
		return "lz4"
	}
	return "algorithm(" + strconv.Itoa(int(a)) + ")"
}

// ErrInvalidCompression wraps every rejection of a Compression value, and of
// text that does not name one.
var ErrInvalidCompression = errors.New("amberpack: invalid compression")

// Compression is an algorithm and its level. The zero value is no
// compression. Level 0 selects the algorithm's default: 3 for zstd, the fast
// compressor for lz4. zstd takes levels up to 22 and lz4 up to 12, where 1
// and above are its high-compression levels.
//
// This package's zstd encoder has four tiers, not 22 levels, and its lz4
// encoder stops at level 9; a level is mapped to the nearest one there is. The
// level changes what is written and never how it is read.
type Compression struct {
	Algorithm Algorithm
	Level     int
}

// Validate reports whether c names a known algorithm at a level it takes.
func (c Compression) Validate() error {
	var max int
	switch c.Algorithm {
	case None:
		if c.Level != 0 {
			return fmt.Errorf("%w: none takes no level, got %d", ErrInvalidCompression, c.Level)
		}
		return nil
	case Zstd:
		max = maxZstdLevel
	case LZ4:
		max = maxLZ4Level
	default:
		return fmt.Errorf("%w: unknown algorithm %d", ErrInvalidCompression, uint8(c.Algorithm))
	}
	if c.Level < 0 || c.Level > max {
		return fmt.Errorf("%w: %s level %d, want 0 to %d", ErrInvalidCompression, c.Algorithm, c.Level, max)
	}
	return nil
}

// String returns the text form ParseCompression reads: "none", "zstd",
// "zstd:19", "lz4", "lz4:9". A level of 0 is left out.
func (c Compression) String() string {
	if c.Level == 0 {
		return c.Algorithm.String()
	}
	return c.Algorithm.String() + ":" + strconv.Itoa(c.Level)
}

// ParseCompression reads the text form: none, zstd, zstd:LEVEL, lz4 or
// lz4:LEVEL. The result is valid.
func ParseCompression(s string) (Compression, error) {
	name, level, hasLevel := strings.Cut(s, ":")
	var c Compression
	switch name {
	case "none":
		c.Algorithm = None
	case "zstd":
		c.Algorithm = Zstd
	case "lz4":
		c.Algorithm = LZ4
	default:
		return Compression{}, fmt.Errorf("%w: %q: want none, zstd[:LEVEL] or lz4[:LEVEL]", ErrInvalidCompression, s)
	}
	if hasLevel {
		n, err := strconv.Atoi(level)
		// The plain decimal form only: Atoi also takes a sign and leading zeros.
		if err != nil || strconv.Itoa(n) != level {
			return Compression{}, fmt.Errorf("%w: %q: bad level %q", ErrInvalidCompression, s, level)
		}
		c.Level = n
	}
	if err := c.Validate(); err != nil {
		return Compression{}, err
	}
	return c, nil
}
