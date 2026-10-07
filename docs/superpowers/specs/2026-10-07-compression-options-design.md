# Compression options: algorithm and level per store, a choice per object, lz4

**Date:** 2026-10-07
**Status:** design agreed with the user on 2026-10-07. The user's decisions:
an additive codec id in the record (no version bump), native integer levels, a
per-object callback that returns the compression to use, and the default
changes to no compression. Branch `compression-options`.
**Repos:** `amber-store/core` and `amber-store/core-rs`, as a paired change.

## Background

There is one encode path, `amberpack.EncodeRecord` (Rust: `encode_record`). It
always tries zstd at the default level and keeps the result only when it is
strictly smaller than the payload; otherwise it stores the payload raw. Nothing
about this is configurable.

The record's flags byte has one defined bit, `0x01` for zstd. `ParseRecord`
rejects every other bit as corrupt.

The record is shared by packstore segments and wire packs, and most paths copy
records verbatim: compaction, the GC copy, `GetRecord` into
`Writer.AddRecord`, and a write of a pre-encoded `Object.Record`. Only four
places encode: `Store.Put`, `prepare` (behind `WriteBatch` and
`WriteParallel`), the repair path behind `PutVerified` and
`PutVerifiedDeferred`, and the wire `Writer.Add`.

## Goal

A caller that opens a packstore chooses the compression its writes use: none,
zstd at a level, or lz4 at a level. It may also choose per object, from the
object's key and bytes. Every store handle reads records of every codec,
whatever it was opened with. A further algorithm can be added later without
changing the shape of the API. Without any option, nothing is compressed.

## Non-goals

- Recompressing existing records. Compaction and GC keep copying verbatim.
- Storing the setting in the store. It belongs to the open handle.
- A format version bump for segments or wire packs.
- Codecs supplied by the caller. A codec id is part of the format, and a
  private one would write records no other implementation reads.
- Exact zstd levels in Go, which would need cgo.
- Updating the consumers: dstore, transport-iroh, dstore-client-rs, dstore-web,
  ajj. "Behaviour change" below says what each will see.
- A compression flag on `amber-bench`. Its dataset is incompressible.

## Record format

The byte at offset 33 keeps its name, flags, and now holds a codec id:

```
33      1     flags    codec id: 0 = raw, 1 = zstd frame, 2 = lz4 block;
                       3–255 reserved and rejected
```

Values 0 and 1 mean what the zstd bit meant, so every existing record stays
valid as it is.

An lz4 payload is one LZ4 block: the block format, with no frame header, no
size prefix and no checksum. The decoder sizes its output from `ulen`, as the
zstd decoder does.

The length invariants generalize:

- codec 0 ⟹ `ulen == slen`
- any other codec ⟹ `slen < ulen`

`ParseRecord` rejects a flags value above 2 with the error it gives today
(`unknown record flags`), wrapping `ErrCorrupt`. `DecodePayload` decodes by
codec id and rejects an unknown one the same way; today it treats every value
without the zstd bit as raw. An lz4 block that does not decode to exactly
`ulen` bytes is `ErrCorrupt`. Decoding allocates `ulen` bytes and no more, and
`ParseRecord` has already bounded `ulen` by `MaxPayload`.

The segment magic, the wire pack magic, the footer and the sidecar do not
change. The sidecar entry carries the flags byte without interpreting it.

**Compatibility.** Releases up to 0.9.0 read raw and zstd records. A store or
a pack holding only codecs 0 and 1 stays readable by them.

*Corrected on 2026-10-07, after the branch review ran v0.9.0 against an lz4
store.* This section first said that those releases reject an lz4 record as
corrupt. Their scrub does, and their wire-pack reader refuses a pack that
holds one. Their read path does not: it tests only the zstd bit, so a `Get`
returns the lz4 block itself as the object's bytes, without an error. And
when such a release indexes an active segment by scanning it, it takes the
first lz4 record for a torn tail and truncates there at its next write. The
decision for an additive codec id rested on the wrong statement. The user's
ruling on the correction: keep the codec id, and add a gate — a segment
format version of its own for segments that hold lz4 records, so that those
releases refuse the store — as a further change on the same branches, with
its own design, before they merge.

