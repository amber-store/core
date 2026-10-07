# Compression Options Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A caller that opens a packstore chooses the compression of what it writes (none, zstd or lz4, each at a level), may choose per object through a callback, and gets no compression unless it asks; the same in Go (`amber-store/core`) and Rust (`amber-store/core-rs`).

**Architecture:** The record's flags byte becomes a codec id (0 raw, 1 zstd, 2 lz4). `amberpack` owns a `Compression` value and an encoder that takes one; decoding dispatches on the codec id and never looks at any setting. `packstore` gains two open-time options, a fixed `Compression` and a callback that returns the `Compression` for one object, and routes its four encode sites through one helper. Part 1 (Tasks 1–8) is the Go side, Part 2 (Tasks 9–17) the Rust port, which is developed against the pushed Go branch.

**Tech Stack:** Go 1.26 with `klauspost/compress/zstd` and `github.com/pierrec/lz4/v4`; Rust 2024 with the `zstd` and `lz4` crates (both build C).

**Spec:** `docs/superpowers/specs/2026-10-07-compression-options-design.md` (same branch). Read it before starting.

## Global Constraints

- Codec ids: `0` raw, `1` zstd frame, `2` lz4 block. Values 3–255 are rejected with the existing message `unknown record flags`, wrapping `ErrCorrupt`. The byte keeps the name "flags" in code (`Record.Flags`, the sidecar field).
- Invariants: codec 0 ⟹ `ulen == slen`; any other codec ⟹ `slen < ulen`. A compressed payload is kept only when strictly smaller, otherwise the record is raw.
- An lz4 payload is one LZ4 **block**: no frame header, no size prefix, no checksum.
- No change to the segment magic, the wire pack magic, the footer or the sidecar.
- Valid levels: none `0`; zstd `0–22`, where 0 means 3; lz4 `0–12`, where 0 means the fast compressor. Both languages accept the same ranges. Go maps zstd to klauspost's nearest tier and lz4 levels 10–12 to 9; Rust uses the level as given.
- Text form: `none`, `zstd`, `zstd:LEVEL`, `lz4`, `lz4:LEVEL`. A level of 0 is printed without the `:0`. Only plain decimal levels parse (no sign, no leading zeros, no spaces).
- The default everywhere is no compression: `packstore.Open` without options, `EncodeRecord`, `Writer.Add`.
- zstd at level 0 must produce, in Go, the bytes v0.9.0's `EncodeRecord` produced.
- Go packages stay flat: no `internal/` directory.
- Go commands run with the system Go (1.26.3). Rust commands run as `nix develop -c cargo …` inside the core-rs checkout: the system cargo is too old.
- `architecture/amberpack.md` is byte-identical in the two repos.
- Every commit message ends with these two lines:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01JSP748hiEjkPGTrZXqWRte
  ```
- Pushing a branch and opening a pull request need the user's go-ahead at that moment (Tasks 8 and 17). Do not tag or release.
- Format before each commit: `go fmt ./...` in Go, `nix develop -c cargo fmt` in Rust.
- Delete every binary you build (`go build -o` outputs, the `target/` directory of a scratch worktree) when the work that needed it is done.

**Checkouts.** The Go work happens on branch `compression-options` of `amber-store/core`, which already holds the spec and this plan; `$GO` below is that checkout's root. The Rust work happens on a new branch `compression-options` of `amber-store/core-rs` cut from `origin/main`; `$RS` is its root (Task 9 creates it). Both local clones under `~/amber-store/` lag GitHub, so work in worktrees cut from `origin/main`, never on the clones' `main`.

## Review Focus

Inputs the spec implies but does not spell out, most likely first. Each has its test in the task named.

1. **A callback that returns an invalid value in the middle of a parallel run.** Expected: the run stops with an error wrapping `ErrInvalidCompression`, none of the rejected objects is stored, and the store keeps working afterwards. Tests: Task 4 (`TestCompressionForInvalidValueFailsTheWrite`), Task 12.
2. **Tiny and empty objects under a compressing setting.** A payload of 0, 1 or a dozen bytes cannot shrink; lz4 and zstd must fall back to raw without an error, and the callback must cope with an empty slice. Tests: Task 2 (`TestTinyPayloadsRoundTrip`), Task 4 (`TestTinyObjectsRoundTrip`), Tasks 10 and 12.
3. **Sloppy compression text.** `"zstd:"`, `"zstd:+3"`, `"zstd:03"`, `" zstd"`, `"ZSTD"`, `"lz4:13"`, `"none:1"`, `""`. Expected: every one is rejected with `ErrInvalidCompression`; none is silently read as something else. Tests: Task 1, Task 9, and the CLI in Tasks 6 and 13.
4. **Writing an object that already exists under another codec.** A handle set to zstd puts an object the store already holds as lz4. Expected: a dedup hit, the callback is not asked, and no second copy is written. Tests: Task 4 (`TestPutDedupsAcrossCodecs`), Task 12.
5. **Options given more than once.** `WithCompression` twice, or `WithCompressionFor(nil)` after a callback. Expected: the last one wins and `nil` means no callback. Tests: Task 4 (`TestCompressionOptionsLastOneWins`). Rust's builder cannot take `nil`; Task 12 tests that the last `compression` call wins.

---

# Part 1 — Go (`amber-store/core`)

## File map

| File | Change |
| --- | --- |
| `amberpack/compression.go` | **new**: `Algorithm`, `Compression`, `Validate`, `String`, `ParseCompression`, `ErrInvalidCompression` |
| `amberpack/compression_test.go` | **new** |
| `amberpack/record.go` | codec ids; `EncodeRecordWith`; per-tier zstd encoders; lz4 encode and decode; `EncodeRecord` becomes raw |
| `amberpack/record_test.go` | existing tests ask for zstd explicitly; new codec tests |
| `amberpack/pack.go` | `WriterOption`, `WithCompression`; `Add` uses the writer's setting |
| `amberpack/pack_test.go` | existing compression tests ask for zstd; new writer tests |
| `packstore/compression.go` | **new**: `WithCompression`, `CompressionFunc`, `WithCompressionFor`, `(*Store).encode` |
| `packstore/packstore.go` | `config` fields; `Open` validates; `Put` uses `encode` |
| `packstore/prepare.go`, `parallel.go`, `gc.go`, `repair.go` | `prepare` becomes a method; encode sites use `encode` |
| `packstore/helpers_test.go` and the other `packstore/*_test.go` | existing tests run under `zstdDefault` |
| `packstore/compression_test.go`, `packstore/mixed_codec_test.go` | **new** |
| `cmd/amber-store/main.go`, `store.go`, `e2e_test.go` | `--compression` |
| `architecture/amberpack.md`, `README.md` | documentation |

---

### Task 1: The `Compression` value

**Files:**
- Create: `amberpack/compression.go`
- Test: `amberpack/compression_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Algorithm uint8` with constants `None = 0`, `Zstd = 1`, `LZ4 = 2` and `String()`; `type Compression struct { Algorithm Algorithm; Level int }` with `Validate() error` and `String() string`; `func ParseCompression(s string) (Compression, error)`; `var ErrInvalidCompression`.

- [ ] **Step 1: Write the failing test**

Create `amberpack/compression_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd $GO && go test ./amberpack -run 'Compression' -count=1`
Expected: FAIL to compile with `undefined: Compression`.

- [ ] **Step 3: Write the implementation**

Create `amberpack/compression.go`:

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd $GO && go test ./amberpack -run 'Compression' -count=1`
Expected: `ok  github.com/amber-store/core/amberpack`.

- [ ] **Step 5: Commit**

```bash
cd $GO && git add amberpack/compression.go amberpack/compression_test.go
git commit -m "amberpack: a Compression value: algorithm, level, text form"
```

---

### Task 2: Codec ids, levels and lz4 in the record codec

**Files:**
- Modify: `amberpack/record.go`
- Modify: `amberpack/record_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `Compression`, `Algorithm`, `None`, `Zstd`, `LZ4`, `ErrInvalidCompression` from Task 1.
- Produces: `func EncodeRecordWith(k key.Key, data []byte, c Compression) ([]byte, error)`. `EncodeRecord(k, data)` now equals `EncodeRecordWith(k, data, Compression{})` and writes a raw record. `DecodePayload(flags, ulen, stored)` and `ParseRecord(b)` keep their signatures and accept codec 2. Unexported, for the tests of this package: `compress(c Compression, data []byte) []byte` and `zstdEncoder(level int) *zstd.Encoder`. The constant `flagZstd` and the variable `zstdEnc` are gone.

- [ ] **Step 1: Add the lz4 dependency**

Run: `cd $GO && go get github.com/pierrec/lz4/v4@v4.1.33`
Expected: `go: added github.com/pierrec/lz4/v4 v4.1.33`. (`go mod tidy` in Step 6 moves it to the direct requirements once the code imports it.)

- [ ] **Step 2: Move the existing tests to explicit zstd and write the new ones**

In `amberpack/record_test.go`:

1. Add to the imports: `"crypto/sha256"`, `"encoding/hex"`, `"strconv"`, `"strings"`.
2. Add below the `compressible` helper:

```go
// zstdDefault is what EncodeRecord did before compression became a choice.
var zstdDefault = Compression{Algorithm: Zstd}
```

3. In `TestRecordRoundTripCompressed` and in `TestParseRecordRejectsOversizedUlen`, replace `EncodeRecord(o.Key, o.Bytes)` with `EncodeRecordWith(o.Key, o.Bytes, zstdDefault)`.
4. Replace every `flagZstd` in the file with `byte(Zstd)`. The line `if rec[33]&flagZstd == 0 {` becomes `if rec[33] != byte(Zstd) {`.
5. Replace both `zstdEnc.EncodeAll(` with `zstdEncoder(0).EncodeAll(`.
6. Append these tests:

```go
func TestEncodeRecordDefaultIsRaw(t *testing.T) {
	o := mkObj(t, compressible(64<<10))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	with, err := EncodeRecordWith(o.Key, o.Bytes, Compression{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec, with) {
		t.Fatal("EncodeRecord differs from EncodeRecordWith with the zero Compression")
	}
	r, err := ParseRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Flags != 0 || r.Ulen != r.Slen || int(r.Slen) != len(o.Bytes) {
		t.Fatalf("default record: flags=%#x ulen=%d slen=%d, want raw of %d bytes", r.Flags, r.Ulen, r.Slen, len(o.Bytes))
	}
}

func TestEncodeRecordWithRoundTrip(t *testing.T) {
	o := mkObj(t, compressible(64<<10))
	for _, c := range []Compression{
		{Zstd, 0}, {Zstd, 1}, {Zstd, 3}, {Zstd, 9}, {Zstd, 19}, {Zstd, 22},
		{LZ4, 0}, {LZ4, 1}, {LZ4, 6}, {LZ4, 9}, {LZ4, 12},
	} {
		t.Run(c.String(), func(t *testing.T) {
			rec, err := EncodeRecordWith(o.Key, o.Bytes, c)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ParseRecord(rec)
			if err != nil {
				t.Fatal(err)
			}
			if r.Flags != byte(c.Algorithm) {
				t.Fatalf("flags = %#x, want %#x", r.Flags, byte(c.Algorithm))
			}
			if r.Slen >= r.Ulen || int(r.Ulen) != len(o.Bytes) {
				t.Fatalf("ulen=%d slen=%d for %d payload bytes", r.Ulen, r.Slen, len(o.Bytes))
			}
			if len(rec) != RecHeaderSize+int(r.Slen) {
				t.Fatalf("record is %d bytes, header says %d", len(rec), RecHeaderSize+int(r.Slen))
			}
			got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, o.Bytes) {
				t.Fatal("payload mismatch")
			}
		})
	}
}

func TestIncompressiblePayloadFallsBackToRaw(t *testing.T) {
	for _, c := range []Compression{{}, {Zstd, 0}, {Zstd, 19}, {LZ4, 0}, {LZ4, 9}} {
		for _, data := range [][]byte{nil, {7}, incompressible(13), incompressible(4096)} {
			o := mkObj(t, data)
			rec, err := EncodeRecordWith(o.Key, o.Bytes, c)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, len(data), err)
			}
			r, err := ParseRecord(rec)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, len(data), err)
			}
			if r.Flags != 0 || r.Ulen != r.Slen {
				t.Fatalf("%s, %d bytes: flags=%#x ulen=%d slen=%d, want raw", c, len(data), r.Flags, r.Ulen, r.Slen)
			}
			got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:])
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s, %d bytes: round trip failed: %v", c, len(data), err)
			}
		}
	}
}

// TestTinyPayloadsRoundTrip covers payloads around the sizes where a
// compressor's own framing outweighs what it saves. Which codec the record
// ends up with is the encoder's business; that it parses and decodes is not.
func TestTinyPayloadsRoundTrip(t *testing.T) {
	for _, c := range []Compression{{Zstd, 0}, {Zstd, 22}, {LZ4, 0}, {LZ4, 12}} {
		for n := 0; n <= 64; n++ {
			data := bytes.Repeat([]byte{'a'}, n)
			o := mkObj(t, data)
			rec, err := EncodeRecordWith(o.Key, o.Bytes, c)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, n, err)
			}
			r, err := ParseRecord(rec)
			if err != nil {
				t.Fatalf("%s, %d bytes: %v", c, n, err)
			}
			got, err := DecodePayload(r.Flags, r.Ulen, rec[RecHeaderSize:])
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s, %d bytes: round trip failed: %v", c, n, err)
			}
		}
	}
}

func TestEncodeRecordWithRejectsInvalidCompression(t *testing.T) {
	o := mkObj(t, compressible(1024))
	for _, c := range []Compression{{Zstd, 23}, {LZ4, 13}, {None, 1}, {Algorithm(3), 0}} {
		if _, err := EncodeRecordWith(o.Key, o.Bytes, c); !errors.Is(err, ErrInvalidCompression) {
			t.Errorf("%+v: err = %v, want ErrInvalidCompression", c, err)
		}
	}
}

// TestZstdDefaultMatchesV090 pins the bytes EncodeRecord produced up to
// v0.9.0, when zstd at the default level was all it did: asking for zstd at
// level 0 must give the same record. The second input is one on which
// klauspost's four tiers give four different results, so it also pins the
// tier that level 0 maps to. An upgrade of klauspost/compress may change
// these bytes; re-pin them then, from a run at level 3 before the upgrade's
// own changes to this file.
func TestZstdDefaultMatchesV090(t *testing.T) {
	small := bytes.Repeat([]byte("abcdefgh"), 32)
	k, err := key.New(key.Blob, uint64(len(small)), small)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := EncodeRecordWith(k, small, zstdDefault)
	if err != nil {
		t.Fatal(err)
	}
	const want = "016806850136466d9b5bc95a74cd7721416792f8662bfbd6cd3a5c019f5f0001" +
		"0101000001000000001f3ea9f10728b52ffd4400000085000040616263646566" +
		"6768015408032bf505d630077f"
	if got := hex.EncodeToString(rec); got != want {
		t.Fatalf("small record:\n got %s\nwant %s", got, want)
	}

	var b bytes.Buffer
	for i := 0; b.Len() < 64<<10; i++ {
		b.WriteString(strconv.Itoa(i * 7919))
		b.WriteByte(' ')
	}
	big := b.Bytes()[:64<<10]
	if k, err = key.New(key.Blob, uint64(len(big)), big); err != nil {
		t.Fatal(err)
	}
	for _, level := range []int{0, 3} {
		rec, err := EncodeRecordWith(k, big, Compression{Algorithm: Zstd, Level: level})
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(rec)
		const wantSum = "51cc65a2ec3525b4e09a60f82f91cce7301328b3a164ee0ce9d63f29a95c6541"
		if len(rec) != 23092 || hex.EncodeToString(sum[:]) != wantSum {
			t.Fatalf("level %d: record of %d bytes, sha256 %x; want 23092 bytes, %s", level, len(rec), sum, wantSum)
		}
	}
}

func TestParseRecordRejectsUnknownCodec(t *testing.T) {
	o := mkObj(t, incompressible(1024))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range []byte{3, 4, 0x80, 0xff} {
		bad := bytes.Clone(rec)
		bad[33] = flags
		fixCRC(bad)
		_, err := ParseRecord(bad)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "unknown record flags") {
			t.Errorf("flags %#x: err = %v, want ErrCorrupt naming unknown record flags", flags, err)
		}
	}
	// A compressed codec on a payload that is not smaller breaks the invariant.
	for _, flags := range []byte{byte(Zstd), byte(LZ4)} {
		bad := bytes.Clone(rec)
		bad[33] = flags
		fixCRC(bad)
		if _, err := ParseRecord(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("flags %#x with slen == ulen: err = %v, want ErrCorrupt", flags, err)
		}
	}
}

func TestDecodePayloadLZ4Errors(t *testing.T) {
	data := compressible(4096)
	block := compress(Compression{Algorithm: LZ4}, data)
	if block == nil {
		t.Fatal("test needs an lz4 block")
	}
	if got, err := DecodePayload(byte(LZ4), uint32(len(data)), block); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the block itself must decode: %v", err)
	}
	cases := map[string]struct {
		ulen   uint32
		stored []byte
	}{
		"garbage":            {100, []byte("definitely not lz4 \xff\xff\xff\xff")},
		"truncated block":    {uint32(len(data)), block[:len(block)/2]},
		"empty block":        {uint32(len(data)), nil},
		"shorter than ulen":  {uint32(len(data)) + 1, block},
		"longer than ulen":   {uint32(len(data)) - 1, block},
		"much longer":        {16, block},
		"claims zero length": {0, block},
	}
	for name, c := range cases {
		if _, err := DecodePayload(byte(LZ4), c.ulen, c.stored); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
	t.Run("bomb stops at ulen", func(t *testing.T) {
		bomb := compress(Compression{Algorithm: LZ4}, make([]byte, 64<<20))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodePayload(byte(LZ4), 1024, bomb)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("want ErrCorrupt, got %v", err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Fatalf("decoding allocated %d bytes for a 1 KiB ulen", grew)
		}
	})
}

func TestDecodePayloadRejectsUnknownCodec(t *testing.T) {
	for _, flags := range []byte{3, 0x80, 0xff} {
		if _, err := DecodePayload(flags, 4, []byte{1, 2, 3, 4}); !errors.Is(err, ErrCorrupt) {
			t.Errorf("flags %#x: err = %v, want ErrCorrupt", flags, err)
		}
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd $GO && go test ./amberpack -count=1`
Expected: FAIL to compile with `undefined: EncodeRecordWith`, `undefined: zstdEncoder`, `undefined: compress`.

- [ ] **Step 4: Rewrite the codec in `amberpack/record.go`**

Replace the import block, the constants, the zstd coders and `EncodeRecord` (everything from `import (` down to the end of `EncodeRecord`, keeping `ErrCorrupt`, `Record`, `castagnoli` and `zero4` as they are) so that the file reads:

```go
package amberpack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sync"

	"github.com/amber-store/core/key"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

const (
	// RecHeaderSize is the fixed record-header length:
	// tag(1) + key(32) + flags(1) + ulen(4) + slen(4) + crc(4). Payload follows.
	// The flags byte holds the payload's codec id, an Algorithm value.
	RecHeaderSize = 46

	tagChunk byte = 0x01

	// MaxPayload bounds one object's payload, stored or decoded. The length
	// fields are untrusted and size allocations. Real objects are ~1 MiB.
	MaxPayload = 256 << 20
)
```

Keep the `var ( castagnoli …; zero4 … )` block, the `ErrCorrupt` declaration and the `Record` type unchanged. Replace the "Shared zstd coders" block and `init` with:

```go
// zstdDec decodes every zstd record; DecodeAll is safe for concurrent use.
var zstdDec *zstd.Decoder

func init() {
	var err error
	// CapLimit stops DecodeAll at the dst capacity DecodePayload sizes from ulen.
	if zstdDec, err = zstd.NewReader(nil, zstd.WithDecodeAllCapLimit(true), zstd.WithDecoderMaxMemory(MaxPayload)); err != nil {
		panic(err)
	}
}

// zstdEncs holds one encoder per klauspost tier, built on first use: an
// encoder's tables are sized by its tier, and most processes use one tier.
// EncodeAll is safe for concurrent use.
var zstdEncs [zstd.SpeedBestCompression]struct {
	once sync.Once
	enc  *zstd.Encoder
}

// zstdEncoder returns the encoder for a zstd level from 0 to 22. klauspost
// has four tiers where zstd has 22 levels; EncoderLevelFromZstd picks the
// nearest (1–2 fastest, 3–5 default, 6–9 better, 10–22 best).
func zstdEncoder(level int) *zstd.Encoder {
	if level == 0 {
		level = 3 // zstd's own default
	}
	tier := zstd.EncoderLevelFromZstd(level)
	e := &zstdEncs[tier-zstd.SpeedFastest]
	e.once.Do(func() {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(tier))
		if err != nil {
			panic(err) // the options are fixed and valid
		}
		e.enc = enc
	})
	return e.enc
}

// The lz4 compressors carry hash tables and are not safe for concurrent use,
// so they are pooled. Each resets its tables per block: the output depends
// on the input alone.
var (
	lz4Fast = sync.Pool{New: func() any { return new(lz4.Compressor) }}
	lz4HC   = sync.Pool{New: func() any { return new(lz4.CompressorHC) }}
)

// compress returns data compressed as c says, or nil when the payload is to
// be stored raw: c is None, data is empty, the result would not be strictly
// smaller, or the compressor reports data incompressible. c must be valid.
func compress(c Compression, data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	switch c.Algorithm {
	case Zstd:
		if out := zstdEncoder(c.Level).EncodeAll(data, make([]byte, 0, len(data))); len(out) < len(data) {
			return out
		}
	case LZ4:
		// One byte short of data: a block that is not strictly smaller does
		// not fit, which the compressor reports with 0 or an error.
		dst := make([]byte, len(data)-1)
		var n int
		var err error
		if c.Level == 0 {
			lc := lz4Fast.Get().(*lz4.Compressor)
			n, err = lc.CompressBlock(data, dst)
			lz4Fast.Put(lc)
		} else {
			hc := lz4HC.Get().(*lz4.CompressorHC)
			// The HC levels here stop at 9; level n is a search depth of 1<<(8+n).
			hc.Level = lz4.CompressionLevel(1 << (8 + min(c.Level, 9)))
			n, err = hc.CompressBlock(data, dst)
			lz4HC.Put(hc)
		}
		if err == nil && n > 0 {
			return dst[:n]
		}
	}
	return nil
}

// EncodeRecord serializes (k, data) into a complete record with the payload
// stored raw. It is EncodeRecordWith with no compression.
func EncodeRecord(k key.Key, data []byte) ([]byte, error) {
	return EncodeRecordWith(k, data, Compression{})
}

// EncodeRecordWith serializes (k, data) into a complete record, compressing
// the payload as c says when that makes it strictly smaller and storing it
// raw otherwise. An invalid c is an error wrapping ErrInvalidCompression. k
// is written as given; canonical-form validation happens on the read side.
func EncodeRecordWith(k key.Key, data []byte, c Compression) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if !payloadFits(len(data)) {
		return nil, fmt.Errorf("amberpack: object %s too large: %d bytes", k, len(data))
	}
	payload, flags := data, byte(None)
	if comp := compress(c, data); comp != nil {
		payload, flags = comp, byte(c.Algorithm)
	}
	rec := make([]byte, RecHeaderSize+len(payload))
	rec[0] = tagChunk
	copy(rec[1:33], k[:])
	rec[33] = flags
	binary.BigEndian.PutUint32(rec[34:38], uint32(len(data)))
	binary.BigEndian.PutUint32(rec[38:42], uint32(len(payload)))
	copy(rec[RecHeaderSize:], payload)
	// CRC over the whole record; the crc field itself is still zero here.
	binary.BigEndian.PutUint32(rec[42:46], crc32.Checksum(rec, castagnoli))
	return rec, nil
}
```

In `ParseRecord`, replace the three flag checks:

```go
	flags := b[33]
	if flags > byte(LZ4) {
		return Record{}, fmt.Errorf("%w: unknown record flags %#x", ErrCorrupt, flags)
	}
```

```go
	if flags == byte(None) && ulen != slen {
		return Record{}, fmt.Errorf("%w: raw record with ulen %d != slen %d", ErrCorrupt, ulen, slen)
	}
```

```go
	if flags != byte(None) && slen >= ulen {
		return Record{}, fmt.Errorf("%w: compressed record with slen %d >= ulen %d", ErrCorrupt, slen, ulen)
	}
```

Replace `DecodePayload` whole:

```go
// DecodePayload returns caller-owned payload bytes from a record's stored
// payload, decoded as the record's flags byte says. stored may be a read-only
// mmap slice and is never retained.
func DecodePayload(flags byte, ulen uint32, stored []byte) ([]byte, error) {
	switch Algorithm(flags) {
	case None:
		out := make([]byte, len(stored))
		copy(out, stored)
		return out, nil
	case Zstd:
		out, err := zstdDec.DecodeAll(stored, make([]byte, 0, ulen))
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
		}
		if uint32(len(out)) != ulen {
			return nil, fmt.Errorf("%w: decompressed to %d bytes, header says %d", ErrCorrupt, len(out), ulen)
		}
		return out, nil
	case LZ4:
		// The block carries no length of its own: ulen sizes the output, and
		// a block that would run past it fails inside the decoder.
		out := make([]byte, ulen)
		n, err := lz4.UncompressBlock(stored, out)
		if err != nil {
			return nil, fmt.Errorf("%w: lz4: %v", ErrCorrupt, err)
		}
		if uint32(n) != ulen {
			return nil, fmt.Errorf("%w: decompressed to %d bytes, header says %d", ErrCorrupt, n, ulen)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: unknown record flags %#x", ErrCorrupt, flags)
}
```

Update the doc comment of `ErrCorrupt` only if it mentions zstd (it does not). In `amberpack/pack.go`, change the package comment's phrase `per-record-zstd unit` to `individually compressed unit`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd $GO && go test ./amberpack -run 'Record|Decode|Encode|Tiny|Incompressible|Zstd|Compression' -count=1`
Expected: `ok`. Tests in `pack_test.go` that assumed compression by default may fail at this point; Task 3 fixes them. If `TestDecodePayloadLZ4Errors/claims_zero_length` fails because the decoder accepts the block, that is a real bug in the decode path: `n` must equal `ulen` for success, so check the comparison, not the test.

- [ ] **Step 6: Tidy the module and commit**

```bash
cd $GO && go mod tidy && go build ./... && go vet ./amberpack
git add go.mod go.sum amberpack/record.go amberpack/record_test.go amberpack/pack.go
git commit -m "amberpack: codec ids in the record, zstd levels, lz4; EncodeRecord stores raw"
```

Expected from `go build ./...`: no output. `packstore` still compiles, because `EncodeRecord` kept its signature.

---

### Task 3: Compression on the wire writer

**Files:**
- Modify: `amberpack/pack.go`
- Modify: `amberpack/pack_test.go`

**Interfaces:**
- Consumes: `Compression`, `EncodeRecordWith` from Tasks 1 and 2; `zstdDefault` from `record_test.go` (same package).
- Produces: `type WriterOption func(*Writer)`; `func WithCompression(c Compression) WriterOption`; `func NewWriter(w io.Writer, opts ...WriterOption) *Writer`. `Writer.Add` encodes with the writer's compression, none by default.

- [ ] **Step 1: Move the existing tests to explicit zstd and write the new ones**

In `amberpack/pack_test.go`:

1. In `TestRoundTrip_Compressed`, change `w := NewWriter(&buf)` to `w := NewWriter(&buf, WithCompression(zstdDefault))`.
2. In `TestWriter_AddRecord_RoundTrip` and `TestReader_Records_RoundTrip`, change `EncodeRecord(o.Key, o.Bytes)` to `EncodeRecordWith(o.Key, o.Bytes, zstdDefault)`, and in `TestReader_Records_RoundTrip` also change `w := NewWriter(&buf)` to `w := NewWriter(&buf, WithCompression(zstdDefault))` (that test compares what `Add` wrote with what `EncodeRecordWith` returned, so both must use the same setting).
3. Append:

```go
// packCodecs returns the codec id of every record in a wire pack, in order.
func packCodecs(t *testing.T, pack []byte) []Algorithm {
	t.Helper()
	var out []Algorithm
	for rec, err := range NewReader(bytes.NewReader(pack)).Records() {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, Algorithm(rec.Flags))
	}
	return out
}

func TestWriterStoresRawByDefault(t *testing.T) {
	big := mkObj(t, bytes.Repeat([]byte("amber"), 50_000))
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Add(big); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := packCodecs(t, buf.Bytes()); len(got) != 1 || got[0] != None {
		t.Fatalf("codecs = %v, want [none]", got)
	}
	if buf.Len() < len(big.Bytes) {
		t.Fatalf("pack of %d bytes is smaller than its %d-byte raw payload", buf.Len(), len(big.Bytes))
	}
}

func TestWriterWithCompression(t *testing.T) {
	big := mkObj(t, bytes.Repeat([]byte("amber"), 50_000))
	for _, c := range []Compression{{Zstd, 0}, {Zstd, 19}, {LZ4, 0}, {LZ4, 9}} {
		var buf bytes.Buffer
		w := NewWriter(&buf, WithCompression(c))
		if err := w.Add(big); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if got := packCodecs(t, buf.Bytes()); len(got) != 1 || got[0] != c.Algorithm {
			t.Fatalf("%s: codecs = %v", c, got)
		}
		objs, err := collect(t, NewReader(&buf))
		if err != nil || len(objs) != 1 || !bytes.Equal(objs[0].Bytes, big.Bytes) {
			t.Fatalf("%s: read back %d objects, %v", c, len(objs), err)
		}
	}
}

func TestReaderReadsAPackThatMixesCodecs(t *testing.T) {
	objs := []fstree.Object{
		mkObj(t, bytes.Repeat([]byte("raw "), 5000)),
		mkObj(t, bytes.Repeat([]byte("zstd "), 5000)),
		mkObj(t, bytes.Repeat([]byte("lz4 "), 5000)),
		mkObj(t, nil),
	}
	settings := []Compression{{}, {Zstd, 0}, {LZ4, 0}, {LZ4, 9}}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, o := range objs {
		rec, err := EncodeRecordWith(o.Key, o.Bytes, settings[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AddRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := []Algorithm{None, Zstd, LZ4, None} // the empty object cannot shrink
	if got := packCodecs(t, buf.Bytes()); !slices.Equal(got, want) {
		t.Fatalf("codecs = %v, want %v", got, want)
	}
	got, err := collect(t, NewReader(&buf))
	if err != nil || len(got) != len(objs) {
		t.Fatalf("read %d objects, %v", len(got), err)
	}
	for i, o := range objs {
		if got[i].Key != o.Key || !bytes.Equal(got[i].Bytes, o.Bytes) {
			t.Errorf("object %d mismatch", i)
		}
	}
}

func TestWriterWithInvalidCompressionFailsAdd(t *testing.T) {
	w := NewWriter(io.Discard, WithCompression(Compression{Algorithm: Zstd, Level: 99}))
	for range 2 {
		if err := w.Add(mkObj(t, []byte("alpha"))); !errors.Is(err, ErrInvalidCompression) {
			t.Fatalf("Add: err = %v, want ErrInvalidCompression", err)
		}
	}
}
```

Add `"io"` and `"slices"` to the test file's imports if they are not there (`errors` already is).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd $GO && go test ./amberpack -count=1`
Expected: FAIL to compile with `undefined: WithCompression` and `too many arguments in call to NewWriter`.

- [ ] **Step 3: Implement the option in `amberpack/pack.go`**

Replace the `Writer` type and `NewWriter`:

```go
// Writer serializes fstree.Objects into the wire pack format. It is not safe for
// concurrent use; a client wanting parallel uploads creates one Writer per pack.
type Writer struct {
	bw          *bufio.Writer
	wroteHeader bool
	compression Compression
}

// WriterOption configures a Writer.
type WriterOption func(*Writer)

// WithCompression sets the compression Add encodes with. The default is no
// compression. An invalid value fails every Add with an error wrapping
// ErrInvalidCompression. AddRecord is unaffected: it writes records as given.
func WithCompression(c Compression) WriterOption {
	return func(w *Writer) { w.compression = c }
}

// NewWriter returns a Writer emitting to w. The caller owns w and must close it;
// Writer.Close only writes the end marker and flushes.
func NewWriter(w io.Writer, opts ...WriterOption) *Writer {
	pw := &Writer{bw: bufio.NewWriter(w)}
	for _, o := range opts {
		o(pw)
	}
	return pw
}
```

In `Add`, change the encode call:

```go
	rec, err := EncodeRecordWith(o.Key, o.Bytes, w.compression)
```

In the package comment at the top of the file, change the Records line and the sentence after the layout so they no longer say every record is zstd:

```go
//	Records  repeat: one EncodeRecordWith output each — a 46-byte header
//	         (tag 0x01 + key[32] + flags + ulen + slen + CRC) followed by the payload
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd $GO && go test ./amberpack -count=1 && go vet ./amberpack`
Expected: `ok  github.com/amber-store/core/amberpack` and no vet output.

- [ ] **Step 5: Commit**

```bash
cd $GO && git add amberpack/pack.go amberpack/pack_test.go
git commit -m "amberpack: the wire writer takes a compression; none by default"
```

---

### Task 4: Packstore options

**Files:**
- Create: `packstore/compression.go`
- Create: `packstore/compression_test.go`
- Modify: `packstore/packstore.go` (the `config` struct at lines 41–48, `Open` at 187–191, `WriteBatch` at 466, `Put` at 508)
- Modify: `packstore/prepare.go`, `packstore/parallel.go:138`, `packstore/gc.go:191`, `packstore/repair.go:89`
- Modify: `packstore/helpers_test.go` and, mechanically, the other `packstore/*_test.go`

**Interfaces:**
- Consumes: `amberpack.Compression`, `amberpack.EncodeRecordWith`, `amberpack.ErrInvalidCompression`, `amberpack.Algorithm`.
- Produces: `func WithCompression(c amberpack.Compression) Option`; `type CompressionFunc func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression`; `func WithCompressionFor(f CompressionFunc) Option`; unexported `func (s *Store) encode(k key.Key, data []byte) ([]byte, error)`; `prepare` becomes `func (s *Store) prepare(obj Object, verify bool) ([]byte, int64, error)`. For tests of this package: `var zstdDefault`, `var lz4Fast`, `var zstd19`, `func codecOf(t, s, k) amberpack.Algorithm`, `func rawStore(t, opts...) *Store`, `func distinct(t, n) []Object`.

- [ ] **Step 1: Keep the existing tests on zstd**

The existing tests were written when every store compressed with zstd, and many rely on it: record sizes that decide when a segment rotates, stores that mix raw and compressed records. They keep running under that setting; the new tests cover the new default.

Add to `packstore/helpers_test.go`, with `"github.com/amber-store/core/amberpack"` added to its imports:

```go
// zstdDefault is what every store wrote with before compression became an
// option. The tests written then still run under it, so they keep covering
// stores that mix raw and compressed records.
var zstdDefault = amberpack.Compression{Algorithm: amberpack.Zstd}
```

Then run, from `$GO/packstore`:

```bash
perl -pi -e 's/amberpack\.EncodeRecord\((.*)\)$/amberpack.EncodeRecordWith($1, zstdDefault)/' *_test.go
perl -pi -e 's/(?<![\w.])Open\((.*)\)$/Open($1, WithCompression(zstdDefault))/ unless /opts\.\.\./' *_test.go
git diff --stat
```

Expected: 14 `EncodeRecord` call sites changed across `durable_dedup_test.go`, `record_test.go`, `recover_test.go`, `footer_test.go`, `repair_test.go`, `verify_test.go`, `parallel_test.go` and `packstore_test.go`; 15 `Open(` call sites changed across `compact_concurrent_test.go`, `concurrent_view_test.go`, `compact_test.go`, `multi_test.go`, `process_test.go`, `repair_test.go`, `packstore_test.go` and `verify_test.go`. The three `Open` calls inside `if` conditions that expect an error (`multi_test.go:90`, `packstore_test.go:540`, `packstore_test.go:937`) and `os.Open` are untouched.

The two helpers that forward `opts...` are edited by hand. In `packstore/packstore_test.go` (`openStore`) and `packstore/repair_test.go` (`repairStore`), replace the `Open` line:

```go
	s, err := Open(dir, append([]Option{WithCompression(zstdDefault)}, opts...)...)
```

(`repairStore` passes `t.TempDir()` where `openStore` passes `dir`.) The option comes first, so a test that passes its own `WithCompression` still wins.

- [ ] **Step 2: Write the new tests**

Create `packstore/compression_test.go`:

```go
package packstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

var (
	lz4Fast = amberpack.Compression{Algorithm: amberpack.LZ4}
	zstd19  = amberpack.Compression{Algorithm: amberpack.Zstd, Level: 19}
)

// codecOf returns the codec id of k's stored record.
func codecOf(t *testing.T, s *Store, k key.Key) amberpack.Algorithm {
	t.Helper()
	rec, err := s.GetRecord(k)
	if err != nil {
		t.Fatalf("GetRecord(%s): %v", k, err)
	}
	return amberpack.Algorithm(rec[33])
}

// rawStore opens a store with the given options and nothing else: unlike
// openStore it adds no compression of its own.
func rawStore(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), append([]Option{WithSync(false)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// distinct returns n compressible objects that differ from one another.
func distinct(t *testing.T, n int) []Object {
	t.Helper()
	objs := make([]Object, n)
	for i := range objs {
		objs[i] = blobObj(t, append(compressible(4096), byte(i), byte(i>>8)))
	}
	return objs
}

func mustGet(t *testing.T, s *Store, o Object) {
	t.Helper()
	data, err := s.Get(o.Key)
	if err != nil || !bytes.Equal(data, o.Data) {
		t.Fatalf("Get(%s): %d bytes, %v; want the %d bytes stored", o.Key, len(data), err, len(o.Data))
	}
}

func TestDefaultStoreWritesRaw(t *testing.T) {
	s := rawStore(t)
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.None {
		t.Fatalf("codec = %s, want none", c)
	}
	if n, ok, err := s.StoredSize(o.Key); err != nil || !ok || n != uint64(len(o.Data)) {
		t.Fatalf("StoredSize = %d, %v, %v; want %d", n, ok, err, len(o.Data))
	}
	mustGet(t, s, o)
}

func TestWithCompressionOnEveryWritePath(t *testing.T) {
	paths := map[string]func(*Store, []Object) error{
		"Put": func(s *Store, objs []Object) error {
			for _, o := range objs {
				if err := s.Put(o.Key, o.Data); err != nil {
					return err
				}
			}
			return nil
		},
		"PutVerified": func(s *Store, objs []Object) error {
			for _, o := range objs {
				if err := s.PutVerified(o.Key, o.Data); err != nil {
					return err
				}
			}
			return nil
		},
		"WriteBatch": func(s *Store, objs []Object) error { return s.WriteBatch(objSeq(objs, -1)) },
		"WriteParallel": func(s *Store, objs []Object) error {
			_, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4})
			return err
		},
	}
	for _, c := range []amberpack.Compression{zstdDefault, zstd19, lz4Fast, {Algorithm: amberpack.LZ4, Level: 9}} {
		for name, write := range paths {
			t.Run(c.String()+"/"+name, func(t *testing.T) {
				s := rawStore(t, WithCompression(c))
				objs := distinct(t, 6)
				if err := write(s, objs); err != nil {
					t.Fatal(err)
				}
				for _, o := range objs {
					if got := codecOf(t, s, o.Key); got != c.Algorithm {
						t.Fatalf("codec = %s, want %s", got, c.Algorithm)
					}
					mustGet(t, s, o)
				}
			})
		}
	}
}

func TestCompressionForReceivesKeyDataAndDefault(t *testing.T) {
	type call struct {
		k    key.Key
		data []byte
		def  amberpack.Compression
	}
	var mu sync.Mutex
	var calls []call
	s := rawStore(t, WithCompression(lz4Fast), WithCompressionFor(
		func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
			mu.Lock()
			calls = append(calls, call{k, bytes.Clone(data), def})
			mu.Unlock()
			return def
		}))
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].k != o.Key || !bytes.Equal(calls[0].data, o.Data) || calls[0].def != lz4Fast {
		t.Fatalf("callback saw %d calls: %+v", len(calls), calls)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.LZ4 {
		t.Fatalf("codec = %s, want lz4", c)
	}
}

func TestCompressionForChoosesPerObject(t *testing.T) {
	// The store's setting is lz4. The callback stores "raw…" objects raw,
	// sends "zstd…" objects to zstd 19 and accepts the setting for the rest.
	choose := func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
		switch {
		case bytes.HasPrefix(data, []byte("raw")):
			return amberpack.Compression{}
		case bytes.HasPrefix(data, []byte("zstd")):
			return zstd19
		}
		return def
	}
	s := rawStore(t, WithCompression(lz4Fast), WithCompressionFor(choose))
	for prefix, want := range map[string]amberpack.Algorithm{
		"raw":   amberpack.None,
		"zstd":  amberpack.Zstd,
		"other": amberpack.LZ4,
	} {
		o := blobObj(t, append([]byte(prefix), compressible(4096)...))
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
		if got := codecOf(t, s, o.Key); got != want {
			t.Errorf("%s object: codec = %s, want %s", prefix, got, want)
		}
		mustGet(t, s, o)
	}
}

func TestCompressionForIsAskedWhenTheSettingIsNone(t *testing.T) {
	var sawDef atomic.Value
	s := rawStore(t, WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			sawDef.Store(def)
			return zstdDefault
		}))
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if def, ok := sawDef.Load().(amberpack.Compression); !ok || def != (amberpack.Compression{}) {
		t.Fatalf("callback saw def = %v, want the zero Compression", sawDef.Load())
	}
	if c := codecOf(t, s, o.Key); c != amberpack.Zstd {
		t.Fatalf("codec = %s, want zstd", c)
	}
}

func TestCompressionForInvalidValueFailsTheWrite(t *testing.T) {
	var bad atomic.Bool
	bad.Store(true)
	s := rawStore(t, WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			if bad.Load() {
				return amberpack.Compression{Algorithm: amberpack.Zstd, Level: 99}
			}
			return def
		}))
	objs := distinct(t, 8)
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, amberpack.ErrInvalidCompression) {
			t.Fatalf("%s: err = %v, want ErrInvalidCompression", name, err)
		}
	}
	err := s.Put(objs[0].Key, objs[0].Data)
	check("Put", err)
	if !strings.Contains(err.Error(), objs[0].Key.String()) {
		t.Fatalf("Put error %q does not name the key", err)
	}
	check("PutVerified", s.PutVerified(objs[0].Key, objs[0].Data))
	check("WriteBatch", s.WriteBatch(objSeq(objs, -1)))
	_, err = s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4})
	check("WriteParallel", err)
	for _, o := range objs {
		if has, err := s.Has(o.Key); err != nil || has {
			t.Fatalf("Has(%s) = %v, %v after the rejected writes", o.Key, has, err)
		}
	}
	// The failure is that object's, not the store's: it keeps working.
	bad.Store(false)
	if _, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 4}); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		mustGet(t, s, o)
	}
}

func TestCompressionForIsNotAskedForDedupHitsOrRecords(t *testing.T) {
	var calls atomic.Int64
	s := rawStore(t, WithCompression(zstdDefault), WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			calls.Add(1)
			return def
		}))
	o := blobObj(t, compressible(4096))
	for range 2 {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WriteBatch(objSeq([]Object{o, o}, -1)); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("callback ran %d times for one new object and three dedup hits", n)
	}
	// A pre-encoded record is appended as it is and keeps its codec.
	var recs []Object
	for i := range 4 {
		p := blobObj(t, append(compressible(4096), 0xEE, byte(i)))
		rec, err := amberpack.EncodeRecordWith(p.Key, p.Data, lz4Fast)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, Object{Key: p.Key, Record: rec})
	}
	if err := s.WriteBatch(objSeq(recs[:2], -1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteParallel(objSeq(recs[2:], -1), WriteOpts{Writers: 2}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("callback ran %d times; pre-encoded records must not reach it", n)
	}
	for _, r := range recs {
		if c := codecOf(t, s, r.Key); c != amberpack.LZ4 {
			t.Fatalf("pre-encoded record stored as %s, want lz4", c)
		}
	}
}

func TestPutDedupsAcrossCodecs(t *testing.T) {
	dir := t.TempDir()
	o := blobObj(t, compressible(4096))
	a, err := Open(dir, WithSync(false), WithCompression(lz4Fast))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	b, err := Open(dir, WithSync(false), WithCompression(zstdDefault), WithCompressionFor(
		func(_ key.Key, _ []byte, def amberpack.Compression) amberpack.Compression {
			calls.Add(1)
			return def
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("callback ran %d times for an object the store already holds", n)
	}
	if c := codecOf(t, b, o.Key); c != amberpack.LZ4 {
		t.Fatalf("codec = %s, want the lz4 record written first", c)
	}
}

func TestCompressionForUnderParallelWriters(t *testing.T) {
	var calls atomic.Int64
	s := rawStore(t, WithCompression(lz4Fast), WithCompressionFor(
		func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
			calls.Add(1)
			if data[len(data)-2]%2 == 0 { // distinct's low index byte
				return zstdDefault
			}
			return def
		}))
	objs := distinct(t, 200)
	stats, err := s.WriteParallel(objSeq(objs, -1), WriteOpts{Writers: 8})
	if err != nil || stats.Stored != len(objs) {
		t.Fatalf("WriteParallel: %+v, %v", stats, err)
	}
	if n := calls.Load(); n != int64(len(objs)) {
		t.Fatalf("callback ran %d times for %d objects", n, len(objs))
	}
	for i, o := range objs {
		want := amberpack.LZ4
		if i%2 == 0 {
			want = amberpack.Zstd
		}
		if got := codecOf(t, s, o.Key); got != want {
			t.Fatalf("object %d: codec = %s, want %s", i, got, want)
		}
	}
}

func TestOpenRejectsInvalidCompression(t *testing.T) {
	for _, c := range []amberpack.Compression{
		{Algorithm: amberpack.Zstd, Level: 23},
		{Algorithm: amberpack.LZ4, Level: -1},
		{Algorithm: amberpack.None, Level: 1},
		{Algorithm: amberpack.Algorithm(9)},
	} {
		dir := filepath.Join(t.TempDir(), "store")
		s, err := Open(dir, WithCompression(c))
		if !errors.Is(err, amberpack.ErrInvalidCompression) {
			if s != nil {
				s.Close()
			}
			t.Fatalf("%+v: err = %v, want ErrInvalidCompression", c, err)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("%+v: Open created %s before rejecting the option", c, dir)
		}
	}
}

func TestCompressionOptionsLastOneWins(t *testing.T) {
	never := func(key.Key, []byte, amberpack.Compression) amberpack.Compression { return zstd19 }
	s := rawStore(t,
		WithCompression(zstdDefault), WithCompressionFor(never),
		WithCompression(lz4Fast), WithCompressionFor(nil))
	o := blobObj(t, compressible(4096))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.LZ4 {
		t.Fatalf("codec = %s, want lz4: the later options replace the earlier ones", c)
	}
}

func TestTinyObjectsRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 2, 12, 13, 64}
	for _, c := range []amberpack.Compression{zstdDefault, lz4Fast, {Algorithm: amberpack.LZ4, Level: 9}} {
		var seen []int
		s := rawStore(t, WithCompression(c), WithCompressionFor(
			func(_ key.Key, data []byte, def amberpack.Compression) amberpack.Compression {
				seen = append(seen, len(data))
				return def
			}))
		for _, n := range sizes {
			o := blobObj(t, bytes.Repeat([]byte{'a'}, n))
			if err := s.Put(o.Key, o.Data); err != nil {
				t.Fatalf("%s, %d bytes: %v", c, n, err)
			}
			mustGet(t, s, o)
		}
		if !slices.Equal(seen, sizes) {
			t.Fatalf("%s: callback saw sizes %v, want %v", c, seen, sizes)
		}
		// Verify re-parses every record: the length invariants hold.
		if err := s.Verify(context.Background()); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
}

func TestRepairReplacementUsesTheHandlesCompression(t *testing.T) {
	var useLZ4 atomic.Bool
	s := rawStore(t, WithCompressionFor(
		func(key.Key, []byte, amberpack.Compression) amberpack.Compression {
			if useLZ4.Load() {
				return lz4Fast
			}
			return zstdDefault
		}))
	o := blobObj(t, compressible(8192))
	if err := s.Put(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.Zstd {
		t.Fatalf("first record stored as %s, want zstd", c)
	}
	off := s.active.index[o.Key].off
	if err := s.sealActiveLocked(); err != nil {
		t.Fatal(err)
	}
	damageRepairRecord(t, s.sealed[0].path, off+amberpack.RecHeaderSize)
	useLZ4.Store(true)
	if err := s.PutVerified(o.Key, o.Data); err != nil {
		t.Fatal(err)
	}
	if c := codecOf(t, s, o.Key); c != amberpack.LZ4 {
		t.Fatalf("replacement stored as %s, want lz4", c)
	}
	mustGet(t, s, o)
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd $GO && go test ./packstore -count=1`
Expected: FAIL to compile with `undefined: WithCompression` and `undefined: WithCompressionFor`.

- [ ] **Step 4: Implement the options**

Create `packstore/compression.go`:

```go
package packstore

import (
	"errors"
	"fmt"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// WithCompression sets the compression for the objects this store encodes:
// those given to Put, PutVerified and PutVerifiedDeferred, and those a batch
// carries as Data. The default is no compression. The setting belongs to
// this handle and is stored nowhere; reading never depends on it, and
// records that arrive already encoded keep the codec they have.
func WithCompression(c amberpack.Compression) Option {
	return func(cfg *config) { cfg.compression = c }
}

// CompressionFunc returns the compression to use for one object. def is the
// store's WithCompression value: returning it accepts the store's setting,
// returning the zero Compression stores the object raw, and anything else
// overrides the setting for this object.
//
// It runs on whichever goroutine is writing, several at once under
// WriteParallel, so it must be safe for concurrent use. It must not modify
// or keep data, and it must not call the store: a repair calls it with the
// store's append lock held. A returned value that does not validate fails
// that object's write with an error wrapping amberpack.ErrInvalidCompression.
type CompressionFunc func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression

// WithCompressionFor makes the store ask f for every object it encodes,
// whatever the WithCompression value is. f is not asked for an object the
// store already holds, nor for a pre-encoded record. A nil f means no
// callback.
func WithCompressionFor(f CompressionFunc) Option {
	return func(cfg *config) { cfg.compressionFor = f }
}

// encode returns the record to append for (k, data), compressed as this
// store's options say.
func (s *Store) encode(k key.Key, data []byte) ([]byte, error) {
	c := s.cfg.compression
	if s.cfg.compressionFor != nil {
		c = s.cfg.compressionFor(k, data, c)
	}
	rec, err := amberpack.EncodeRecordWith(k, data, c)
	if errors.Is(err, amberpack.ErrInvalidCompression) {
		return nil, fmt.Errorf("packstore: object %s: %w", k, err)
	}
	return rec, err
}
```

In `packstore/packstore.go`, extend `config`:

```go
type config struct {
	segmentSize    int64
	sync           bool
	compression    amberpack.Compression
	compressionFor CompressionFunc
}
```

In `Open`, validate right after the options are applied and before `os.MkdirAll`:

```go
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.compression.Validate(); err != nil {
		return nil, fmt.Errorf("packstore: %w", err)
	}
```

In `Put`, replace `rec, err := amberpack.EncodeRecord(k, data)` with:

```go
	rec, err := s.encode(k, data)
```

In `WriteBatch`, replace `rec, _, err := prepare(obj, false)` with `rec, _, err := s.prepare(obj, false)`.

In `packstore/parallel.go`, replace `rec, ulen, err := prepare(obj, verify)` with `rec, ulen, err := s.prepare(obj, verify)`.

In `packstore/gc.go`, replace `rec, _, err := prepare(Object{Key: k, Record: raw}, false)` with `rec, _, err := s.prepare(Object{Key: k, Record: raw}, false)`.

In `packstore/repair.go`, replace `replacement, err := amberpack.EncodeRecord(k, data)` with:

```go
	replacement, err := s.encode(k, data)
```

In `packstore/prepare.go`, make `prepare` a method and encode through the store. Its comment and first lines become:

```go
// prepare returns the record to append for obj and the payload length the
// write stats charge for it. For Data that is the store's encoding (encode)
// after the optional verification; for a pre-encoded Record it is the record
// itself, after ParseRecord (framing, flags, length invariants, CRC,
// canonical key), a check that the record names obj.Key and is exactly one
// record long, and, with verify, a decode and rehash of the payload. Every
// rejection of a Record wraps ErrCorrupt, a verification failure ErrVerify.
func (s *Store) prepare(obj Object, verify bool) ([]byte, int64, error) {
	if obj.Record == nil {
		if verify {
			if err := verifyObject(obj); err != nil {
				return nil, 0, err
			}
		}
		rec, err := s.encode(obj.Key, obj.Data)
		if err != nil {
			return nil, 0, err
		}
		return rec, int64(len(obj.Data)), nil
	}
```

The rest of the function is unchanged. If `packstore.go` or `repair.go` no longer uses the `amberpack` import after these edits, the compiler says so; `packstore.go` still uses it for `amberpack.Compression`.

In `packstore/segment.go`, the `Object` doc comment says a `Record` is "the complete record as amberpack.EncodeRecord produced it"; change that to "as amberpack.EncodeRecordWith produced it". Do the same in the `GetRecord` comment in `packstore.go` ("exactly as written by amberpack.EncodeRecord").

- [ ] **Step 5: Run the package's tests**

Run: `cd $GO && go vet ./packstore && go test ./packstore -count=1 && go test -race ./packstore -run 'Compression|Dedups|Tiny|Repair|DefaultStore' -count=1`
Expected: `ok` three times over (vet prints nothing).

If an existing test fails, it is one that the two `perl` commands did not reach and that relies on zstd. The rule: give its store `WithCompression(zstdDefault)` and its `amberpack.EncodeRecord` comparison `EncodeRecordWith(…, zstdDefault)`. Do not change what it asserts.

- [ ] **Step 6: Run everything else that opens a packstore**

Run: `cd $GO && go test ./... -count=1`
Expected: every package `ok`. If `cmd/amber-store` fails to build on a `go:embed` pattern, generate the embedded admin UI first with `nix develop -c go generate ./cmd/amber-store` (its output is ignored by git) and run again.

A test outside `packstore` that fails now relied on compressed sizes through a store opened with the default. Apply the same rule: pass `packstore.WithCompression(amberpack.Compression{Algorithm: amberpack.Zstd})` where that test opens its store.

- [ ] **Step 7: Commit**

```bash
cd $GO && git add packstore
git commit -m "packstore: WithCompression and WithCompressionFor; no compression by default"
```

Add any file outside `packstore` that Step 6 made you change to the same commit.

---

### Task 5: A store that mixes codecs survives the store's machinery

**Files:**
- Create: `packstore/mixed_codec_test.go`

A garbage collection over a store that mixes codecs is tested through the CLI, in Task 6.

**Interfaces:**
- Consumes: `WithCompression`, `codecOf`, `zstdDefault`, `lz4Fast`, `zstd19` from Task 4; the existing `Compact`, `Verify`, `sidecarSuffix`.
- Produces: nothing new. This task is tests only: it proves the read side and the copy paths treat the codec byte as opaque.

- [ ] **Step 1: Write the test**

Create `packstore/mixed_codec_test.go`:

```go
package packstore

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// TestMixedCodecStore fills one store through five handles, each with its own
// compression, and then reads it through a handle with the default: after a
// plain reopen, after losing every sidecar, and after a compaction.
func TestMixedCodecStore(t *testing.T) {
	dir := t.TempDir()
	settings := []amberpack.Compression{
		{}, zstdDefault, lz4Fast, zstd19, {Algorithm: amberpack.LZ4, Level: 9},
	}
	var objs []Object
	for si, c := range settings {
		s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false), WithCompression(c))
		if err != nil {
			t.Fatal(err)
		}
		for i := range 12 {
			data := compressible(3000)
			if i%3 == 0 {
				data = incompressible(3000)
			}
			o := blobObj(t, append(data, byte(si), byte(i)))
			if err := s.Put(o.Key, o.Data); err != nil {
				t.Fatal(err)
			}
			objs = append(objs, o)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	reopen := func() *Store {
		t.Helper()
		s, err := Open(dir, WithSegmentSize(8<<10), WithSync(false))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// readAll checks every object and returns each one's codec.
	readAll := func(s *Store) map[key.Key]amberpack.Algorithm {
		t.Helper()
		codecs := make(map[key.Key]amberpack.Algorithm, len(objs))
		for _, o := range objs {
			data, err := s.Get(o.Key)
			if err != nil || !bytes.Equal(data, o.Data) {
				t.Fatalf("Get(%s): %d bytes, %v", o.Key, len(data), err)
			}
			codecs[o.Key] = codecOf(t, s, o.Key)
		}
		return codecs
	}

	s := reopen()
	before := readAll(s)
	count := map[amberpack.Algorithm]int{}
	for _, c := range before {
		count[c]++
	}
	for _, a := range []amberpack.Algorithm{amberpack.None, amberpack.Zstd, amberpack.LZ4} {
		if count[a] == 0 {
			t.Fatalf("the store holds no %s record: %v", a, count)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Without their sidecars the active segments are indexed by scanning them.
	sidecars, err := filepath.Glob(filepath.Join(dir, "*"+sidecarSuffix))
	if err != nil || len(sidecars) == 0 {
		t.Fatalf("sidecars = %v, %v; want at least one", sidecars, err)
	}
	for _, p := range sidecars {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	s = reopen()
	defer s.Close()
	if got := readAll(s); !maps.Equal(got, before) {
		t.Fatal("codecs changed across a reopen without sidecars")
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Compaction copies the live records as they are.
	live := func(k key.Key) bool { return k[0]%2 == 0 }
	if _, err := s.Compact(live, CompactOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if !live(o.Key) {
			continue
		}
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("after compaction, Get(%s): %d bytes, %v", o.Key, len(data), err)
		}
		if got := codecOf(t, s, o.Key); got != before[o.Key] {
			t.Fatalf("compaction changed %s from %s to %s", o.Key, before[o.Key], got)
		}
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run it**

Run: `cd $GO && go test ./packstore -run TestMixedCodecStore -count=1 -v`
Expected: `--- PASS: TestMixedCodecStore`. Nothing in the store reads the codec byte except `DecodePayload`, so this passes on Task 4's code. A failure here is a finding: stop and report which step failed and with what error, rather than adjusting the test.

- [ ] **Step 3: Check that the test can fail**

Temporarily break the lz4 decode: in `amberpack/record.go`, in the `LZ4` case of `DecodePayload`, change `if uint32(n) != ulen {` to `if uint32(n) == ulen {`. Run the test again.
Expected: FAIL in `readAll` with a `corrupt pack data` error. Restore the line and run once more; expected PASS. `git diff amberpack/record.go` must be empty afterwards.

- [ ] **Step 4: Commit**

```bash
cd $GO && git add packstore/mixed_codec_test.go
git commit -m "packstore: a store that mixes codecs reads, recovers and compacts"
```

---

### Task 6: `--compression` on the CLI

**Files:**
- Modify: `cmd/amber-store/main.go` (the global `Flags` list, after `segment-size`)
- Modify: `cmd/amber-store/store.go` (`openStore`)
- Modify: `cmd/amber-store/e2e_test.go`

**Interfaces:**
- Consumes: `amberpack.ParseCompression`, `packstore.WithCompression`.
- Produces: the global flag `--compression none|zstd[:LEVEL]|lz4[:LEVEL]`, default `none`. Like `--segment-size`, it goes before the subcommand. Part 2's `interop/check.sh` (Task 15) relies on this exact syntax.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/amber-store/e2e_test.go` (and add `"fmt"` to its imports):

```go
// dirSize returns the total size of the regular files under dir.
func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		n += info.Size()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestE2E_Compression(t *testing.T) {
	src := t.TempDir()
	writeFixture(t, src)
	// Numbered lines: they compress well, and unlike one repeated line they
	// do not chunk into identical pieces that dedup away.
	var numbered bytes.Buffer
	for i := range 4000 {
		fmt.Fprintf(&numbered, "line %06d of the compression test\n", i)
	}
	text := numbered.Bytes() // ~140 KiB
	if err := os.WriteFile(filepath.Join(src, "big.txt"), text, 0o644); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, comp := range []string{"none", "zstd:19", "lz4:9"} {
		store := t.TempDir()
		if _, err := runApp(t, "--store", store, "--compression", comp, "ingest", "--no-progress", "--ref", "v1", src); err != nil {
			t.Fatalf("%s: ingest: %v", comp, err)
		}
		// Read back without the flag: reading never depends on the setting.
		dest := t.TempDir()
		if _, err := runApp(t, "--store", store, "restore", "ref:v1", dest); err != nil {
			t.Fatalf("%s: restore: %v", comp, err)
		}
		got, err := os.ReadFile(filepath.Join(dest, "big.txt"))
		if err != nil || !bytes.Equal(got, text) {
			t.Fatalf("%s: restored big.txt: %d bytes, %v", comp, len(got), err)
		}
		sizes[comp] = dirSize(t, filepath.Join(store, "packstore"))
	}
	if sizes["none"] < int64(len(text)) {
		t.Errorf("the default store is %d bytes, smaller than the %d-byte file: something compressed it", sizes["none"], len(text))
	}
	for _, comp := range []string{"zstd:19", "lz4:9"} {
		if sizes[comp] >= sizes["none"]/2 {
			t.Errorf("--compression %s: packstore is %d bytes, the uncompressed one %d", comp, sizes[comp], sizes["none"])
		}
	}
}

func TestE2E_CompressionRejectsBadValues(t *testing.T) {
	src := t.TempDir()
	writeFixture(t, src)
	store := t.TempDir()
	for _, bad := range []string{"gzip", "zstd:23", "zstd:", "lz4:x", "ZSTD", ""} {
		_, err := runApp(t, "--store", store, "--compression", bad, "ingest", "--no-progress", src)
		if err == nil || !strings.Contains(err.Error(), "invalid compression") {
			t.Errorf("--compression %q: err = %v, want an invalid compression error", bad, err)
		}
	}
	if entries, _ := os.ReadDir(store); len(entries) != 0 {
		t.Errorf("a rejected --compression still created %d entries in the store", len(entries))
	}
}

// TestE2E_GCOverMixedCodecs runs a collection over a store that one ingest
// wrote with lz4 and the next with zstd: the tree that stays live holds
// records of both, and restores after the cycle.
func TestE2E_GCOverMixedCodecs(t *testing.T) {
	src := t.TempDir()
	writeFixture(t, src)
	keep := []byte(strings.Repeat("kept across both ingests\n", 3000))
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), keep, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.txt"), []byte(strings.Repeat("first version\n", 3000)), 0o644); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	seg := []string{"--store", store, "--segment-size", "4096"}
	if _, err := runApp(t, append(seg, "--compression", "lz4", "ingest", "--no-progress", "--ref", "v1", src)...); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second := []byte(strings.Repeat("second version\n", 3000))
	if err := os.WriteFile(filepath.Join(src, "big.txt"), second, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runApp(t, append(seg, "--compression", "zstd:19", "ingest", "--no-progress", "--ref", "v1", src)...); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := runApp(t, append(seg, "gc", "run", "--grace", "1ms", "--garbage", "0")...); err != nil {
		t.Fatalf("gc run: %v", err)
	}
	dest := t.TempDir()
	if _, err := runApp(t, append(seg, "restore", "ref:v1", dest)...); err != nil {
		t.Fatalf("restore after gc: %v", err)
	}
	for name, want := range map[string][]byte{"keep.txt": keep, "big.txt": second} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s after gc: %d bytes, %v", name, len(got), err)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd $GO && go test ./cmd/amber-store -run 'E2E_Compression|E2E_GCOverMixedCodecs' -count=1`
Expected: FAIL with `flag provided but not defined: -compression`.

- [ ] **Step 3: Implement the flag**

In `cmd/amber-store/main.go`, add after the `segment-size` flag:

```go
			&cli.StringFlag{
				Name:  "compression",
				Usage: "compression for the objects this command writes: none, zstd[:LEVEL] or lz4[:LEVEL]",
				Value: "none",
			},
```

In `cmd/amber-store/store.go`, add `"github.com/amber-store/core/amberpack"` to the imports and change the start of `openStore`:

```go
func openStore(c *cli.Context) (*packstore.Store, *refstore.Store, error) {
	dir := c.String("store")
	if dir == "" {
		return nil, nil, fmt.Errorf("no store directory: set --store or $AMBER_STORE")
	}
	compression, err := amberpack.ParseCompression(c.String("compression"))
	if err != nil {
		return nil, nil, fmt.Errorf("--compression: %w", err)
	}
	objects, err := packstore.Open(filepath.Join(dir, "packstore"),
		packstore.WithSync(true), packstore.WithSegmentSize(c.Int64("segment-size")),
		packstore.WithCompression(compression))
```

The flag is parsed before anything is opened, so a bad value creates nothing.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd $GO && go test ./cmd/amber-store -count=1`
Expected: `ok  github.com/amber-store/core/cmd/amber-store`.

If `TestE2E_CompressionRejectsBadValues` fails on the empty value `""`: `ParseCompression("")` rejects it, so check that `openStore` passes the flag's string through unchanged rather than substituting a default for an empty one.

- [ ] **Step 5: Commit**

```bash
cd $GO && git add cmd/amber-store
git commit -m "amber-store: --compression none|zstd[:LEVEL]|lz4[:LEVEL]"
```

---

### Task 7: Documentation

**Files:**
- Modify: `architecture/amberpack.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: the names from Tasks 1–6.
- Produces: the text of `architecture/amberpack.md` that Task 16 copies into core-rs byte for byte.

- [ ] **Step 1: Rewrite the record's compression section in `architecture/amberpack.md`**

In the opening paragraph, change `individually-zstd-compressed` to `individually compressed`.

In the record table, replace the `flags` and `payload` rows:

```
33      1     flags    codec id: 0 = raw, 1 = zstd, 2 = lz4; 3–255 reserved (rejected)
```

```
46      slen  payload  raw object bytes, a zstd frame or an LZ4 block, as the codec id says
```

Replace the paragraph that starts `**Compression is opportunistic and per-record.**`, its two bullets and the `ulen`/`slen` sentence after them with:

```markdown
**Compression is per record and is the writer's choice.** For each object the
writer picks none, zstd or lz4, at a level. A packstore takes the choice as an
option when it is opened and may be given a function that chooses per object;
without either it compresses nothing. Whatever is chosen, the compressed
payload is kept only when it is *strictly* smaller than the original; if it
isn't, the payload is stored raw under codec 0. Two invariants follow and are
enforced on parse:

- codec 0 ⟹ `ulen == slen`
- any other codec ⟹ `slen < ulen`

A zstd payload is one zstd frame. An lz4 payload is one LZ4 block: the block
format, without the frame format's header, size prefix or checksum, so a
reader needs `ulen` to decode it. The level is not recorded. It changes what
the writer produces and never how a reader decodes, so two implementations may
map the same level differently and still read each other's records.

The `ulen`/`slen` split lets a reader size its decompression buffer exactly and
detect a payload that decompresses to the wrong length.

**Codec ids and earlier releases.** Codecs 0 and 1 are the two values of what
was a single zstd flag bit, so records written before lz4 existed are valid as
they are. A release from before codec 2 rejects an lz4 record as corrupt
(`unknown record flags`), in a segment and in a wire pack alike: lz4 is for
stores and peers that all run a release that knows it. A later algorithm takes
the next id, and neither the pack version nor the segment version changes for
it.
```

In the section "Why one codec for disk and wire", the sentence `Sharing the codec means an object compressed once on disk can be copied byte-for-byte into a wire pack without re-encoding` stays; append to that paragraph:

```markdown
A copied record keeps the codec it was written with, whatever the
compression setting of the store or writer that copies it.
```

- [ ] **Step 2: Document the option and the flag in `README.md`**

After the "A minimal embedding looks like:" code block, add:

````markdown
Objects are stored uncompressed unless the store is opened with a compression
option:

```go
objects, _ := packstore.Open(filepath.Join(dir, "packstore"),
	packstore.WithCompression(amberpack.Compression{Algorithm: amberpack.Zstd}))
```

`WithCompression` takes none, zstd (levels 1–22) or lz4 (0 for the fast
compressor, 1–12 for high compression); level 0 is each algorithm's default.
`WithCompressionFor` adds a function that chooses per object, from its key and
bytes. Every store reads records of every codec, whatever it was opened with,
and existing records are never recompressed. Releases before this one read raw
and zstd records but not lz4 ones; see
[architecture/amberpack.md](architecture/amberpack.md).
````

In "## The CLI", after the first code block (the two `ingest` lines), add:

````markdown
New objects are stored uncompressed by default. The global flag
`--compression none|zstd[:LEVEL]|lz4[:LEVEL]`, given before the command, sets
the compression for what that command writes:

```sh
amber-store --store ./store --compression zstd:19 ingest ./some/dir
```
````

- [ ] **Step 3: Check the documents against the code**

Run: `cd $GO && grep -n -iE "zstd flag|per-record-zstd|individually-zstd|flags & zstd" -r architecture README.md amberpack packstore`
Expected: no output. Any hit is a leftover description of the single zstd bit; rewrite it in the terms above.

Run: `cd $GO && go test ./... -count=1`
Expected: every package `ok` (nothing changed in code; this guards against an accidental edit).

- [ ] **Step 4: Commit**

```bash
cd $GO && git add architecture/amberpack.md README.md
git commit -m "docs: the codec id, compression as the writer's choice, --compression"
```

---

### Task 8: Verify the Go side and publish the branch

**Files:** none changed.

**Interfaces:**
- Consumes: Tasks 1–7.
- Produces: the pushed branch `compression-options` on `amber-store/core` and its head commit SHA, which Part 2 pins (`$GO_SHA` below).

- [ ] **Step 1: Run what CI runs**

```bash
cd $GO && go vet ./... && go test ./... -count=1 && go test -race ./amberpack ./packstore ./refstore ./gc -count=1
```

Expected: no vet output and every package `ok`.

- [ ] **Step 2: Check the branch holds no stray files**

Run: `cd $GO && git status --short && git log --oneline origin/main..HEAD`
Expected: a clean tree, and the commits of the spec, this plan and Tasks 1–7.

- [ ] **Step 3: Ask the user before publishing**

Pushing and opening the pull request are visible to others. Ask the user: "The Go side is done and green locally. Shall I push `compression-options` to `amber-store/core` and open the pull request?" Continue only on a yes.

- [ ] **Step 4: Push and open the pull request**

```bash
cd $GO && git push -u origin compression-options
gh pr create --repo amber-store/core --base main --head compression-options \
  --title "Compression options: algorithm and level per store, a choice per object, lz4" \
  --body "$(cat <<'EOF'
A caller that opens a packstore now chooses the compression of what it writes: none, zstd or lz4, each at a level, and optionally per object through a callback.

**The default changes: a store opened without options no longer compresses.** `packstore.WithCompression(amberpack.Compression{Algorithm: amberpack.Zstd})` restores what v0.9.0 did, byte for byte. `EncodeRecord` and the wire `Writer.Add` follow the same default; `amberpack.NewWriter` takes `amberpack.WithCompression`.

- The record's flags byte is now a codec id: 0 raw, 1 zstd, 2 lz4. Existing records are valid as they are, and no format version changes.
- Releases up to v0.9.0 read raw and zstd records and reject lz4 ones as corrupt, so turn lz4 on only where every reader runs this release.
- `packstore.WithCompressionFor` asks a callback for each object the store encodes. Records that arrive already encoded, and those copied by compaction and GC, keep their codec.
- `amber-store --compression none|zstd[:LEVEL]|lz4[:LEVEL]`.
- zstd levels map to klauspost's four tiers; lz4 levels above 9 are written at 9.

Design: `docs/superpowers/specs/2026-10-07-compression-options-design.md`.

Rust counterpart: to follow (amber-store/core-rs, same branch name).

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01JSP748hiEjkPGTrZXqWRte
EOF
)"
```

Record the pull request number and `git rev-parse HEAD` (the full SHA, `$GO_SHA`). Task 17 replaces the "to follow" line with the Rust pull request's number, using `gh api -X PATCH repos/amber-store/core/pulls/N -F body=@file` (`gh pr edit` fails on this repository with a Projects deprecation error).


---

# Part 2 — Rust (`amber-store/core-rs`)

Part 2 needs Task 8 done: `tools/vectorgen` and CI pin the pushed Go commit `$GO_SHA`. Every `cargo` and `go` command below runs inside `nix develop -c …` from `$RS`.

The Rust names follow the Go ones. Where a Go test has a Rust port below, the port asserts the same things.

## File map

| File | Change |
| --- | --- |
| `Cargo.toml`, `Cargo.lock` | the `lz4` crate |
| `src/amberpack.rs` | `Compression`, `Error::InvalidCompression`, codec ids, `encode_record_with`, lz4, `Writer::compression`; tests |
| `src/packstore/mod.rs` | `Options::compression`, `Options::compression_for`, `Options::encode`, validation in `open_with`, `Error::is_invalid_compression` |
| `src/packstore/prepare.rs`, `repair.rs`, `parallel.rs` | encode through `Options::encode` |
| `src/packstore/testutil.rs`, `src/packstore/*_tests.rs` | existing tests run under `ZSTD` |
| `src/packstore/compression_tests.rs` | **new** |
| `examples/amber-store.rs`, `tests/cli_e2e.rs` | `--compression` |
| `tools/vectorgen/*.go`, `tests/golden/amberpack/records_lz4.json`, `tests/golden_amberpack.rs` | vectors |
| `interop/check.sh`, `.github/workflows/ci.yml` | cross-language check, the Go pin |
| `architecture/amberpack.md`, `README.md`, `PORTING.md`, `VECTORS.md`, `port-notes/*.md` | documentation |

---

### Task 9: The `Compression` value in Rust

**Files:**
- Modify: `src/amberpack.rs`

**Interfaces:**
- Consumes: nothing.
- Produces: `pub enum Compression { None, Zstd { level: i32 }, Lz4 { level: i32 } }` (`#[non_exhaustive]`, `Default` = `None`, `Copy`, `PartialEq`) with `validate(&self) -> Result<(), Error>`, `Display` and `FromStr<Err = Error>`; `Error::InvalidCompression(String)`; `Error::is_invalid_compression(&self) -> bool`.

- [ ] **Step 1: Create the worktree**

```bash
git -C ~/amber-store/core-rs fetch origin
git -C ~/amber-store/core-rs worktree add -b compression-options <a directory outside the clones>/core-rs origin/main
```

`$RS` is that directory. Run `cd $RS && nix develop -c cargo test --locked` once first.
Expected: all tests pass on the untouched tree (this also builds the dependencies; the macOS run takes several minutes).

- [ ] **Step 2: Write the failing tests**

In `src/amberpack.rs`, inside `mod tests`, add:

```rust
    #[test]
    fn compression_validate() {
        for c in [
            Compression::None,
            Compression::Zstd { level: 0 },
            Compression::Zstd { level: 1 },
            Compression::Zstd { level: 22 },
            Compression::Lz4 { level: 0 },
            Compression::Lz4 { level: 1 },
            Compression::Lz4 { level: 12 },
        ] {
            c.validate().unwrap_or_else(|e| panic!("{c:?}: {e}"));
        }
        for c in [
            Compression::Zstd { level: -1 },
            Compression::Zstd { level: 23 },
            Compression::Lz4 { level: -1 },
            Compression::Lz4 { level: 13 },
        ] {
            let err = c.validate().expect_err("must be invalid");
            assert!(err.is_invalid_compression(), "{c:?}: {err}");
        }
        assert_eq!(Compression::default(), Compression::None);
        assert_eq!(
            Compression::Zstd { level: 23 }.validate().unwrap_err().to_string(),
            "amberpack: invalid compression: zstd level 23, want 0 to 22"
        );
    }

    #[test]
    fn compression_text_form() {
        for (text, c) in [
            ("none", Compression::None),
            ("zstd", Compression::Zstd { level: 0 }),
            ("zstd:19", Compression::Zstd { level: 19 }),
            ("lz4", Compression::Lz4 { level: 0 }),
            ("lz4:9", Compression::Lz4 { level: 9 }),
        ] {
            assert_eq!(c.to_string(), text);
            assert_eq!(text.parse::<Compression>().unwrap(), c, "{text}");
        }
        // An explicit level of 0 parses; it prints without the level.
        for (text, c) in [
            ("zstd:0", Compression::Zstd { level: 0 }),
            ("lz4:0", Compression::Lz4 { level: 0 }),
            ("none:0", Compression::None),
            ("lz4:12", Compression::Lz4 { level: 12 }),
        ] {
            assert_eq!(text.parse::<Compression>().unwrap(), c, "{text}");
        }
    }

    #[test]
    fn compression_parse_rejects_sloppy_text() {
        for text in [
            "", " ", "gzip", "ZSTD", "Zstd", " zstd", "zstd ", "zstd:", "zstd:x", "zstd:+3",
            "zstd:03", "zstd:-1", "zstd:3 ", "zstd:23", "zstd:3:4", "lz4:13", "lz4:-0", "none:1",
            ":3", "lz4hc",
        ] {
            match text.parse::<Compression>() {
                Err(e) => assert!(e.is_invalid_compression(), "{text:?}: {e}"),
                Ok(c) => panic!("{text:?} parsed as {c:?}"),
            }
        }
    }
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd $RS && nix develop -c cargo test --lib amberpack::tests::compression`
Expected: a compile error, `cannot find type Compression in this scope`.

- [ ] **Step 4: Implement**

In `src/amberpack.rs`, add `use std::fmt;` and `use std::str::FromStr;` to the imports. Add this variant to `enum Error`, after `TooLarge`:

```rust
    /// A [`Compression`] the codec does not take, or text that does not name
    /// one (Go: `ErrInvalidCompression`).
    #[error("amberpack: invalid compression: {0}")]
    InvalidCompression(String),
```

and this method to `impl Error`:

```rust
    /// Go's `errors.Is(err, ErrInvalidCompression)`.
    pub fn is_invalid_compression(&self) -> bool {
        matches!(self, Error::InvalidCompression(_))
    }
```

Add, above the `Record` struct:

```rust
/// The highest level each algorithm takes.
const MAX_ZSTD_LEVEL: i32 = 22;
const MAX_LZ4_LEVEL: i32 = 12;

/// How a record's payload is compressed: an algorithm and its level. The
/// default is no compression. Level 0 selects the algorithm's default: 3 for
/// zstd, the fast compressor for lz4. zstd takes levels up to 22 and lz4 up
/// to 12, where 1 and above are its high-compression levels.
///
/// Go: `Compression{Algorithm, Level}`. The Go encoders have fewer distinct
/// levels and map a level to the nearest one they have; this crate uses the
/// level as given. The level changes what is written and never how it is
/// read.
#[non_exhaustive]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Compression {
    /// The payload is stored as it is.
    #[default]
    None,
    /// The payload is one zstd frame.
    Zstd {
        /// 0 to 22; 0 means 3.
        level: i32,
    },
    /// The payload is one LZ4 block.
    Lz4 {
        /// 0 to 12; 0 means the fast compressor.
        level: i32,
    },
}

impl Compression {
    /// Reports whether the level is one the algorithm takes (Go: `Validate`).
    pub fn validate(&self) -> Result<(), Error> {
        let (name, level, max) = match *self {
            Compression::None => return Ok(()),
            Compression::Zstd { level } => ("zstd", level, MAX_ZSTD_LEVEL),
            Compression::Lz4 { level } => ("lz4", level, MAX_LZ4_LEVEL),
        };
        if !(0..=max).contains(&level) {
            return Err(Error::InvalidCompression(format!(
                "{name} level {level}, want 0 to {max}"
            )));
        }
        Ok(())
    }
}

/// The text form [`FromStr`] reads: `none`, `zstd`, `zstd:19`, `lz4`,
/// `lz4:9`. A level of 0 is left out (Go: `Compression.String`).
impl fmt::Display for Compression {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let (name, level) = match *self {
            Compression::None => return f.write_str("none"),
            Compression::Zstd { level } => ("zstd", level),
            Compression::Lz4 { level } => ("lz4", level),
        };
        if level == 0 {
            f.write_str(name)
        } else {
            write!(f, "{name}:{level}")
        }
    }
}

/// Reads the text form: `none`, `zstd`, `zstd:LEVEL`, `lz4` or `lz4:LEVEL`.
/// The result is valid (Go: `ParseCompression`).
impl FromStr for Compression {
    type Err = Error;

    fn from_str(s: &str) -> Result<Compression, Error> {
        let (name, level_text) = match s.split_once(':') {
            Some((name, level)) => (name, Some(level)),
            None => (s, None),
        };
        if !matches!(name, "none" | "zstd" | "lz4") {
            return Err(Error::InvalidCompression(format!(
                "{s:?}: want none, zstd[:LEVEL] or lz4[:LEVEL]"
            )));
        }
        let level = match level_text {
            None => 0,
            // The plain decimal form only: parse also takes a sign and
            // leading zeros.
            Some(text) => match text.parse::<i32>() {
                Ok(n) if n.to_string() == text => n,
                _ => {
                    return Err(Error::InvalidCompression(format!(
                        "{s:?}: bad level {text:?}"
                    )));
                }
            },
        };
        let c = match name {
            "none" if level != 0 => {
                return Err(Error::InvalidCompression(format!(
                    "none takes no level, got {level}"
                )));
            }
            "none" => Compression::None,
            "zstd" => Compression::Zstd { level },
            _ => Compression::Lz4 { level },
        };
        c.validate()?;
        Ok(c)
    }
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `cd $RS && nix develop -c cargo test --lib amberpack::tests::compression`
Expected: `test result: ok. 3 passed`.

If the build fails elsewhere on a non-exhaustive `match` over `amberpack::Error`, add an arm for `InvalidCompression` there that treats it like `TooLarge`.

- [ ] **Step 6: Commit**

```bash
cd $RS && git add src/amberpack.rs
git commit -m "amberpack: a Compression value: algorithm, level, text form"
```

---

### Task 10: Codec ids, levels and lz4 in the Rust record codec

**Files:**
- Modify: `Cargo.toml`, `Cargo.lock`
- Modify: `src/amberpack.rs`

**Interfaces:**
- Consumes: `Compression`, `Error::InvalidCompression` from Task 9.
- Produces: `pub fn encode_record_with(k: Key, data: &[u8], c: Compression) -> Result<Vec<u8>, Error>`. `encode_record(k, data)` now equals `encode_record_with(k, data, Compression::None)`. `parse_record` and `decode_payload` keep their signatures and accept codec 2. Private: `const CODEC_RAW: u8 = 0`, `CODEC_ZSTD: u8 = 1`, `CODEC_LZ4: u8 = 2` replace `FLAG_ZSTD`.

- [ ] **Step 1: Add the dependency**

In `Cargo.toml`, under `[dependencies]`, after the `zstd` line:

```toml
lz4 = "1"
```

Run: `cd $RS && nix develop -c cargo build`
Expected: it builds, and `Cargo.lock` gains `lz4` and `lz4-sys`.

- [ ] **Step 2: Move the existing tests to explicit zstd and write the new ones**

In `src/amberpack.rs`, inside `mod tests`:

1. Add `const ZSTD: Compression = Compression::Zstd { level: 0 };`.
2. Replace every `FLAG_ZSTD` with `CODEC_ZSTD`.
3. In `record_round_trip_compressed`, in `parse_record_rejects_oversized_ulen`, and in the case of `parse_record_rejects_corruption` that asserts `bad[33] == CODEC_ZSTD`, replace `encode_record(X, Y)` with `encode_record_with(X, Y, ZSTD)`.
4. Add:

```rust
    #[test]
    fn encode_record_default_is_raw() {
        let data = compressible(64 << 10);
        let k = mk_key(&data);
        let rec = encode_record(k, &data).unwrap();
        assert_eq!(rec, encode_record_with(k, &data, Compression::None).unwrap());
        let r = parse_record(&rec).unwrap();
        assert_eq!((r.flags, r.ulen, r.slen), (0, data.len() as u32, data.len() as u32));
    }

    #[test]
    fn encode_record_with_round_trip() {
        let data = compressible(64 << 10);
        let k = mk_key(&data);
        for (c, codec) in [
            (Compression::Zstd { level: 0 }, CODEC_ZSTD),
            (Compression::Zstd { level: 1 }, CODEC_ZSTD),
            (Compression::Zstd { level: 9 }, CODEC_ZSTD),
            (Compression::Zstd { level: 19 }, CODEC_ZSTD),
            (Compression::Zstd { level: 22 }, CODEC_ZSTD),
            (Compression::Lz4 { level: 0 }, CODEC_LZ4),
            (Compression::Lz4 { level: 1 }, CODEC_LZ4),
            (Compression::Lz4 { level: 6 }, CODEC_LZ4),
            (Compression::Lz4 { level: 9 }, CODEC_LZ4),
            (Compression::Lz4 { level: 12 }, CODEC_LZ4),
        ] {
            let rec = encode_record_with(k, &data, c).unwrap();
            let r = parse_record(&rec).unwrap_or_else(|e| panic!("{c}: {e}"));
            assert_eq!(r.flags, codec, "{c}");
            assert!(r.slen < r.ulen, "{c}: slen {} ulen {}", r.slen, r.ulen);
            assert_eq!(rec.len(), REC_HEADER_SIZE + r.slen as usize, "{c}");
            let got = decode_payload(r.flags, r.ulen, &rec[REC_HEADER_SIZE..]).unwrap();
            assert_eq!(got, data, "{c}");
        }
        // Level 0 is zstd's default, 3.
        assert_eq!(
            encode_record_with(k, &data, Compression::Zstd { level: 0 }).unwrap(),
            encode_record_with(k, &data, Compression::Zstd { level: 3 }).unwrap()
        );
    }

    #[test]
    fn incompressible_payload_falls_back_to_raw() {
        for c in [
            Compression::None,
            Compression::Zstd { level: 0 },
            Compression::Zstd { level: 19 },
            Compression::Lz4 { level: 0 },
            Compression::Lz4 { level: 9 },
        ] {
            for data in [vec![], vec![7u8], incompressible(13), incompressible(4096)] {
                let rec = encode_record_with(mk_key(&data), &data, c).unwrap();
                let r = parse_record(&rec).unwrap();
                assert_eq!((r.flags, r.ulen), (0, r.slen), "{c}, {} bytes", data.len());
                let got = decode_payload(r.flags, r.ulen, &rec[REC_HEADER_SIZE..]).unwrap();
                assert_eq!(got, data, "{c}");
            }
        }
    }

    /// Payloads around the sizes where a compressor's own framing outweighs
    /// what it saves. Which codec the record ends up with is the encoder's
    /// business; that it parses and decodes is not.
    #[test]
    fn tiny_payloads_round_trip() {
        for c in [
            Compression::Zstd { level: 0 },
            Compression::Zstd { level: 22 },
            Compression::Lz4 { level: 0 },
            Compression::Lz4 { level: 12 },
        ] {
            for n in 0..=64usize {
                let data = vec![b'a'; n];
                let rec = encode_record_with(mk_key(&data), &data, c).unwrap();
                let r = parse_record(&rec).unwrap_or_else(|e| panic!("{c}, {n} bytes: {e}"));
                let got = decode_payload(r.flags, r.ulen, &rec[REC_HEADER_SIZE..]).unwrap();
                assert_eq!(got, data, "{c}, {n} bytes");
            }
        }
    }

    #[test]
    fn encode_record_with_rejects_invalid_compression() {
        let data = compressible(1024);
        for c in [Compression::Zstd { level: 23 }, Compression::Lz4 { level: 13 }] {
            let err = encode_record_with(mk_key(&data), &data, c).unwrap_err();
            assert!(err.is_invalid_compression(), "{c:?}: {err}");
        }
    }

    #[test]
    fn parse_record_rejects_unknown_codec() {
        let data = incompressible(1024);
        let rec = encode_record(mk_key(&data), &data).unwrap();
        for flags in [3u8, 4, 0x80, 0xff] {
            let mut bad = rec.clone();
            bad[33] = flags;
            fix_crc(&mut bad);
            let err = parse_record(&bad).unwrap_err();
            assert!(err.is_corrupt(), "flags {flags:#x}: {err}");
            assert!(err.to_string().contains("unknown record flags"), "{err}");
        }
        // A compressed codec on a payload that is not smaller breaks the
        // invariant.
        for flags in [CODEC_ZSTD, CODEC_LZ4] {
            let mut bad = rec.clone();
            bad[33] = flags;
            fix_crc(&mut bad);
            assert!(parse_record(&bad).unwrap_err().is_corrupt(), "flags {flags:#x}");
        }
    }

    #[test]
    fn decode_payload_lz4_errors() {
        let data = compressible(4096);
        let block = lz4::block::compress(&data, None, false).unwrap();
        assert_eq!(decode_payload(CODEC_LZ4, data.len() as u32, &block).unwrap(), data);
        let cases: [(&str, u32, &[u8]); 7] = [
            ("garbage", 100, b"definitely not lz4 \xff\xff\xff\xff"),
            ("truncated block", data.len() as u32, &block[..block.len() / 2]),
            ("empty block", data.len() as u32, &[]),
            ("shorter than ulen", data.len() as u32 + 1, &block),
            ("longer than ulen", data.len() as u32 - 1, &block),
            ("much longer", 16, &block),
            ("claims zero length", 0, &block),
        ];
        for (name, ulen, stored) in cases {
            match decode_payload(CODEC_LZ4, ulen, stored) {
                Err(e) => assert!(e.is_corrupt(), "{name}: {e}"),
                Ok(out) => panic!("{name}: decoded {} bytes", out.len()),
            }
        }
        // A bomb stops at ulen: the output buffer is ulen bytes and no more.
        let bomb = lz4::block::compress(&vec![0u8; 64 << 20], None, false).unwrap();
        assert!(decode_payload(CODEC_LZ4, 1024, &bomb).unwrap_err().is_corrupt());
    }

    #[test]
    fn decode_payload_rejects_unknown_codec() {
        for flags in [3u8, 0x80, 0xff] {
            let err = decode_payload(flags, 4, &[1, 2, 3, 4]).unwrap_err();
            assert!(err.is_corrupt(), "flags {flags:#x}: {err}");
        }
    }
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd $RS && nix develop -c cargo test --lib amberpack`
Expected: compile errors, `cannot find function encode_record_with` and `cannot find value CODEC_ZSTD`.

- [ ] **Step 4: Implement**

In `src/amberpack.rs`, replace `const FLAG_ZSTD: u8 = 0x01;` with:

```rust
/// The codec ids a record's flags byte holds.
const CODEC_RAW: u8 = 0;
const CODEC_ZSTD: u8 = 1;
const CODEC_LZ4: u8 = 2;
```

Add to `impl Compression`:

```rust
    /// The codec id a record compressed this way carries.
    fn codec(&self) -> u8 {
        match self {
            Compression::None => CODEC_RAW,
            Compression::Zstd { .. } => CODEC_ZSTD,
            Compression::Lz4 { .. } => CODEC_LZ4,
        }
    }
```

Replace `encode_record` with:

```rust
/// Serializes `(k, data)` into a complete record with the payload stored
/// raw. It is [`encode_record_with`] with no compression (Go: `EncodeRecord`).
pub fn encode_record(k: Key, data: &[u8]) -> Result<Vec<u8>, Error> {
    encode_record_with(k, data, Compression::None)
}

/// Returns `data` compressed as `c` says, or `None` when the payload is to be
/// stored raw: `c` is `None`, `data` is empty, or the result is not strictly
/// smaller. A compressor failure (allocation, in practice impossible) stores
/// raw too, which is indistinguishable from "did not get smaller". `c` must
/// be valid.
fn compress(c: Compression, data: &[u8]) -> Option<Vec<u8>> {
    if data.is_empty() {
        return None;
    }
    let out = match c {
        Compression::None => return None,
        Compression::Zstd { level } => {
            let level = if level == 0 {
                zstd::DEFAULT_COMPRESSION_LEVEL
            } else {
                level
            };
            zstd::bulk::compress(data, level).ok()?
        }
        Compression::Lz4 { level } => {
            let mode = if level == 0 {
                lz4::block::CompressionMode::DEFAULT
            } else {
                lz4::block::CompressionMode::HIGHCOMPRESSION(level)
            };
            // No size prefix: the record's ulen is the block's length.
            lz4::block::compress(data, Some(mode), false).ok()?
        }
    };
    (out.len() < data.len()).then_some(out)
}

/// Serializes `(k, data)` into a complete record, compressing the payload as
/// `c` says when that makes it strictly smaller and storing it raw otherwise.
/// An invalid `c` is [`Error::InvalidCompression`]. `k` is written as given;
/// canonical-form validation happens on the read side (Go:
/// `EncodeRecordWith`).
pub fn encode_record_with(k: Key, data: &[u8], c: Compression) -> Result<Vec<u8>, Error> {
    c.validate()?;
    if !payload_fits(data.len()) {
        return Err(Error::TooLarge {
            key: k,
            len: data.len(),
        });
    }
    let comp = compress(c, data);
    let (payload, flags): (&[u8], u8) = match comp.as_deref() {
        Some(p) => (p, c.codec()),
        None => (data, CODEC_RAW),
    };
    let mut rec = vec![0u8; REC_HEADER_SIZE + payload.len()];
    rec[0] = TAG_CHUNK;
    rec[1..33].copy_from_slice(k.as_bytes());
    rec[33] = flags;
    rec[34..38].copy_from_slice(&(data.len() as u32).to_be_bytes());
    rec[38..42].copy_from_slice(&(payload.len() as u32).to_be_bytes());
    rec[REC_HEADER_SIZE..].copy_from_slice(payload);
    // CRC over the whole record; the crc field itself is still zero here.
    let crc = crc32c::crc32c(&rec);
    rec[42..46].copy_from_slice(&crc.to_be_bytes());
    Ok(rec)
}
```

In `parse_record`, replace the three flag checks:

```rust
    let flags = b[33];
    if flags > CODEC_LZ4 {
        return Err(Error::Corrupt(format!("unknown record flags {flags:#x}")));
    }
```

```rust
    if flags == CODEC_RAW && ulen != slen {
```

```rust
    if flags != CODEC_RAW && slen >= ulen {
```

Replace `decode_payload`:

```rust
/// Returns caller-owned payload bytes from a record's stored payload, decoded
/// as the record's flags byte says. `stored` may be a read-only mmap slice
/// and is never retained.
pub fn decode_payload(flags: u8, ulen: u32, stored: &[u8]) -> Result<Vec<u8>, Error> {
    let out = match flags {
        CODEC_RAW => return Ok(stored.to_vec()),
        // The decompression buffer is capped at ulen, so a frame that would
        // expand past the header's claim fails inside zstd rather than
        // allocating; either way the record is Corrupt (Go decodes fully,
        // then reports the length mismatch — same class, slightly different
        // message in that edge).
        CODEC_ZSTD => DECOMPRESSOR
            .with_borrow_mut(|d| -> io::Result<Vec<u8>> {
                if d.is_none() {
                    *d = Some(zstd::bulk::Decompressor::new()?);
                }
                d.as_mut().unwrap().decompress(stored, ulen as usize)
            })
            .map_err(|e| Error::Corrupt(format!("zstd: {e}")))?,
        // The block carries no length of its own: ulen sizes the output, and
        // a block that would run past it fails inside liblz4.
        CODEC_LZ4 => {
            let size = i32::try_from(ulen)
                .map_err(|_| Error::Corrupt(format!("lz4: ulen {ulen} out of range")))?;
            lz4::block::decompress(stored, Some(size))
                .map_err(|e| Error::Corrupt(format!("lz4: {e}")))?
        }
        _ => return Err(Error::Corrupt(format!("unknown record flags {flags:#x}"))),
    };
    if out.len() != ulen as usize {
        return Err(Error::Corrupt(format!(
            "decompressed to {} bytes, header says {}",
            out.len(),
            ulen
        )));
    }
    Ok(out)
}
```

In the module comment at the top of the file, change `per-record-zstd unit` to `individually compressed unit`, and extend the compatibility note:

```rust
//! Compatibility note: record *headers* and raw (uncompressed) records are
//! byte-identical with the Go implementation. Compressed payloads are not
//! (Go uses `klauspost/compress` and `pierrec/lz4`, this port libzstd and
//! liblz4, and the two map levels differently), but each side decodes what
//! the other wrote; see PORTING.md.
```

- [ ] **Step 5: Run the tests**

Run: `cd $RS && nix develop -c cargo test --lib amberpack`
Expected: the tests of Step 2 pass. A test of the wire `Writer` that expected compression (`round_trip_compressed`) fails until Task 11; any other failure is a test that assumed compression by default and that item 3 of Step 2 missed. Give it `ZSTD` explicitly and leave its assertions alone.

- [ ] **Step 6: Commit**

```bash
cd $RS && git add Cargo.toml Cargo.lock src/amberpack.rs
git commit -m "amberpack: codec ids in the record, zstd levels, lz4; encode_record stores raw"
```

---

### Task 11: Compression on the Rust wire writer

**Files:**
- Modify: `src/amberpack.rs`

**Interfaces:**
- Consumes: `Compression`, `encode_record_with`.
- Produces: `pub fn compression(self, c: Compression) -> Writer<W>` on `Writer<W>`. `Writer::add` encodes with the writer's compression, none by default (Go: `NewWriter(w, WithCompression(c))`).

- [ ] **Step 1: Move the existing test to explicit zstd and write the new ones**

In `mod tests`, in `round_trip_compressed`, change `Writer::new(Vec::new())` to `Writer::new(Vec::new()).compression(ZSTD)`. In `add_record_round_trip`, change `encode_record(` to `encode_record_with(` with `ZSTD` as the last argument. Add:

```rust
    /// The codec id of every record in a wire pack, in order.
    fn pack_codecs(pack: &[u8]) -> Vec<u8> {
        Reader::new(pack)
            .records()
            .map(|r| r.unwrap().record.flags)
            .collect()
    }

    #[test]
    fn writer_stores_raw_by_default() {
        let big = b"amber".repeat(50_000);
        let mut w = Writer::new(Vec::new());
        w.add(mk_key(&big), &big).unwrap();
        let out = w.finish().unwrap();
        assert_eq!(pack_codecs(&out), [CODEC_RAW]);
        assert!(out.len() >= big.len());
    }

    #[test]
    fn writer_with_compression() {
        let big = b"amber".repeat(50_000);
        for c in [
            Compression::Zstd { level: 0 },
            Compression::Zstd { level: 19 },
            Compression::Lz4 { level: 0 },
            Compression::Lz4 { level: 9 },
        ] {
            let mut w = Writer::new(Vec::new()).compression(c);
            w.add(mk_key(&big), &big).unwrap();
            let out = w.finish().unwrap();
            assert_eq!(pack_codecs(&out), [c.codec()], "{c}");
            let objs = collect(Reader::new(&out[..])).unwrap();
            assert_eq!(objs, vec![(mk_key(&big), big.clone())], "{c}");
        }
    }

    #[test]
    fn reader_reads_a_pack_that_mixes_codecs() {
        let objs = [
            b"raw ".repeat(5000),
            b"zstd ".repeat(5000),
            b"lz4 ".repeat(5000),
            Vec::new(),
        ];
        let settings = [
            Compression::None,
            Compression::Zstd { level: 0 },
            Compression::Lz4 { level: 0 },
            Compression::Lz4 { level: 9 },
        ];
        let mut w = Writer::new(Vec::new());
        for (data, c) in objs.iter().zip(settings) {
            w.add_record(&encode_record_with(mk_key(data), data, c).unwrap()).unwrap();
        }
        let out = w.finish().unwrap();
        // The empty object cannot shrink.
        assert_eq!(pack_codecs(&out), [CODEC_RAW, CODEC_ZSTD, CODEC_LZ4, CODEC_RAW]);
        let got = collect(Reader::new(&out[..])).unwrap();
        let want: Vec<_> = objs.iter().map(|d| (mk_key(d), d.clone())).collect();
        assert_eq!(got, want);
    }

    #[test]
    fn writer_with_invalid_compression_fails_add() {
        let mut w = Writer::new(Vec::new()).compression(Compression::Zstd { level: 99 });
        for _ in 0..2 {
            assert!(w.add(mk_key(b"alpha"), b"alpha").unwrap_err().is_invalid_compression());
        }
    }
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd $RS && nix develop -c cargo test --lib amberpack`
Expected: a compile error, `no method named compression found for struct Writer`.

- [ ] **Step 3: Implement**

In `src/amberpack.rs`, add the field and the builder method, and use it in `add`:

```rust
pub struct Writer<W: Write> {
    bw: BufWriter<W>,
    wrote_header: bool,
    compression: Compression,
}
```

```rust
    pub fn new(w: W) -> Writer<W> {
        Writer {
            bw: BufWriter::new(w),
            wrote_header: false,
            compression: Compression::None,
        }
    }

    /// Sets the compression [`Writer::add`] encodes with. The default is no
    /// compression. An invalid value fails every `add` with
    /// [`Error::InvalidCompression`]. [`Writer::add_record`] is unaffected:
    /// it writes records as given (Go: `WithCompression`).
    pub fn compression(mut self, c: Compression) -> Writer<W> {
        self.compression = c;
        self
    }
```

```rust
        let rec = encode_record_with(k, data, self.compression)?;
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd $RS && nix develop -c cargo test --lib amberpack`
Expected: `test result: ok`, no failures.

- [ ] **Step 5: Commit**

```bash
cd $RS && git add src/amberpack.rs
git commit -m "amberpack: the wire writer takes a compression; none by default"
```

---

### Task 12: Packstore options in Rust

**Files:**
- Modify: `src/packstore/mod.rs` (`Options` at lines 271–306, `open_with` at 494, `write_batch` at 792, `put` at 827, `impl Error` near 240, the test-module list near 1207)
- Modify: `src/packstore/prepare.rs`, `src/packstore/parallel.rs:228`, `src/packstore/repair.rs:102` and `:123`
- Modify: `src/packstore/testutil.rs` and, mechanically, `src/packstore/*_tests.rs`
- Create: `src/packstore/compression_tests.rs`

**Interfaces:**
- Consumes: `amberpack::Compression`, `amberpack::encode_record_with`, `amberpack::Error::is_invalid_compression`.
- Produces: `Options::compression(self, c: Compression) -> Options`; `Options::compression_for(self, f: impl Fn(&Key, &[u8], Compression) -> Compression + Send + Sync + 'static) -> Options`; `packstore::Error::is_invalid_compression(&self) -> bool`. `Options` is `Clone` and no longer `Copy`. Private: `Options::encode(&self, k: Key, data: &[u8]) -> Result<Vec<u8>, Error>`; `prepare(cfg: &Options, obj: Object, verify: bool)`. For tests: `testutil::ZSTD`, `testutil::zstd_opts()`.

- [ ] **Step 1: Keep the existing tests on zstd**

As in Go, the existing packstore tests keep running under the setting they were written for. Run these **before** adding the helpers of the next paragraph (the first command would otherwise rewrite the helper itself):

```bash
cd $RS/src/packstore
perl -pi -e 's/\bOptions::(?:new|default)\(\)/zstd_opts()/g' *_tests.rs testutil.rs
perl -pi -e 's/\bStore::open\(((?:[^()]|\([^()]*\))*)\)/Store::open_with($1, zstd_opts())/g' *_tests.rs testutil.rs
perl -pi -e 's/\bencode_record\(((?:[^()]|\([^()]*\))*)\)/encode_record_with($1, ZSTD)/g; s/\bencode_record\b/encode_record_with/ if /^use /' *_tests.rs testutil.rs
git diff --stat
```

Expected: changes in `testutil.rs` and in the `*_tests.rs` files that open stores or encode records; no file outside `src/packstore`.

Then add to `src/packstore/testutil.rs` (with `use crate::amberpack::Compression;` and `Options` added to its `use super::{…}` list):

```rust
/// What every store wrote with before compression became an option. The
/// tests written then still run under it, so they keep covering stores that
/// mix raw and compressed records.
pub(crate) const ZSTD: Compression = Compression::Zstd { level: 0 };

/// The default options plus [`ZSTD`].
pub(crate) fn zstd_opts() -> Options {
    Options::new().compression(ZSTD)
}
```

- [ ] **Step 2: Write the new tests**

Register the module in `src/packstore/mod.rs`, next to the other test modules:

```rust
#[cfg(test)]
mod compression_tests;
```

Create `src/packstore/compression_tests.rs`:

```rust
//! Compression options (Go: `packstore/compression_test.go` and
//! `packstore/mixed_codec_test.go`).

use std::collections::HashMap;
use std::fs;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use tempfile::TempDir;

use super::sidecar::SIDECAR_SUFFIX;
use super::store_tests::obj_seq;
use super::testutil::*;
use super::{CompactOpts, Object, Options, Store, WriteOpts};
use crate::amberpack::{Compression, REC_HEADER_SIZE, encode_record_with};
use crate::key::Key;

const LZ4: Compression = Compression::Lz4 { level: 0 };
const LZ4_9: Compression = Compression::Lz4 { level: 9 };
const ZSTD_19: Compression = Compression::Zstd { level: 19 };

const RAW: u8 = 0;
const CODEC_ZSTD: u8 = 1;
const CODEC_LZ4: u8 = 2;

fn codec_id(c: Compression) -> u8 {
    match c {
        Compression::None => RAW,
        Compression::Zstd { .. } => CODEC_ZSTD,
        Compression::Lz4 { .. } => CODEC_LZ4,
    }
}

/// The codec id of `k`'s stored record (Go: `codecOf`).
fn codec_of(s: &Store, k: Key) -> u8 {
    s.get_record(k).unwrap()[33]
}

/// Opens a store with `opts` and nothing else: no compression unless they
/// say so (Go: `rawStore`).
fn raw_store(opts: Options) -> (TempDir, Store) {
    let dir = TempDir::new().unwrap();
    let s = Store::open_with(dir.path(), opts.sync(false)).unwrap();
    (dir, s)
}

/// n compressible objects that differ from one another (Go: `distinct`).
fn distinct(n: usize) -> Vec<Object> {
    (0..n)
        .map(|i| {
            let mut data = compressible(4096);
            data.push(i as u8);
            data.push((i >> 8) as u8);
            blob_obj(&data)
        })
        .collect()
}

fn must_get(s: &Store, o: &Object) {
    assert_eq!(s.get(o.key).unwrap(), o.data, "get({})", o.key);
}

fn parallel(writers: usize) -> WriteOpts {
    WriteOpts {
        writers,
        ..WriteOpts::default()
    }
}

#[test]
fn default_store_writes_raw() {
    let (_dir, s) = raw_store(Options::new());
    let o = blob_obj(&compressible(4096));
    s.put(o.key, &o.data).unwrap();
    assert_eq!(codec_of(&s, o.key), RAW);
    assert_eq!(s.stored_size(o.key).unwrap(), Some(o.data.len() as u64));
    must_get(&s, &o);
}

#[test]
fn compression_on_every_write_path() {
    type Write = fn(&Store, &[Object]);
    let paths: [(&str, Write); 4] = [
        ("put", |s, objs| {
            for o in objs {
                s.put(o.key, &o.data).unwrap();
            }
        }),
        ("put_verified", |s, objs| {
            for o in objs {
                s.put_verified(o.key, &o.data).unwrap();
            }
        }),
        ("write_batch", |s, objs| {
            s.write_batch(obj_seq(objs, None)).unwrap();
        }),
        ("write_parallel", |s, objs| {
            s.write_parallel(obj_seq(objs, None), parallel(4)).1.unwrap();
        }),
    ];
    for c in [ZSTD, ZSTD_19, LZ4, LZ4_9] {
        for (name, write) in paths {
            let (_dir, s) = raw_store(Options::new().compression(c));
            let objs = distinct(6);
            write(&s, &objs);
            for o in &objs {
                assert_eq!(codec_of(&s, o.key), codec_id(c), "{c}/{name}");
                must_get(&s, o);
            }
        }
    }
}

#[test]
fn compression_for_receives_key_data_and_default() {
    let calls: Arc<Mutex<Vec<(Key, Vec<u8>, Compression)>>> = Arc::default();
    let seen = Arc::clone(&calls);
    let (_dir, s) = raw_store(Options::new().compression(LZ4).compression_for(
        move |k, data, def| {
            seen.lock().unwrap().push((*k, data.to_vec(), def));
            def
        },
    ));
    let o = blob_obj(&compressible(4096));
    s.put(o.key, &o.data).unwrap();
    assert_eq!(*calls.lock().unwrap(), vec![(o.key, o.data.clone(), LZ4)]);
    assert_eq!(codec_of(&s, o.key), CODEC_LZ4);
}

#[test]
fn compression_for_chooses_per_object() {
    // The store's setting is lz4. The callback stores "raw…" objects raw,
    // sends "zstd…" objects to zstd 19 and accepts the setting for the rest.
    let (_dir, s) = raw_store(Options::new().compression(LZ4).compression_for(
        |_, data, def| {
            if data.starts_with(b"raw") {
                Compression::None
            } else if data.starts_with(b"zstd") {
                ZSTD_19
            } else {
                def
            }
        },
    ));
    for (prefix, want) in [("raw", RAW), ("zstd", CODEC_ZSTD), ("other", CODEC_LZ4)] {
        let mut data = prefix.as_bytes().to_vec();
        data.extend(compressible(4096));
        let o = blob_obj(&data);
        s.put(o.key, &o.data).unwrap();
        assert_eq!(codec_of(&s, o.key), want, "{prefix} object");
        must_get(&s, &o);
    }
}

#[test]
fn compression_for_is_asked_when_the_setting_is_none() {
    let saw: Arc<Mutex<Option<Compression>>> = Arc::default();
    let seen = Arc::clone(&saw);
    let (_dir, s) = raw_store(Options::new().compression_for(move |_, _, def| {
        *seen.lock().unwrap() = Some(def);
        ZSTD
    }));
    let o = blob_obj(&compressible(4096));
    s.put(o.key, &o.data).unwrap();
    assert_eq!(*saw.lock().unwrap(), Some(Compression::None));
    assert_eq!(codec_of(&s, o.key), CODEC_ZSTD);
}

#[test]
fn compression_for_invalid_value_fails_the_write() {
    let bad = Arc::new(AtomicBool::new(true));
    let flag = Arc::clone(&bad);
    let (_dir, s) = raw_store(Options::new().compression_for(move |_, _, def| {
        if flag.load(Ordering::SeqCst) {
            Compression::Zstd { level: 99 }
        } else {
            def
        }
    }));
    let objs = distinct(8);
    let err = s.put(objs[0].key, &objs[0].data).unwrap_err();
    assert!(err.is_invalid_compression(), "put: {err}");
    assert!(
        err.to_string().contains(&objs[0].key.to_string()),
        "put error {err} does not name the key"
    );
    let err = s.put_verified(objs[0].key, &objs[0].data).unwrap_err();
    assert!(err.is_invalid_compression(), "put_verified: {err}");
    let err = s.write_batch(obj_seq(&objs, None)).unwrap_err();
    assert!(err.is_invalid_compression(), "write_batch: {err}");
    let err = s.write_parallel(obj_seq(&objs, None), parallel(4)).1.unwrap_err();
    assert!(err.is_invalid_compression(), "write_parallel: {err}");
    for o in &objs {
        assert!(!s.has(o.key).unwrap(), "{} stored by a rejected write", o.key);
    }
    // The failure is that object's, not the store's: it keeps working.
    bad.store(false, Ordering::SeqCst);
    s.write_parallel(obj_seq(&objs, None), parallel(4)).1.unwrap();
    for o in &objs {
        must_get(&s, o);
    }
}

#[test]
fn compression_for_is_not_asked_for_dedup_hits_or_records() {
    let calls = Arc::new(AtomicUsize::new(0));
    let n = Arc::clone(&calls);
    let (_dir, s) = raw_store(Options::new().compression(ZSTD).compression_for(
        move |_, _, def| {
            n.fetch_add(1, Ordering::SeqCst);
            def
        },
    ));
    let o = blob_obj(&compressible(4096));
    s.put(o.key, &o.data).unwrap();
    s.put(o.key, &o.data).unwrap();
    s.write_batch(obj_seq(&[o.clone(), o.clone()], None)).unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1, "one new object, three dedup hits");

    // A pre-encoded record is appended as it is and keeps its codec.
    let recs: Vec<Object> = (0..4u8)
        .map(|i| {
            let mut data = compressible(4096);
            data.extend([0xEE, i]);
            let p = blob_obj(&data);
            Object {
                key: p.key,
                data: Vec::new(),
                record: Some(encode_record_with(p.key, &p.data, LZ4).unwrap()),
            }
        })
        .collect();
    s.write_batch(obj_seq(&recs[..2], None)).unwrap();
    s.write_parallel(obj_seq(&recs[2..], None), parallel(2)).1.unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1, "pre-encoded records must not reach the callback");
    for r in &recs {
        assert_eq!(codec_of(&s, r.key), CODEC_LZ4);
    }
}

#[test]
fn put_dedups_across_codecs() {
    let dir = TempDir::new().unwrap();
    let o = blob_obj(&compressible(4096));
    let a = Store::open_with(dir.path(), Options::new().sync(false).compression(LZ4)).unwrap();
    a.put(o.key, &o.data).unwrap();
    a.close().unwrap();

    let calls = Arc::new(AtomicUsize::new(0));
    let n = Arc::clone(&calls);
    let b = Store::open_with(
        dir.path(),
        Options::new().sync(false).compression(ZSTD).compression_for(move |_, _, def| {
            n.fetch_add(1, Ordering::SeqCst);
            def
        }),
    )
    .unwrap();
    b.put(o.key, &o.data).unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 0, "the store already holds the object");
    assert_eq!(codec_of(&b, o.key), CODEC_LZ4, "the record written first stays");
}

#[test]
fn compression_for_under_parallel_writers() {
    let calls = Arc::new(AtomicUsize::new(0));
    let n = Arc::clone(&calls);
    let (_dir, s) = raw_store(Options::new().compression(LZ4).compression_for(
        move |_, data, def| {
            n.fetch_add(1, Ordering::SeqCst);
            // distinct's low index byte
            if data[data.len() - 2] % 2 == 0 { ZSTD } else { def }
        },
    ));
    let objs = distinct(200);
    let (stats, res) = s.write_parallel(obj_seq(&objs, None), parallel(8));
    res.unwrap();
    assert_eq!(stats.stored, objs.len());
    assert_eq!(calls.load(Ordering::SeqCst), objs.len());
    for (i, o) in objs.iter().enumerate() {
        let want = if i % 2 == 0 { CODEC_ZSTD } else { CODEC_LZ4 };
        assert_eq!(codec_of(&s, o.key), want, "object {i}");
    }
}

#[test]
fn open_rejects_invalid_compression() {
    for c in [
        Compression::Zstd { level: 23 },
        Compression::Zstd { level: -1 },
        Compression::Lz4 { level: 13 },
    ] {
        let tmp = TempDir::new().unwrap();
        let dir = tmp.path().join("store");
        let err = Store::open_with(&dir, Options::new().compression(c))
            .err()
            .expect("open must fail");
        assert!(err.is_invalid_compression(), "{c:?}: {err}");
        assert!(!dir.exists(), "{c:?}: open created the directory before rejecting the option");
    }
}

#[test]
fn the_last_compression_wins() {
    let (_dir, s) = raw_store(Options::new().compression(ZSTD).compression(LZ4));
    let o = blob_obj(&compressible(4096));
    s.put(o.key, &o.data).unwrap();
    assert_eq!(codec_of(&s, o.key), CODEC_LZ4);
}

#[test]
fn tiny_objects_round_trip() {
    let sizes = [0usize, 1, 2, 12, 13, 64];
    for c in [ZSTD, LZ4, LZ4_9] {
        let seen: Arc<Mutex<Vec<usize>>> = Arc::default();
        let log = Arc::clone(&seen);
        let (_dir, s) = raw_store(Options::new().compression(c).compression_for(
            move |_, data, def| {
                log.lock().unwrap().push(data.len());
                def
            },
        ));
        for n in sizes {
            let o = blob_obj(&vec![b'a'; n]);
            s.put(o.key, &o.data).unwrap();
            must_get(&s, &o);
        }
        assert_eq!(*seen.lock().unwrap(), sizes, "{c}");
        // verify re-parses every record: the length invariants hold.
        s.verify(|| false).unwrap();
    }
}

#[test]
fn repair_replacement_uses_the_handles_compression() {
    let objs = test_objects(4); // even indexes compress
    let (dir, path, entries) = write_sealed_file(&objs); // zstd records
    let mut bytes = fs::read(&path).unwrap();
    bytes[entries[0].off as usize + REC_HEADER_SIZE] ^= 0x40;
    fs::write(&path, bytes).unwrap();

    let s = Store::open_with(dir.path(), Options::new().compression(LZ4)).unwrap();
    assert!(s.verify(|| false).is_err());
    s.put_verified(objs[0].key, &objs[0].data).unwrap();
    assert_eq!(codec_of(&s, objs[0].key), CODEC_LZ4, "the replacement uses the handle's setting");
    assert_eq!(codec_of(&s, objs[2].key), CODEC_ZSTD, "its neighbours keep their codec");
    must_get(&s, &objs[0]);
    s.verify(|| false).unwrap();
}

/// Fills one store through five handles, each with its own compression, and
/// reads it through a handle with the default: after a plain reopen, after
/// losing every sidecar, and after a compaction (Go: `TestMixedCodecStore`).
#[test]
fn mixed_codec_store() {
    let dir = TempDir::new().unwrap();
    let small = || Options::new().segment_size(8 << 10).sync(false);
    let mut objs = Vec::new();
    for (si, c) in [Compression::None, ZSTD, LZ4, ZSTD_19, LZ4_9].into_iter().enumerate() {
        let s = Store::open_with(dir.path(), small().compression(c)).unwrap();
        for i in 0..12u8 {
            let mut data = if i % 3 == 0 {
                incompressible(3000)
            } else {
                compressible(3000)
            };
            data.extend([si as u8, i]);
            let o = blob_obj(&data);
            s.put(o.key, &o.data).unwrap();
            objs.push(o);
        }
        s.close().unwrap();
    }
    let read_all = |s: &Store| -> HashMap<Key, u8> {
        objs.iter()
            .map(|o| {
                must_get(s, o);
                (o.key, codec_of(s, o.key))
            })
            .collect()
    };

    let s = Store::open_with(dir.path(), small()).unwrap();
    let before = read_all(&s);
    for codec in [RAW, CODEC_ZSTD, CODEC_LZ4] {
        assert!(before.values().any(|&c| c == codec), "the store holds no record of codec {codec}");
    }
    s.verify(|| false).unwrap();
    s.close().unwrap();

    // Without their sidecars the active segments are indexed by scanning them.
    let sidecars: Vec<_> = fs::read_dir(dir.path())
        .unwrap()
        .map(|e| e.unwrap().path())
        .filter(|p| p.to_string_lossy().ends_with(SIDECAR_SUFFIX))
        .collect();
    assert!(!sidecars.is_empty(), "want at least one sidecar");
    for p in sidecars {
        fs::remove_file(p).unwrap();
    }
    let s = Store::open_with(dir.path(), small()).unwrap();
    assert_eq!(read_all(&s), before, "codecs changed across a reopen without sidecars");
    s.verify(|| false).unwrap();

    // Compaction copies the live records as they are.
    let live = |k: Key| k.as_bytes()[0] % 2 == 0;
    s.compact(live, CompactOpts::default()).unwrap();
    for o in objs.iter().filter(|o| live(o.key)) {
        must_get(&s, o);
        assert_eq!(codec_of(&s, o.key), before[&o.key], "compaction changed {}", o.key);
    }
    s.verify(|| false).unwrap();
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd $RS && nix develop -c cargo test --lib packstore`
Expected: compile errors, `no method named compression found for struct Options` among them.

- [ ] **Step 4: Implement**

In `src/packstore/mod.rs`:

Change the amberpack import from `use crate::amberpack::{self, REC_HEADER_SIZE, decode_payload, encode_record};` to:

```rust
use crate::amberpack::{self, Compression, REC_HEADER_SIZE, decode_payload, encode_record_with};
```

Make sure `std::fmt` and `std::sync::Arc` are imported. Replace the `Options` struct, its `Default` impl, and add the methods:

```rust
/// Chooses the compression for one object (Go: `CompressionFunc`).
type CompressionFor = Arc<dyn Fn(&Key, &[u8], Compression) -> Compression + Send + Sync>;

/// Store configuration (Go: the `WithSegmentSize` / `WithSync` /
/// `WithCompression` / `WithCompressionFor` options).
#[derive(Clone)]
pub struct Options {
    segment_size: u64,
    sync: bool,
    compression: Compression,
    compression_for: Option<CompressionFor>,
}

impl fmt::Debug for Options {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Options")
            .field("segment_size", &self.segment_size)
            .field("sync", &self.sync)
            .field("compression", &self.compression)
            .field("compression_for", &self.compression_for.as_ref().map(|_| "<fn>"))
            .finish()
    }
}

impl Default for Options {
    fn default() -> Options {
        Options {
            segment_size: DEFAULT_SEGMENT_SIZE,
            sync: true,
            compression: Compression::None,
            compression_for: None,
        }
    }
}
```

Add to `impl Options`, after `sync`:

```rust
    /// Sets the compression for the objects this store encodes: those given
    /// to `put`, `put_verified` and `put_verified_deferred`, and those a
    /// batch carries as data. The default is no compression. The setting
    /// belongs to this handle and is stored nowhere; reading never depends
    /// on it, and records that arrive already encoded keep the codec they
    /// have (Go: `WithCompression`).
    pub fn compression(mut self, c: Compression) -> Options {
        self.compression = c;
        self
    }

    /// Makes the store ask `f` for every object it encodes, whatever the
    /// [`Options::compression`] value is. `f` gets the key, the bytes and
    /// that value: returning it accepts the store's setting, returning
    /// [`Compression::None`] stores the object raw, and anything else
    /// overrides the setting for this object. `f` is not asked for an object
    /// the store already holds, nor for a pre-encoded record.
    ///
    /// `f` runs on whichever thread is writing, several at once under
    /// `write_parallel`. It must not call the store: a repair calls it with
    /// the store's append lock held. A returned value that does not validate
    /// fails that object's write with an invalid-compression error (Go:
    /// `WithCompressionFor`).
    pub fn compression_for(
        mut self,
        f: impl Fn(&Key, &[u8], Compression) -> Compression + Send + Sync + 'static,
    ) -> Options {
        self.compression_for = Some(Arc::new(f));
        self
    }

    /// Returns the record to append for `(k, data)`, compressed as these
    /// options say (Go: `Store.encode`).
    fn encode(&self, k: Key, data: &[u8]) -> Result<Vec<u8>, Error> {
        let mut c = self.compression;
        if let Some(f) = &self.compression_for {
            c = f(&k, data, c);
        }
        encode_record_with(k, data, c).map_err(|e| {
            if e.is_invalid_compression() {
                Error::Context {
                    msg: format!("packstore: object {k}"),
                    source: Box::new(Error::Pack(e)),
                }
            } else {
                Error::Pack(e)
            }
        })
    }
```

Add to `impl Error`, next to `is_corrupt`:

```rust
    /// Go's `errors.Is(err, amberpack.ErrInvalidCompression)`.
    pub fn is_invalid_compression(&self) -> bool {
        match self {
            Error::Pack(e) => e.is_invalid_compression(),
            Error::Context { source, .. } => source.is_invalid_compression(),
            _ => false,
        }
    }
```

In `open_with`, validate before anything is created:

```rust
    pub fn open_with(dir: impl AsRef<Path>, cfg: Options) -> Result<Store, Error> {
        cfg.compression.validate().map_err(|e| Error::Context {
            msg: "packstore".into(),
            source: Box::new(Error::Pack(e)),
        })?;
        let dir = dir.as_ref().to_path_buf();
```

In `put`, replace `let rec = encode_record(k, data).map_err(Error::Pack)?;` with:

```rust
        let rec = self.cfg.encode(k, data)?;
```

In `write_batch`, replace `prepare(obj, false)` with `prepare(&self.cfg, obj, false)`. In `src/packstore/parallel.rs`, replace `prepare(obj, verify)` with `prepare(&self.cfg, obj, verify)`.

In `src/packstore/prepare.rs`, drop `encode_record` from the amberpack import, add `Options` to `use super::{…}`, and change `prepare`:

```rust
pub(super) fn prepare(cfg: &Options, obj: Object, verify: bool) -> Result<(Vec<u8>, u64), Error> {
    let Some(record) = obj.record else {
        if verify {
            verify_object(obj.key, &obj.data).map_err(Error::Verify)?;
        }
        let rec = cfg.encode(obj.key, &obj.data)?;
        return Ok((rec, obj.data.len() as u64));
    };
```

Its doc comment says the record is "[`encode_record`]'s output"; change that to "the store's encoding ([`Options::encode`])".

In `src/packstore/repair.rs`, drop `encode_record` from the import and replace both encode calls:

```rust
            let record = self.cfg.encode(key, data)?;
```

```rust
        let replacement = self.cfg.encode(key, data)?;
```

Doc comments in `mod.rs` that link to `` [`encode_record`] `` (the `Object` type and `get_record`) now name an item that is no longer imported: change them to `` [`amberpack::encode_record_with`] ``.

- [ ] **Step 5: Run the tests**

Run: `cd $RS && nix develop -c cargo test --lib packstore`
Expected: `test result: ok`.

What can need a hand after the mechanical edit of Step 1:

- A test file that imports names from `testutil` one by one rather than with `*` needs `zstd_opts` and `ZSTD` added to that import.
- `Options` is no longer `Copy`: where a test passes one `Options` value twice, the second use needs `.clone()`.
- A test that still fails on a size, a flag or a rotation count is one the commands did not reach. Give its store `zstd_opts()` and its record `encode_record_with(…, ZSTD)`; leave its assertions alone.
- If `write_parallel` or `write_batch` wraps the encode error in a variant `is_invalid_compression` does not look through, extend `is_invalid_compression` to look through it, as `is_corrupt` does.

- [ ] **Step 6: Run everything**

Run: `cd $RS && nix develop -c cargo fmt && nix develop -c cargo clippy --locked --all-targets -- -D warnings && nix develop -c cargo test --locked`
Expected: clippy clean (remove any `Options` import the edit left unused) and all tests pass. `tests/golden_amberpack.rs::golden_records_compressed` asserts that a Rust re-encode sets the zstd flag and fails now; Task 14 fixes it, so note it and go on. Any other failure outside `src/packstore` is a test that relied on compression by default: pass `.compression(Compression::Zstd { level: 0 })` where it opens its store.

- [ ] **Step 7: Commit**

```bash
cd $RS && git add src
git commit -m "packstore: Options::compression and compression_for; no compression by default"
```

---

### Task 13: `--compression` on the Rust CLI

**Files:**
- Modify: `examples/amber-store.rs` (the `Cli` struct near line 41, `open_store` near line 297)
- Modify: `tests/cli_e2e.rs`

**Interfaces:**
- Consumes: `amberpack::Compression` (`FromStr`), `Options::compression`.
- Produces: the global flag `--compression none|zstd[:LEVEL]|lz4[:LEVEL]`, default `none`, with the syntax of the Go CLI (Task 6). Task 15's `interop/check.sh` uses it.

- [ ] **Step 1: Write the failing tests**

Append to `tests/cli_e2e.rs`:

```rust
/// The total size of the regular files under `dir` (Go: `dirSize`).
fn dir_size(dir: &Path) -> u64 {
    fs::read_dir(dir)
        .unwrap()
        .map(|e| e.unwrap())
        .map(|e| {
            let meta = e.metadata().unwrap();
            if meta.is_dir() { dir_size(&e.path()) } else { meta.len() }
        })
        .sum()
}

// Port of Go TestE2E_Compression.
#[test]
fn compression_end_to_end() {
    let src = TempDir::new().unwrap();
    write_fixture(src.path());
    // Numbered lines: they compress well, and unlike one repeated line they
    // do not chunk into identical pieces that dedup away. About 140 KiB.
    let text: String = (0..4000)
        .map(|i| format!("line {i:06} of the compression test\n"))
        .collect();
    fs::write(src.path().join("big.txt"), &text).unwrap();
    let src_s = src.path().display().to_string();

    let mut sizes = std::collections::HashMap::new();
    for comp in ["none", "zstd:19", "lz4:9"] {
        let store = TempDir::new().unwrap();
        let store_s = store.path().display().to_string();
        run_app(&[
            "--store", &store_s, "--compression", comp, "ingest", "--no-progress", "--ref", "v1",
            &src_s,
        ])
        .unwrap_or_else(|e| panic!("{comp}: ingest: {e}"));
        // Read back without the flag: reading never depends on the setting.
        let dest = TempDir::new().unwrap();
        let dest_s = dest.path().display().to_string();
        run_app(&["--store", &store_s, "restore", "ref:v1", &dest_s])
            .unwrap_or_else(|e| panic!("{comp}: restore: {e}"));
        assert_eq!(fs::read_to_string(dest.path().join("big.txt")).unwrap(), text, "{comp}");
        sizes.insert(comp, dir_size(&store.path().join("packstore")));
    }
    assert!(
        sizes["none"] >= text.len() as u64,
        "the default store is {} bytes, smaller than the {}-byte file",
        sizes["none"],
        text.len()
    );
    for comp in ["zstd:19", "lz4:9"] {
        assert!(
            sizes[comp] < sizes["none"] / 2,
            "--compression {comp}: packstore is {} bytes, the uncompressed one {}",
            sizes[comp],
            sizes["none"]
        );
    }
}

// Port of Go TestE2E_CompressionRejectsBadValues.
#[test]
fn compression_rejects_bad_values() {
    let src = TempDir::new().unwrap();
    write_fixture(src.path());
    let src_s = src.path().display().to_string();
    let store = TempDir::new().unwrap();
    let store_s = store.path().display().to_string();
    for bad in ["gzip", "zstd:23", "zstd:", "lz4:x", "ZSTD", ""] {
        let err = run_app(&["--store", &store_s, "--compression", bad, "ingest", "--no-progress", &src_s])
            .expect_err("a bad --compression must fail");
        assert!(err.contains("invalid compression"), "--compression {bad:?}: {err}");
    }
    assert_eq!(fs::read_dir(store.path()).unwrap().count(), 0, "a rejected flag created files");
}

// Port of Go TestE2E_GCOverMixedCodecs.
#[test]
fn gc_over_mixed_codecs() {
    let src = TempDir::new().unwrap();
    write_fixture(src.path());
    let keep = "kept across both ingests\n".repeat(3000);
    fs::write(src.path().join("keep.txt"), &keep).unwrap();
    fs::write(src.path().join("big.txt"), "first version\n".repeat(3000)).unwrap();
    let src_s = src.path().display().to_string();
    let store = TempDir::new().unwrap();

    run_seg(store.path(), &["--compression", "lz4", "ingest", "--no-progress", "--ref", "v1", &src_s])
        .unwrap_or_else(|e| panic!("first ingest: {e}"));
    let second = "second version\n".repeat(3000);
    fs::write(src.path().join("big.txt"), &second).unwrap();
    run_seg(store.path(), &["--compression", "zstd:19", "ingest", "--no-progress", "--ref", "v1", &src_s])
        .unwrap_or_else(|e| panic!("second ingest: {e}"));
    std::thread::sleep(std::time::Duration::from_millis(50));
    run_seg(store.path(), &["gc", "run", "--grace", "1ms", "--garbage", "0"])
        .unwrap_or_else(|e| panic!("gc run: {e}"));
    let dest = TempDir::new().unwrap();
    let dest_s = dest.path().display().to_string();
    run_seg(store.path(), &["restore", "ref:v1", &dest_s])
        .unwrap_or_else(|e| panic!("restore after gc: {e}"));
    assert_eq!(fs::read_to_string(dest.path().join("keep.txt")).unwrap(), keep);
    assert_eq!(fs::read_to_string(dest.path().join("big.txt")).unwrap(), second);
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd $RS && nix develop -c cargo test --test cli_e2e compression`
Expected: FAIL; the CLI rejects the flag with `unexpected argument '--compression'`.

- [ ] **Step 3: Implement the flag**

In `examples/amber-store.rs`, add the field to `Cli`, after `segment_size`:

```rust
    /// compression for the objects this command writes: none, zstd[:LEVEL]
    /// or lz4[:LEVEL]
    #[arg(long, global = true, default_value = "none")]
    compression: Compression,
```

Import the type (`use amber_store_core::amberpack::Compression;`, alongside the example's other `amber_store_core` imports), and pass it in `open_store`:

```rust
        packstore::Options::new()
            .sync(true)
            .segment_size(cli.segment_size)
            .compression(cli.compression),
```

clap parses the value through `Compression`'s `FromStr`, so a bad value is a usage error before any store is opened.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd $RS && nix develop -c cargo test --test cli_e2e`
Expected: `test result: ok`.

- [ ] **Step 5: Commit**

```bash
cd $RS && git add examples/amber-store.rs tests/cli_e2e.rs
git commit -m "amber-store example: --compression none|zstd[:LEVEL]|lz4[:LEVEL]"
```

---

### Task 14: Golden vectors

**Files:**
- Modify: `tools/vectorgen/go.mod`, `tools/vectorgen/go.sum`
- Modify: `tools/vectorgen/amberpack.go`, `tools/vectorgen/segments.go`
- Create: `tests/golden/amberpack/records_lz4.json` (generated)
- Modify: `tests/golden_amberpack.rs`
- Modify: `VECTORS.md`, `port-notes/vectorgen.md`

**Interfaces:**
- Consumes: the pushed Go commit `$GO_SHA` (Task 8) with `amberpack.EncodeRecordWith`, `amberpack.WithCompression`, `packstore.WithCompression`.
- Produces: `tests/golden/amberpack/records_lz4.json`, shaped `{ "cases": [ { "record_hex", "key", "payload", "level" } ] }`.

- [ ] **Step 1: Point vectorgen at the Go branch**

```bash
cd $RS/tools/vectorgen && nix develop -c go get github.com/amber-store/core@$GO_SHA && nix develop -c go mod tidy
```

Expected: `go.mod` requires `github.com/amber-store/core` at a pseudo-version ending in the first 12 characters of `$GO_SHA`, and `go.sum` gains `github.com/pierrec/lz4/v4`.

- [ ] **Step 2: Ask for zstd where vectorgen relied on the default**

In `tools/vectorgen/amberpack.go`, add near the top of the file:

```go
// zstdDefault is what the Go encoder did before compression became a choice;
// the vectors generated then are generated with it still.
var zstdDefault = amberpack.Compression{Algorithm: amberpack.Zstd}
```

In the `records_compressed.json` loop, replace `amberpack.EncodeRecord(k, data)` with `amberpack.EncodeRecordWith(k, data, zstdDefault)`, and its check `if recFlags(rec)&0x01 == 0 {` with `if recFlags(rec) != 0x01 {`.

For `pack_go.bin`, replace `w := amberpack.NewWriter(packF)` with `w := amberpack.NewWriter(packF, amberpack.WithCompression(zstdDefault))`.

In `tools/vectorgen/segments.go`, add the option to the `packstore.Open` call:

```go
	st, err := packstore.Open(dir, packstore.WithSegmentSize(segmentSize), packstore.WithSync(false),
		packstore.WithCompression(amberpack.Compression{Algorithm: amberpack.Zstd}))
```

(import `github.com/amber-store/core/amberpack` there if the file does not).

The `records_raw.json` loop keeps `amberpack.EncodeRecord`: its payloads never compress, and raw is now what that function writes.

- [ ] **Step 3: Generate the lz4 vectors**

In `tools/vectorgen/amberpack.go`, add the types next to `compressedRecordCase`:

```go
type lz4RecordCase struct {
	RecordHex string  `json:"record_hex"`
	Key       string  `json:"key"`
	Payload   Payload `json:"payload"`
	Level     int     `json:"level"`
}

type lz4RecordsFile struct {
	Cases []lz4RecordCase `json:"cases"`
}
```

and, right after `records_compressed.json` is written, add:

```go
	// --- records_lz4.json: the same compressible payloads as lz4 records, at
	// the fast level and at an HC level; decode-only vectors for Rust.
	lz4File := lz4RecordsFile{}
	for _, level := range []int{0, 9} {
		for _, p := range compCases {
			data := p.Materialize()
			k, err := key.New(key.Blob, uint64(len(data)), data)
			if err != nil {
				return err
			}
			rec, err := amberpack.EncodeRecordWith(k, data, amberpack.Compression{Algorithm: amberpack.LZ4, Level: level})
			if err != nil {
				return err
			}
			if recFlags(rec) != 0x02 {
				return fmt.Errorf("records_lz4: payload %v at level %d did not compress", p, level)
			}
			lz4File.Cases = append(lz4File.Cases, lz4RecordCase{
				RecordHex: hex.EncodeToString(rec),
				Key:       k.String(),
				Payload:   p,
				Level:     level,
			})
		}
	}
	if err := writeJSON(filepath.Join(dir, "records_lz4.json"), lz4File); err != nil {
		return err
	}
```

Update the comment above `genAmberpack` to list `records_lz4.json`.

- [ ] **Step 4: Regenerate into a scratch directory and compare**

```bash
cd $RS/tools/vectorgen && rm -rf /tmp/vectors-check && nix develop -c go run . /tmp/vectors-check
diff -r /tmp/vectors-check $RS/tests/golden
```

Expected: exactly one line of output, `Only in /tmp/vectors-check/amberpack: records_lz4.json`. Every existing file, `records_compressed.json`, `pack_go.bin` and `segments_go` included, is byte-identical: that is the proof that asking for zstd at level 0 reproduces what v0.9.0 wrote.

If an existing file differs, stop. Either a writer in vectorgen still uses the new default (find it with `grep -n -E "packstore\.Open|NewWriter|EncodeRecord\(" *.go`), or the Go side does not reproduce the v0.9.0 bytes, which is a bug in Part 1.

Then: `cp /tmp/vectors-check/amberpack/records_lz4.json $RS/tests/golden/amberpack/ && rm -rf /tmp/vectors-check`

- [ ] **Step 5: Write the Rust tests over the vectors**

In `tests/golden_amberpack.rs`, add `Compression` and `encode_record_with` to the file's `amber_store_core::amberpack` import. In `golden_records_compressed`, the re-encode must ask for zstd now; replace

```rust
        let enc = encode_record(k, &payload).unwrap_or_else(|e| panic!("case {i}: encode: {e}"));
```

with

```rust
        let enc = encode_record_with(k, &payload, Compression::Zstd { level: 0 })
            .unwrap_or_else(|e| panic!("case {i}: encode: {e}"));
```

Add the types and the new test:

```rust
#[derive(serde::Deserialize)]
struct Lz4File {
    cases: Vec<Lz4Case>,
}

#[derive(serde::Deserialize)]
struct Lz4Case {
    record_hex: String,
    key: String,
    payload: common::Payload,
    level: i32,
}

/// Go-encoded lz4 records, at the fast level and at an HC level: parse +
/// decode recovers the payload exactly. A Rust re-encode at the same level is
/// NOT byte-compared (liblz4 and Go's pierrec/lz4 produce different blocks),
/// but it must carry codec 2 and round-trip.
#[test]
fn golden_records_lz4() {
    let Some(bytes) = common::load("amberpack/records_lz4.json") else {
        return;
    };
    let file: Lz4File = serde_json::from_slice(&bytes).expect("records_lz4.json parses");
    assert!(!file.cases.is_empty(), "records_lz4.json has no cases");
    let levels: std::collections::BTreeSet<i32> = file.cases.iter().map(|c| c.level).collect();
    assert!(levels.contains(&0) && levels.len() > 1, "want the fast level and an HC level");

    for (i, c) in file.cases.iter().enumerate() {
        let k = parse_key(&c.key, &format!("case {i}"));
        let payload = c.payload.bytes();
        let rec = hex::decode(&c.record_hex).unwrap_or_else(|e| panic!("case {i}: hex: {e}"));

        let r = parse_record(&rec).unwrap_or_else(|e| panic!("case {i}: parse Go record: {e}"));
        assert_eq!(r.key, k, "case {i}: key");
        assert_eq!(r.flags, 2, "case {i}: Go record must carry the lz4 codec");
        assert_eq!(r.ulen as usize, payload.len(), "case {i}: ulen");
        assert!(r.slen < r.ulen, "case {i}: compressed slen < ulen");
        assert_eq!(rec.len(), REC_HEADER_SIZE + r.slen as usize, "case {i}: record length");
        let got = decode_payload(r.flags, r.ulen, &rec[REC_HEADER_SIZE..])
            .unwrap_or_else(|e| panic!("case {i}: decode Go payload: {e}"));
        assert_eq!(got, payload, "case {i}: Go payload round-trip");

        let enc = encode_record_with(k, &payload, Compression::Lz4 { level: c.level })
            .unwrap_or_else(|e| panic!("case {i}: encode: {e}"));
        let r2 = parse_record(&enc).unwrap_or_else(|e| panic!("case {i}: parse own record: {e}"));
        assert_eq!(r2.flags, 2, "case {i}: own flags");
        let got2 = decode_payload(r2.flags, r2.ulen, &enc[REC_HEADER_SIZE..])
            .unwrap_or_else(|e| panic!("case {i}: decode own payload: {e}"));
        assert_eq!(got2, payload, "case {i}: own payload round-trip");
    }
}
```

- [ ] **Step 6: Run the golden tests**

Run: `cd $RS && nix develop -c cargo test --locked --test golden_amberpack --test golden_packstore`
Expected: `test result: ok` for both, `golden_records_lz4` and `golden_records_compressed` among them.

To see `golden_records_lz4` fail for the right reason, change `assert_eq!(r.flags, 2, …)` to `1` once, run, see `Go record must carry the lz4 codec`, and change it back.

- [ ] **Step 7: Document the vectors**

In `VECTORS.md`, after the `records_compressed.json` entry and its JSON block, add:

````markdown
- `records_lz4.json`: Go-encoded lz4 records of the same compressible
  payloads, at level 0 (the fast compressor) and at level 9 (high
  compression) — decode-only vectors, like the zstd ones (a Rust encoder
  produces different blocks):

```json
{ "cases": [ { "record_hex": "...", "key": "<64 hex>", "payload": {..}, "level": 0 } ] }
```
````

In the same file, where `records_compressed.json` is described, change "Go-encoded records with compressible payloads" to "Go-encoded zstd records (level 0) of compressible payloads".

Append to `port-notes/vectorgen.md`:

```markdown
## Compression options

Go made compression a choice and its default none. `vectorgen` now asks for
zstd at level 0 wherever it relied on the old default: the
`records_compressed.json` loop, the writer of `pack_go.bin`, and the store
behind `segments_go`. A full regeneration at the Go branch's head, through the
pseudo-version in `go.mod`, reproduced every existing file byte for byte and
added `amberpack/records_lz4.json`.
```

- [ ] **Step 8: Commit**

```bash
cd $RS && git add tools/vectorgen tests/golden/amberpack/records_lz4.json tests/golden_amberpack.rs VECTORS.md port-notes/vectorgen.md
git commit -m "vectors: lz4 records from Go; zstd asked for where it was the default"
```

---

### Task 15: The cross-language check

**Files:**
- Modify: `interop/check.sh`
- Modify: `.github/workflows/ci.yml` (line 39, the Go checkout's `ref`)

**Interfaces:**
- Consumes: `--compression` on both CLIs (Tasks 6 and 13); the Go checkout at `$GO`.
- Produces: check number 6 of `interop/check.sh`.

- [ ] **Step 1: Give the test tree something that compresses**

In `interop/check.sh`, in the `== creating test tree` block, after the line that writes `big.bin`, add:

```bash
seq 1 150000 > "$T/compressible.txt"   # ~1 MiB of numbers: it compresses and does not dedup
```

Numbers rather than one repeated byte or line: repeated content chunks into identical pieces, which dedup away before compression has anything to do.

- [ ] **Step 2: Add the check**

Add to the numbered list in the header comment:

```bash
#   6. compression: each implementation ingests the tree with zstd and with
#      lz4 into a store of its own, the OTHER lists and exports it byte for
#      byte, and each compressed store is smaller than the default one.
```

Insert the section right after the `== cross-exporting (tar byte-compare, 4 combinations)` block and before the `== restore` block. It must run there, while `store-go` and `store-rs` hold only the first tree:

```bash
echo "== compression (each side reads what the OTHER wrote with zstd and with lz4)"
store_bytes() { find "$1" -type f -exec cat {} + | wc -c | tr -d ' '; }
PLAIN_GO=$(store_bytes "$WORK/store-go/packstore")
PLAIN_RS=$(store_bytes "$WORK/store-rs/packstore")
for comp in zstd:19 lz4:9; do
  tag=${comp%%:*}
  [ "$("$GO" --store "$WORK/store-go-$tag" --compression "$comp" ingest "$T")" = "$ROOT_GO" ] \
    || { echo "FAIL: go root key differs with --compression $comp" >&2; exit 1; }
  [ "$("$RS" --store "$WORK/store-rs-$tag" --compression "$comp" ingest "$T")" = "$ROOT_GO" ] \
    || { echo "FAIL: rust root key differs with --compression $comp" >&2; exit 1; }
  "$RS" --store "$WORK/store-go-$tag" ls --keys "$ROOT_GO" > "$WORK/ls-rs-from-go-$tag.txt"
  "$GO" --store "$WORK/store-rs-$tag" ls --keys "$ROOT_GO" > "$WORK/ls-go-from-rs-$tag.txt"
  cmp "$WORK/ls-go-own.txt" "$WORK/ls-rs-from-go-$tag.txt"
  cmp "$WORK/ls-go-own.txt" "$WORK/ls-go-from-rs-$tag.txt"
  "$RS" --store "$WORK/store-go-$tag" export -o "$WORK/rs-from-go-$tag.tar" "$ROOT_GO"
  "$GO" --store "$WORK/store-rs-$tag" export -o "$WORK/go-from-rs-$tag.tar" "$ROOT_GO"
  cmp "$WORK/go-from-go.tar" "$WORK/rs-from-go-$tag.tar"
  cmp "$WORK/go-from-go.tar" "$WORK/go-from-rs-$tag.tar"
  # A smaller store is the evidence that compressed records were written.
  [ "$(store_bytes "$WORK/store-go-$tag/packstore")" -lt "$PLAIN_GO" ] \
    || { echo "FAIL: go store is no smaller with --compression $comp" >&2; exit 1; }
  [ "$(store_bytes "$WORK/store-rs-$tag/packstore")" -lt "$PLAIN_RS" ] \
    || { echo "FAIL: rust store is no smaller with --compression $comp" >&2; exit 1; }
done
```

- [ ] **Step 3: Run it against the Go branch**

Run: `cd $RS && AMBER_GO_REPO=$GO nix develop -c bash interop/check.sh`
Expected: every section prints and the script exits 0, `== compression …` among them.

To see the new section fail for the right reason, temporarily change `--compression "$comp"` in the Go ingest line to `--compression none` and run again. Expected: `FAIL: go store is no smaller with --compression zstd:19`. Restore the line.

The script builds `target/debug/examples/amber-store`; leave `target/` for now (Task 17 removes it).

- [ ] **Step 4: Point CI's interop job at the Go branch**

In `.github/workflows/ci.yml`, change the Go checkout's `ref` (line 39) from the v0.9.0 commit to the branch head, with the pull request number from Task 8:

```yaml
          ref: <$GO_SHA> # core compression-options (amber-store/core#<N>)
```

The pin moves to the release tag when both sides are released; that is the release procedure's step, not this plan's.

- [ ] **Step 5: Commit**

```bash
cd $RS && git add interop/check.sh .github/workflows/ci.yml
git commit -m "interop: each side reads what the other wrote with zstd and with lz4"
```

---

### Task 16: Rust documentation

**Files:**
- Modify: `architecture/amberpack.md` (copied from Go)
- Modify: `README.md`, `PORTING.md`, `port-notes/amberpack.md`

**Interfaces:**
- Consumes: `$GO/architecture/amberpack.md` from Task 7.
- Produces: nothing code depends on.

- [ ] **Step 1: Copy the architecture document**

```bash
cp $GO/architecture/amberpack.md $RS/architecture/amberpack.md
diff -r $GO/architecture $RS/architecture
```

Expected: no output from `diff`. If another file under `architecture/` differs, it differed before this change; leave it and mention it in the pull request.

- [ ] **Step 2: `README.md`**

Replace the "Interoperable, not byte-identical" bullet with:

```markdown
- **Interoperable, not byte-identical**: compressed record payloads. Go uses
  klauspost's zstd and pierrec's lz4, this crate libzstd and liblz4, and the
  two map compression levels differently; each decodes what the other wrote,
  but pack and segment files that contain compressed records differ byte-wise.
  Content addressing is unaffected, because keys hash the uncompressed bytes.
```

After the "A minimal embedding:" code block, add:

````markdown
Objects are stored uncompressed unless the store is opened with a compression
option:

```rust
use amber_store_core::amberpack::Compression;

let store = packstore::Store::open_with(
    dir.join("packstore"),
    packstore::Options::new().compression(Compression::Zstd { level: 0 }),
)?;
```

`Options::compression` takes none, zstd (levels 1–22) or lz4 (0 for the fast
compressor, 1–12 for high compression); level 0 is each algorithm's default.
`Options::compression_for` adds a function that chooses per object, from its
key and bytes. Every store reads records of every codec, whatever it was
opened with. Releases before this one read raw and zstd records but not lz4
ones; see [architecture/amberpack.md](architecture/amberpack.md). The example
CLI takes the setting as `--compression none|zstd[:LEVEL]|lz4[:LEVEL]`.
````

- [ ] **Step 3: `PORTING.md`**

In the "Interoperable but not byte-identical" list, replace the zstd bullet with:

```markdown
- Compressed record payloads. For zstd, Go uses `klauspost/compress`
  (EncodeAll, four tiers) and Rust libzstd (the `zstd` crate, 22 levels); for
  lz4, Go uses `pierrec/lz4/v4` (HC levels up to 9) and Rust liblz4 (the
  `lz4` crate, HC levels up to 12). A level therefore means the nearest tier
  in Go and exactly itself in Rust. The compress-only-if-strictly-smaller
  rule is identical, but compressed payloads — and therefore record CRCs,
  segment bodies, and pack bytes containing them — differ between
  implementations. Correctness is unaffected: keys hash the *uncompressed*
  object bytes.
```

In the dependency table, after the `klauspost/compress/zstd` row, add:

```markdown
| `pierrec/lz4/v4` | `lz4` (liblz4) |
```

In the `### amberpack` section, replace `compress-only-if-strictly-smaller (libzstd default level)` with:

```markdown
a codec id in the flags byte (0 raw, 1 zstd, 2 lz4) chosen by the caller
through `Compression` (Go: `Compression{Algorithm, Level}`; the default is
none), compress-only-if-strictly-smaller
```

Add at the end of that section:

```markdown
`encode_record_with(k, data, c)` is Go's `EncodeRecordWith`; `Writer::new(w)
.compression(c)` is Go's `NewWriter(w, WithCompression(c))`. In `packstore`,
`Options::compression` and `Options::compression_for` are Go's
`WithCompression` and `WithCompressionFor`; the callback takes `(&Key, &[u8],
Compression)` and returns the `Compression` to use. `Options` holds the
callback behind an `Arc`, so it is `Clone` and not `Copy`. Go's
`ErrInvalidCompression` is `amberpack::Error::InvalidCompression`, tested
with `is_invalid_compression()` on either error type.
```

- [ ] **Step 4: `port-notes/amberpack.md`**

In the API mapping table, replace the `EncodeRecord` row and add the new rows after it:

```markdown
| `EncodeRecord(k, data)` | `encode_record(k, &data)` (raw, as in Go) |
| `EncodeRecordWith(k, data, c)` | `encode_record_with(k, &data, c)` |
| `Compression{Algorithm, Level}`, `None` / `Zstd` / `LZ4` | `Compression::{None, Zstd { level }, Lz4 { level }}` (`#[non_exhaustive]`) |
| `Compression.Validate()` / `.String()` / `ParseCompression(s)` | `Compression::validate()` / `Display` / `FromStr` |
| `NewWriter(w, WithCompression(c))` | `Writer::new(w).compression(c)` |
| `errors.Is(err, ErrInvalidCompression)` | `Error::is_invalid_compression()` |
```

Add below the table:

```markdown
Go's `Compression` can name an algorithm that does not exist
(`Algorithm(9)`) and rejects it in `Validate`; the Rust enum cannot, so
`validate` only checks levels. Go's `none` with a level is likewise a state
the Rust type cannot hold; `FromStr` rejects the text `none:1` with the same
message.
```

- [ ] **Step 5: Check and commit**

Run: `cd $RS && grep -rn -iE "per-record-zstd|individually-zstd|zstd flag|flags & zstd" README.md PORTING.md architecture src | grep -v "^architecture/packstore.md"`
Expected: no output.

```bash
cd $RS && git add architecture/amberpack.md README.md PORTING.md port-notes/amberpack.md
git commit -m "docs: the codec id, compression as the writer's choice, the Go names"
```

---

### Task 17: Verify the Rust side, publish, link the pair, clean up

**Files:** none changed in either repository.

**Interfaces:**
- Consumes: everything above.
- Produces: the pull request on `amber-store/core-rs`, and both pull requests naming each other.

- [ ] **Step 1: Run what CI runs**

```bash
cd $RS
nix develop -c cargo fmt --check
nix develop -c cargo clippy --locked --all-targets -- -D warnings
nix develop -c cargo build --locked --all-targets
nix develop -c cargo test --locked
AMBER_GO_REPO=$GO nix develop -c bash interop/check.sh
```

Expected: each command exits 0.

- [ ] **Step 2: Ask the user before publishing**

Ask: "The Rust side is done and green locally, interop included. Shall I push `compression-options` to `amber-store/core-rs` and open the pull request?" Continue only on a yes.

- [ ] **Step 3: Push and open the pull request**

Use the title of the Go pull request, word for word, and the Go pull request's number `N`:

```bash
cd $RS && git push -u origin compression-options
gh pr create --repo amber-store/core-rs --base main --head compression-options \
  --title "Compression options: algorithm and level per store, a choice per object, lz4" \
  --body "$(cat <<'EOF'
The Rust side of amber-store/core#N: a caller that opens a packstore chooses the compression of what it writes (none, zstd or lz4, each at a level), optionally per object through a callback.

**The default changes: a store opened without options no longer compresses.** `Options::new().compression(Compression::Zstd { level: 0 })` restores what 0.9.0 did. `encode_record` and the wire `Writer::add` follow the same default; `Writer::new(w).compression(c)` sets it for a pack.

- The record's flags byte is now a codec id: 0 raw, 1 zstd, 2 lz4. Existing records are valid as they are, and no format version changes.
- Releases up to 0.9.0 read raw and zstd records and reject lz4 ones as corrupt.
- `Options::compression_for` asks a callback for each object the store encodes. `Options` is now `Clone` and no longer `Copy`.
- The example CLI takes `--compression none|zstd[:LEVEL]|lz4[:LEVEL]`.
- New vectors `amberpack/records_lz4.json`; every existing vector regenerates byte for byte with zstd asked for explicitly. `interop/check.sh` has each side read what the other wrote with zstd and with lz4.
- `tools/vectorgen` and the CI interop job pin the head of the Go branch; they move to the tag at release.

Go counterpart: amber-store/core#N

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01JSP748hiEjkPGTrZXqWRte
EOF
)"
```

- [ ] **Step 4: Name the Rust pull request in the Go one**

With `M` the number just created, fetch the Go pull request's body, replace the line `Rust counterpart: to follow (amber-store/core-rs, same branch name).` with `Rust counterpart: amber-store/core-rs#M`, and write it back:

```bash
gh api repos/amber-store/core/pulls/N --jq .body > /tmp/go-pr-body.md
perl -pi -e 's|^Rust counterpart: .*$|Rust counterpart: amber-store/core-rs#M|' /tmp/go-pr-body.md
gh api -X PATCH repos/amber-store/core/pulls/N -F body=@/tmp/go-pr-body.md --jq .html_url
rm /tmp/go-pr-body.md
```

- [ ] **Step 5: Watch CI**

Run: `gh pr checks N --repo amber-store/core --watch` and `gh pr checks M --repo amber-store/core-rs --watch`
Expected: all green. The core-rs macOS job takes about 15 minutes. A red check with no steps and "job was not acquired by Runner" is GitHub capacity: re-run it.

- [ ] **Step 6: Remove what was built**

```bash
cd $RS && nix develop -c cargo clean
```

and delete the scratch probe directories next to the worktrees if they exist (`lz4probe`, `lz4crate`). Both worktrees stay until the pull requests are merged.

- [ ] **Step 7: Report**

Tell the user: the two pull request links, that CI is green (or what is not), the default change and the lz4 compatibility rule in one sentence each, and that nothing is merged, tagged or released. The consumers that will write uncompressed once they move to this release (dstore, ajj, dstore-client-rs, and transport-iroh's `SendPack`) and dstore-web's lz4 gap are listed in the spec under "Behaviour change"; say so, and that none of them was touched.
