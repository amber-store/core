# The Commit object, revised: jj's fields, a footprint length, commits inside directories

**Date:** 2026-09-23
**Status:** directed by the user on 2026-09-23: "make changes to the commit
object to support jj"; "I would like to keep the key semantics analogue to the
rest. We should have the number of bytes of the children plus the size of the
commit object itself"; "directory object should be able to point to commits.
For the file operations (tar export for example) the commit object would be
skipped and the tree of the commit can be used". Stage 3 of 3 on the way to a
jj backend. Branch `commit-jj`, from `main`; independent of stages 1 and 2.
**Repos:** `amber-store/core` (this change); `amber-store/core-rs` follows
through the golden vectors defined here.

## One decision to confirm: which children the length counts

The length field of a commit's key becomes a footprint, like a directory's:
**the commit's own serialized bytes plus the length of each of its trees**
(the tree, and the further terms of a conflict). **Parent commits are not
counted**, although they are children in the object graph. Two reasons:

- **It would overflow.** A parent's length would itself contain its tree and
  its parents, so a merge would count the history its two parents share twice.
  A feature branch is cut from a recent commit, so both parents of its merge
  carry almost the whole history: every merge roughly doubles the value. The
  field is at most 8 bytes; a repository with a 1 GiB history overflows it
  after about 34 such merges, and `key.New` then fails, so the commit could
  not be created. Linear history alone would already sum every snapshot ever
  taken.
- **It would not be a footprint.** The reason for the rule (next section but
  one) is that a directory may hold a commit, and a directory's length is the
  `du`-style size of what is beneath it. With parents counted, a directory
  holding a commit would report the size of that commit's whole history.

With the trees only, a commit's length is the size of the snapshot it records
plus its own few hundred bytes, which is what a directory listing should show
for it, and it can be verified from the commit's bytes alone.

## Background

`Commit` (CAS type 5, shipped in v0.0.9 on 2026-09-22) mirrors git's commit
object. A jj backend stores jj's `Commit`, which has more:

| jj `backend::Commit` | amber `Commit` today | this change |
| --- | --- | --- |
| `parents`, `description`, `author`, `committer` | covered (ms map into ns losslessly) | none |
| `change_id` | missing | optional key 7 |
| `root_tree: Merge<TreeId>` (a conflict is 2n+1 trees) | one tree | optional key 8, the terms after the first |
| `conflict_labels: Merge<String>` | missing | optional key 9 |
| an empty author or committer name (unconfigured user) | rejected | allowed |
| `secure_sig` | opaque signature bytes; the payload rule fits jj's signing callback | none |
| `predecessors` | missing | left out: deprecated in jj, which keeps them in its operation log; they would also be the first non-owning edge in the object graph |

## The record

A canonical CBOR map (RFC 8949 §4.2 core-deterministic), integer keys. Keys
0–4 are always present; 5–9 are omitted when absent, so a commit that uses
none of the new fields encodes exactly as before.