## The compression value

Go, in `amberpack`:

```go
type Algorithm uint8 // the values are the record codec ids

const (
	None Algorithm = 0
	Zstd Algorithm = 1
	LZ4  Algorithm = 2
)

// Compression is an algorithm and its level. The zero value is no
// compression. Level 0 selects the algorithm's default level.
type Compression struct {
	Algorithm Algorithm
	Level     int
}

func (c Compression) Validate() error // wraps ErrInvalidCompression
func (c Compression) String() string  // "none", "zstd", "zstd:19", "lz4", "lz4:9"
func ParseCompression(s string) (Compression, error)

var ErrInvalidCompression = errors.New("amberpack: invalid compression")
```

Rust, in `amberpack`:

```rust
#[non_exhaustive]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Compression {
    #[default]
    None,
    Zstd { level: i32 },
    Lz4 { level: i32 },
}

impl Compression {
    pub fn validate(&self) -> Result<(), Error>; // Error::InvalidCompression
}
// Display and FromStr use the same text form as Go.
```

`#[non_exhaustive]` lets a later release add an algorithm without breaking
callers that match on the enum.

Levels are the algorithm's own numbers. `Validate` accepts the same range in
both languages, so one setting is valid on both sides:

| algorithm | valid levels | level 0 means | Go | Rust |
| --- | --- | --- | --- | --- |
| none | 0 | — | — | — |
| zstd | 0–22 | 3 | the klauspost tier nearest the level: 1–2 fastest, 3–5 default, 6–9 better, 10–22 best | libzstd at exactly that level |
| lz4 | 0–12 | the fast compressor | 0: `CompressBlock`; 1–9: `CompressBlockHC` at that level; 10–12: level 9 | 0: the fast compressor; 1–12: the HC compressor at exactly that level |

The same level can therefore produce different bytes, and different sizes, in
Go and in Rust. Each side decodes what the other wrote. A level outside the
range, a level other than 0 with `None`, and an unknown algorithm are all
invalid.

The text form is `none`, `zstd`, `zstd:LEVEL`, `lz4` or `lz4:LEVEL`. `String`
omits a level of 0, and `ParseCompression(c.String())` returns `c`.

## Encoding

```go
func EncodeRecordWith(k key.Key, data []byte, c Compression) ([]byte, error)
func EncodeRecord(k key.Key, data []byte) ([]byte, error) // EncodeRecordWith(k, data, Compression{})
```

Rust: `encode_record_with(k, data, c)` and `encode_record(k, data)`.

`EncodeRecordWith` validates `c` and returns its error for an invalid value.
With `None` it stores the payload raw. Otherwise it compresses and keeps the
result only when it is strictly smaller than the payload; when it is not, or
when the compressor reports the input incompressible, it stores the payload
raw with codec 0. An empty payload is always raw.

The wire writer takes the same value:

```go
func NewWriter(w io.Writer, opts ...WriterOption) *Writer
func WithCompression(c Compression) WriterOption
```

`Writer.Add` encodes with the writer's compression, which is `None` unless the
option is given. `NewWriter` returns no error, so an invalid value fails each
`Add`. In Rust this is a builder method: `Writer::new(w).compression(c)`. The
writer takes no per-object callback; a caller that wants one encodes with
`EncodeRecordWith` and calls `AddRecord`.

## Packstore options

Go:

```go
// WithCompression sets the compression for the objects this store encodes.
// The default is no compression.
func WithCompression(c amberpack.Compression) Option

// CompressionFunc returns the compression to use for one object. def is the
// store's WithCompression value.
type CompressionFunc func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression

// WithCompressionFor makes the store ask f for every object it encodes.
func WithCompressionFor(f CompressionFunc) Option
```

Rust, on `packstore::Options`:

