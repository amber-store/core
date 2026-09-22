# Commit object type

**Date:** 2026-09-22
**Status:** approved as is on 2026-09-22; implemented on branch `commit-object`.
**Repos:** `amber-store/core` (this change); `amber-store/core-rs` follows
through the golden vector defined here.

## Background

Five of the sixteen CAS object types are defined (`Blob`, `FileNode`,
`DirLeaf`, `DirNode`, `XattrSet`). `architecture/types.md` closes with
"snapshot/versioning objects are intentionally out of scope at this stage",
and references are mutable names with no history. The store can name a root
but cannot say who made it, when, why, or what it replaced.

## Goal

A sixth object type, `Commit` (type 5): the analogue of git's commit object,
carrying the same data, encoded as deterministic CBOR like every other
structured object. The object-graph walks (reachability, completeness, gc
mark) follow it, packstore verifies it, and the docs describe it.

## Non-goals

- Branch, merge, diff or log machinery. A reference pointing at a commit key
  is the branch; moving it is the consumer's job.
- Creating or verifying signatures. As with references, core carries the
  signature fields opaquely; signing is a consumer concern.
- Shallow history. A commit is complete only when its whole ancestry is
  present.
- Tag objects, git import/export.
- The `core-rs` port. It lands there separately, proven by the vector below.

## Mapping from git

| git commit | amber `Commit` |
| --- | --- |
| `tree` | key 0, a directory root key |
| `parent` (0..n, ordered) | key 1, array of `Commit` keys |
| `author` name, email, time, tz | key 2, identity map |
| `committer` name, email, time, tz | key 3, identity map |
| message | key 4, UTF-8 text |
| `gpgsig` | keys 5 and 6, SSHSIG blob and public key (reference convention) |
| `encoding` | dropped: the message is UTF-8 by definition |
| `mergetag`, unknown extra headers | dropped: there are no tag objects |

## The record

A canonical CBOR map (RFC 8949 §4.2 core-deterministic) with integer keys,
the same convention as `DirLeaf` entries and reference records.

| CBOR key | Field | CBOR type | Notes |
| --- | --- | --- | --- |
| 0 | tree | 32-byte byte string | canonical key of type `DirLeaf` or `DirNode` |
| 1 | parents | array of 32-byte byte strings | canonical keys of type `Commit`; order is significant (first parent is the mainline); empty array for a root commit; no duplicates; at most 256 |
| 2 | author | identity map | who wrote the change |
| 3 | committer | identity map | who recorded the commit |
| 4 | message | text string | UTF-8, may be empty, at most 1 MiB |
| 5 | signature | byte string, omitted when absent | raw SSHSIG v1 blob, at most 64 KiB |
| 6 | public_key | byte string, omitted when absent | signer's key, SSH wire format, at most 16 KiB |

Identity map, all four keys always present:

| CBOR key | Field | CBOR type | Notes |
| --- | --- | --- | --- |
| 0 | name | text string | 1–1024 bytes, valid UTF-8, no control characters |
| 1 | email | text string | 0–1024 bytes, same character rules |
| 2 | when | int64 | ns since the Unix epoch, the store's time convention (git stores seconds) |
| 3 | tz_offset | int | minutes east of UTC, −1439..1439 |

**Signature payload:** the deterministic encoding of the record without key
5, so the signature covers the public key. Convention for consumers that
sign: SSHSIG v1, namespace `amber-store-commit`, SHA-512. The commit key
hashes the full bytes including the signature, as in git.

**Decoding is strict:** the input must be byte-for-byte what the encoder
would produce for the same record. Unknown map keys, indefinite-length
items, non-minimal integers and trailing bytes are rejected.

## The key

Type 5. The length field is the commit's **own serialized byte length**, the
`Blob`/`XattrSet` rule. `packstore.verifyObject` therefore checks a commit's
length field as well as its hash.

Considered and rejected: own bytes + `tree.length`, a du-style snapshot
footprint. Parents cannot be included without double-counting shared data,
so the sum would be partial and surprising, the store could no longer verify
the field, and the tree key (whose length already is the footprint) is one
fetch away.

## Graph semantics