| CBOR key | Field | CBOR type | Notes |
| --- | --- | --- | --- |
| 0 | tree | 32-byte byte string | `DirLeaf` or `DirNode` key. In a conflicted commit, the first term |
| 1 | parents | array of 32-byte byte strings | unchanged |
| 2 | author | identity map | **name may now be empty** (0–1024 bytes) |
| 3 | committer | identity map | likewise |
| 4 | message | text string | unchanged |
| 5 | signature | byte string, optional | unchanged |
| 6 | public_key | byte string, optional | unchanged |
| 7 | change_id | byte string, optional | 1–64 bytes, opaque: an identity that follows the change when the commit is rewritten |
| 8 | conflict_terms | array of 32-byte byte strings, optional | the terms of a conflicted tree after the first, alternating *remove, add, remove, add, …* (jj's order); an even number, 2–254; each a `DirLeaf` or `DirNode` key; a key may repeat |
| 9 | conflict_labels | array of text strings, optional | only with key 8, one label per term counting the tree (so `1 + len(conflict_terms)`), each 0–65536 bytes of valid UTF-8 with no code point below U+0020 and no U+007F (raised from 1024 after review: jj puts a description's whole first line into a label), at least one of them non-empty; absent when no term is labelled |

A **conflicted commit** records the tree `A0 − R0 + A1 − R1 + …`: key 0 holds
`A0` and key 8 holds `R0, A1, R1, A2, …`. Wherever a commit stands for a
directory (`ls`, `export`, `restore`, a directory entry), it stands for key 0,
the first side. Considered and rejected: the git backend's trick of a
synthetic tree with `.jjconflict-side-N` directories, which exists to make
git keep the sides alive; here the walk below does that.

The signature payload is unchanged in rule — the encoding without key 5 — and
therefore covers the new fields. Decoding stays strict: the input must be the
bytes the encoder would produce, so an empty array under key 8 or 9, labels
without terms, or all-empty labels are rejected.

## The key

Type 5. The length field is **the commit's own serialized byte length plus
the length fields of its tree and conflict terms** (see the decision above).
An overflow of 64 bits is an encoding error. `packstore.verifyObject`, which
checked `length == len(bytes)` for a commit, decodes it and checks this sum
instead; the keys it needs are in the commit's own bytes.

## Graph semantics

`fstree.ChildKeys` of a commit returns the tree, the conflict terms in order,
then the parents in order. Reachability, completeness and the gc mark all
dispatch through it, so every side of a conflict is transferred, required and
kept alive with the commit.

## Commits inside directories

A directory entry of type `S_IFDIR` may carry a `Commit` key as its content
key, where until now only a `DirLeaf` or `DirNode` key could stand. It reads
as the directory the commit records:

- `fstree.LookupEntry`, `ListEntries` and `CollectEntries` accept a commit
  key wherever they accept a directory key and continue with its tree; so do
  `ResolvePath` and `ResolveEntry`, which are built on them, and
  `tarexport.Write`. New: `fstree.DirOf(k, get)`, the directory object a key
  stands for. A commit's tree is never a commit, so one step suffices.
- File operations (`ls`, `export`, `restore`) therefore pass through the
  commit to its tree, and a tar of such a tree contains a plain directory.
- The object graph needs nothing new: a `DirLeaf`'s children are its content
  keys, so a directory that holds a commit keeps the commit, its tree **and
  its history** alive, requires all of it for completeness, and transfers it.
- The directory's length adds the entry's content-key length, as for any
  entry, which with the rule above is the snapshot's footprint.

This is also where jj's `TreeValue::GitSubmodule(CommitId)` has a place to go.
Nothing in this change creates such an entry: `ingest` reads a filesystem,
which has none.

## Code

- `commit`: `Commit` gains `ChangeID []byte`, `ConflictTerms []key.Key`,
  `ConflictLabels []string`; `Trees()` (the tree, then the terms) and
  `Conflicted()`; bounds `MaxChangeIDLen = 64`, `MaxConflictTerms = 254`,
  `MaxLabelLen = 65536`; validation and the wire struct for keys 7–9; an
  identity's name may be empty; `Object` computes the footprint length.
- `fstree`: `ChildKeys` follows `Trees()`; `DirOf`; the three directory
  readers take a commit key.
- `packstore/verify.go`: the commit length check. `packstore` imports
  `commit` (which imports only `key` and the CBOR library).
- `tarexport`: a commit root is accepted.
- CLI: `descend` no longer peels by hand, the readers do; `commit create`
  resolves its `TREE` with `fstree.DirOf` and gains `--change-id HEX`;
  `commit show` prints `change-id`, `conflict-remove`/`conflict-add` and
  `conflict-label` lines.

## Testing

- `commit`: round trips with each new field and with all; hand-assembled
  bytes for a conflicted commit; a validation table (odd or too many terms, a
  term that is not a directory key, labels without terms, a label count that
  does not match, all-empty labels, label and change-id bounds, control
  characters); strict-decoding rejections (empty arrays under 8 and 9,
  reordered keys); an empty identity name accepted; the key's length is own
  bytes plus every term's length, and overflow is an error; **two golden
  vectors**: the existing merge (its bytes and key change, because its
  parents' keys did) and a conflicted, labelled commit with a change id.
- `fstree`: `ChildKeys` order with conflict terms; reachability and
  completeness require every term; the readers and `DirOf` through a commit
  root and through a directory entry that holds a commit, at the root and in
  a subdirectory; a `DirLeaf` holding a commit has the expected length; a
  missing commit or tree behind such an entry fails completeness.
- `packstore`: a commit with own-bytes-only length (the v0.0.9 rule) now
  fails verification; the footprint length passes; a conflicted commit's
  length must include every term.
- `gc`: a reference to a tree that holds a commit keeps the commit's tree and
  ancestors through a cycle; dropping it reclaims them.
- `tarexport` and CLI end to end: a tree with a commit entry exports and
  lists as a plain directory; `commit create --change-id`, `commit show`.

## Compatibility

**Every commit key changes**, because the length field is part of the key:
commits written by v0.0.9 carry their own byte length there, fail
verification under the new rule, and cannot be named as parents of new
commits with their old keys. There is no migration; v0.0.9 is a day old, and
such commits are re-created. A commit that uses keys 7–9 cannot be decoded by
v0.0.9, which rejects unknown keys. `core-rs` adopts the change through the
two golden vectors.