```rust
pub fn compression(self, c: Compression) -> Options
pub fn compression_for(
    self,
    f: impl Fn(&Key, &[u8], Compression) -> Compression + Send + Sync + 'static,
) -> Options
```

`Options` holds the closure behind an `Arc`, so it keeps `Clone` and loses
`Copy`, and its `Debug` is written by hand.

For each object the store encodes, it takes the `WithCompression` value, passes
it to the callback if there is one, and encodes with what comes back:

```go
c := s.cfg.compression
if s.cfg.compressionFor != nil {
	c = s.cfg.compressionFor(k, data, c)
}
rec, err := amberpack.EncodeRecordWith(k, data, c)
```

A callback that returns `def` accepts the store's setting. One that returns
`Compression{}` stores the object raw. Anything else overrides the setting for
that object:

```go
packstore.WithCompression(amberpack.Compression{Algorithm: amberpack.LZ4}),
packstore.WithCompressionFor(func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
	if k.Type() != key.Blob {
		// Index nodes, directories, commits: small and read often.
		return amberpack.Compression{Algorithm: amberpack.Zstd, Level: 19}
	}
	return def // file content: lz4
}),
```

The rules:

- **When it is called.** Once for each object the store encodes, after the
  dedup check has missed: in `Put`, in `WriteBatch` and `WriteParallel` for an
  object that carries `Data`, and in `PutVerified` and `PutVerifiedDeferred`
  when they write a new record or a replacement. It is called whatever the
  `WithCompression` value is, `None` included.
- **When it is not.** For a dedup hit, for an object that carries a
  pre-encoded `Record`, and for the copies made by compaction, GC and repair.
  Those records keep the codec they have.
- **What it may do.** It runs on whichever goroutine or thread is writing,
  several at once under `WriteParallel`, so it must be safe to call
  concurrently. It must not modify or keep `data`. It must not call the store:
  the repair path calls it with the append lock held.
- **Validation.** `Open` validates the `WithCompression` value and fails for an
  invalid one with an error wrapping `ErrInvalidCompression`. A value returned
  by the callback is validated at that write. An
  invalid one fails the write with an error wrapping `ErrInvalidCompression`
  that names the key, and the object is not stored. In `WriteBatch` and
  `WriteParallel` it ends the run as any other encode error does, leaving the
  valid prefix.
- **Scope.** The setting belongs to the handle and is stored nowhere. Processes
  that share a store directory may each use their own. Reading never consults
  it.

`WriteStats.BytesStored` keeps counting uncompressed payload bytes.

## The CLI

`amber-store` gains a global flag in both implementations (Go
`cmd/amber-store`, Rust `examples/amber-store`):

```
--compression none|zstd[:LEVEL]|lz4[:LEVEL]     (default: none)
```

It is parsed with `ParseCompression` and passed as `WithCompression` to the
store the command opens. An invalid value is a usage error. The CLI offers no
per-object choice.

The flag is here for two reasons. With the default now `none`, the CLI would
otherwise have no way to compress at all. And `interop/check.sh` drives the two
CLIs, so it needs the flag to exercise the codecs across languages.

## Behaviour change

- A store opened without options writes raw records. 0.9.0 wrote zstd when
  that was smaller. `WithCompression(amberpack.Compression{Algorithm:
  amberpack.Zstd})` restores what 0.9.0 did, and in Go it produces the same
  bytes.
- `EncodeRecord` and `Writer.Add` write raw records by default, for the same
  reason: there is one default.
- Reading is unaffected. Existing stores open and read as before.

What the consumers will see when they move to this release, none of it changed
here:

- **dstore, ajj, dstore-client-rs** open packstores without a compression
  option, so their new writes become raw until they pass one.
- **transport-iroh** sends packs through `Writer.Add` (`protocol.SendPack`), so
  those packs become raw until it passes `amberpack.WithCompression`. Its
  record path (`AddRecord`) is verbatim and unaffected.
- **dstore-web** has its own port of the read path. It reads raw and zstd
  records and cannot read lz4 records until it learns codec 2.

## Libraries