`fstree.ChildKeys` of a `Commit` returns the tree, then the parents in
order. Everything else follows from that one case, because every walk
already treats only `Blob` and `XattrSet` as leaves:

- `ReachableKeys`: transferring a commit transfers its whole history.
- `CheckComplete`, and so `gc.PrepareRef`: a reference may name a commit
  only when its entire ancestry is present. A commit is an interior node,
  so a missing parent surfaces as the wrapped `get` error, exactly like a
  missing `DirNode`.
- gc mark: history stays live while any reference reaches it. The mark
  prunes at already-marked keys, so trees shared between commits cost once.
- All walks are iterative, so history depth is not a stack concern.

Known cost: `PrepareRef` re-walks the closure on every reference put, and
with commits the closure is all of history. The walk grows with the number
of unique objects ever committed, not with the size of the new commit.
Stopping the walk at commits already named by a live reference is the
obvious later lever; it is not part of this change.

## Code layout

New package `commit`, importing only `key` and fxamacker/cbor:

```go
type Identity struct {
    Name, Email string
    When        int64 // ns since the Unix epoch
    TZOffset    int   // minutes east of UTC
}

type Commit struct {
    Tree      key.Key
    Parents   []key.Key
    Author    Identity
    Committer Identity
    Message   string
    Signature []byte
    PublicKey []byte
}

func (c Commit) Encode() ([]byte, error)           // validates, deterministic
func (c Commit) Object() (key.Key, []byte, error)  // Encode + key
func (c Commit) SignaturePayload() ([]byte, error) // Encode without key 5
func Decode(b []byte) (Commit, error)              // strict canonical
```

Touched elsewhere:

- `key/type.go`: `Commit Type = 5`, `IsValid`, `String`, error text.
- `fstree/children.go`: the `key.Commit` case. `fstree` imports `commit`;
  `commit` does not import `fstree`, so there is no cycle.
- `packstore/verify.go`: `Commit` joins the length-checked types.

Considered: putting the codec in `fstree`. Rejected because a commit is not
a filesystem tree object and carries reference-style validation; `reference`
already sets the precedent of a record package with its own encoder.

## CLI

- `descend` peels a `Commit` root to its tree before walking the path, so
  `ls`, `restore` and `export` accept commit keys and references to commits.
- `amber-store commit create [--ref NAME] [--parent SPEC]... -m MSG
  --author 'Name <email>' [--committer 'Name <email>'] TREE-SPEC` prints
  the new commit key. The core CLI has no configured identity, so
  `--author` is required; the committer defaults to the author, and both
  timestamps to now in the local zone.
- `amber-store commit show SPEC` prints the record in a git-like layout.

The two `commit` commands are separable. They are proposed because without
them the CLI cannot produce the type and the end-to-end test cannot cover it.

## Testing

- `key`: type 5 is valid and named; the reserved-type tests move to 6.
- `commit`: round trip; a validation table (tree of the wrong type, parent
  that is not a `Commit` key, duplicate parents, every bound, control
  characters, tz range); canonical rejection (reordered keys, unknown key,
  non-minimal integer, trailing bytes); the signature payload differs from
  the encoding only by key 5; one **golden vector** pinning exact hex bytes
  and key for a fixed merge commit, which `core-rs` adopts.
- `fstree`: `ChildKeys` order; `ReachableKeys` and `CheckComplete` over a
  chain with a merge; a missing parent fails completeness.
- `packstore`: a commit with a wrong length field fails verification.
- `gc`: a reference to the tip keeps all ancestors' trees through a cycle;
  dropping it reclaims them.
- CLI e2e: ingest twice, `commit create` twice, `ls` and `restore` through a
  reference to the second commit, `commit show`.

## Docs

`architecture/types.md` (table row, length rule, replace the out-of-scope
paragraph), new `architecture/commits.md` holding the record tables above,
README object-type and package tables, the `fstree` package comment.

## Compatibility

The format is permanent once emitted: keys are content addresses. Readers
older than this change reject type-5 keys with `ErrReservedType`, so a pack
containing a commit fails to parse there; stores without commits are
unaffected. `core-rs` must gain the type before it can read such a store.
