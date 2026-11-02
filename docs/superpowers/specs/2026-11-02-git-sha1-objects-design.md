# Git SHA-1 objects

**Status:** implemented on branch `git-sha1-objects`. The format and its reasons
live in [architecture/git-objects.md](../../../architecture/git-objects.md); this
records what was asked for and which calls were made along the way.

## Intent

The store is to be used to push git repositories and branches to. It needs an
object type that holds a git object under the id git gives it.

## Asked for

- A new object type: git SHA-1.
- The size is not encoded in the key: it encodes to 0.
- The type stays at the end of the key, as with the others.
- The SHA-1 keeps its normal ordering: it is not reversed.
- gc follows the links between git objects.
- The record limit stays as it is (256 MiB).
- core-rs is updated at the same time.

## Calls made

- **Type 6, `GitSHA1`.** One type for blob, tree, commit and tag: a git id does
  not tell them apart.
- **Layout:** SHA-1, ten zero bytes, a zero length, header `0x60`. Anything else
  after the SHA-1 does not validate.
- **Payload:** the exact bytes git hashes, `<kind> <size>\0<content>`.
- **Collision-detecting SHA-1** (`pjbgf/sha1cd`, `sha1-checked` in Rust), a new
  dependency on each side.
- **Verification** checks the hash, the frame and that the links can be read, and
  stays well short of `git fsck`, so that old repositories get in.
- **Submodule commits are not followed**, and a mode goes by git's `canon_mode`.
- **Blobs are marked without being read**, going by the referring tree entry or
  tag, with a second bit so that a link that calls the same object a directory
  still gets it walked.
- **Segment format version 4** for git records. Releases up to 0.10.0 would
  truncate an active segment at the first one, so they must refuse the store.
- **File operations refuse git keys.** `ls`, `restore` and `export` are not taught
  to read a git tree.

## Not in this work

- The push command itself: the next piece.
- Reading a pushed branch back as files, or serving `git fetch`.
- SHA-256 repositories.
- Git objects above 256 MiB, deltas between versions.
