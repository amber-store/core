package chunkers

import (
	"io"

	gocdc "github.com/PlakarKorp/go-cdc-chunkers"
	_ "github.com/PlakarKorp/go-cdc-chunkers/chunkers/ultracdc" // register "ultracdc"
)

// ByteOpts configures the ultracdc byte chunker (re-exported so callers need not
// import the upstream package).
type ByteOpts = gocdc.ChunkerOpts

// The default byte-chunker sizes: min 32 KiB, normal (target) 512 KiB, max
// 1 MiB. They determine every Blob boundary and so every file key, so two
// stores dedup against each other only when they chunk with the same sizes.
const (
	DefaultMinSize    = 32 << 10
	DefaultNormalSize = 512 << 10
	DefaultMaxSize    = 1 << 20
)

// SplitBytes runs the ultracdc content-defined chunker over r and calls fn once
// per chunk, in order. Each chunk passed to fn is a fresh copy, so fn may retain
// it (the underlying chunker reuses its read buffer between calls). A nil opts
// uses the default sizes (DefaultMinSize, DefaultNormalSize, DefaultMaxSize),
// and so does every zero field of a non-nil opts. An empty reader yields zero
// chunks.
func SplitBytes(r io.Reader, opts *ByteOpts, fn func(chunk []byte) error) error {
	c, err := gocdc.NewChunker("ultracdc", r, withDefaults(opts))
	if err != nil {
		return err
	}
	for {
		chunk, err := c.Next()
		if len(chunk) > 0 {
			cp := make([]byte, len(chunk))
			copy(cp, chunk)
			if ferr := fn(cp); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// withDefaults returns a copy of opts with every zero size replaced by the
// package default; nil selects all three defaults.
func withDefaults(opts *ByteOpts) *ByteOpts {
	o := ByteOpts{MinSize: DefaultMinSize, NormalSize: DefaultNormalSize, MaxSize: DefaultMaxSize}
	if opts != nil {
		if opts.MinSize != 0 {
			o.MinSize = opts.MinSize
		}
		if opts.NormalSize != 0 {
			o.NormalSize = opts.NormalSize
		}
		if opts.MaxSize != 0 {
			o.MaxSize = opts.MaxSize
		}
		o.Key = opts.Key
	}
	return &o
}