- **Go.** `github.com/pierrec/lz4/v4`, pure Go: `CompressBlock`,
  `CompressBlockHC`, `UncompressBlock`. zstd stays on klauspost, with one
  encoder per tier, each created on first use. The decoder does not change.
- **Rust.** The `lz4` crate, which builds the C library, for its HC levels;
  the pure-Rust `lz4_flex` has none. core-rs already builds C for libzstd and
  SQLite. zstd stays on the `zstd` crate, now called with the chosen level.

## Testing

Go, `amberpack`:

- A round trip for each codec at levels 0, 1, a middle level and the maximum,
  with the flags byte checked.
- Incompressible input falls back to codec 0 under every algorithm. So does an
  empty payload.
- `ParseRecord` rejects flags 3, 0x80 and 0xff, and an lz4 record with
  `slen >= ulen`. `DecodePayload` rejects an unknown codec.
- An lz4 block that decodes shorter or longer than `ulen`, and a truncated one,
  are `ErrCorrupt`.
- `Validate` over a table of valid and invalid values. `ParseCompression` and
  `String` round trip, and bad text is rejected.
- A pinned record: `EncodeRecordWith` at zstd level 0 gives, for a fixed
  compressible input, the bytes 0.9.0's `EncodeRecord` gave (captured from the
  v0.9.0 tag before the change). `EncodeRecord` gives a raw record.
- The wire writer is raw by default and honours `WithCompression`. The reader
  reads a pack that mixes all three codecs.

Go, `packstore`:

- A default store writes raw: `StoredSize` equals the payload length for
  compressible data.
- `WithCompression` with zstd and with lz4 compresses, on `Put`, `WriteBatch`
  and `WriteParallel`.
- The callback receives the key, the bytes and the store's value. Returning
  `Compression{}` stores raw, and returning another value writes that codec.
- An invalid value from the callback fails the write and the object is absent
  afterwards.
- The callback is not called for a dedup hit or for a `Record` object.
- `WriteParallel` with a callback passes under the race detector.
- `Open` rejects an invalid `WithCompression` value.
- A repair replacement is encoded with the handle's setting.
- A store holding raw, zstd and lz4 records reopens under a different setting
  and returns every object, and it survives compaction, a GC cycle, a scrub,
  and recovery of an active segment from its sidecar and from a prefix scan.
- Tests that relied on compression by default now ask for zstd explicitly.

Go, CLI: an end-to-end run that ingests with `--compression lz4:9` and restores
through a handle opened with the default, and a rejected bad value.

Rust: the same tests, ported, and in addition:

- `vectorgen` asks for zstd explicitly where it relied on the default.
  `records_compressed.json` and `segments_go` must come out byte-identical to
  the committed ones.
- A new `records_lz4.json`: Go-encoded lz4 records, at the fast level and at an
  HC level, which Rust parses and decodes to the expected payloads.
- `interop/check.sh` adds a compressible file of 1 MiB to its tree, and for
  each of `zstd:19` and `lz4:9` has each CLI ingest into a store of its own
  with that setting, then has the other CLI list and export it. The outputs
  must be byte-identical to those of the default stores, and each compressed
  packstore must be smaller than the default one, which shows that compressed
  records were written.

## Documentation

- `architecture/amberpack.md`, byte-identical in both repos: the flags byte as
  a codec id, compression as the writer's choice, the generalized invariants,
  the compatibility note.
- `README.md` in both repos: the options and the changed default.
- core-rs: `PORTING.md` (the lz4 dependency row, the level mapping, the Go
  names of the new API), `VECTORS.md` (the new vector file),
  `port-notes/amberpack.md` and `port-notes/vectorgen.md`.

## Delivery

Two pull requests named `compression-options`, each naming the other as its
counterpart. The Rust side is developed against the Go branch before anything
is tagged: `vectorgen` on the Go pseudo-version, and `interop/check.sh` with
`AMBER_GO_REPO` pointing at the Go worktree.

The release is a separate step, taken when the user asks: 0.10.0 on both sides,
Go first. Its notes open with the changed default and the lz4 compatibility
rule.
