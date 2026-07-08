---
title: Integrity & Threat Model
description: What docbank defends against, which layer owns each integrity guarantee, and the trade-offs that were considered and deliberately not taken.
---

# Integrity & Threat Model

This page records where docbank's integrity guarantees live and — just
as deliberately — where they don't. Several of the decisions below have
been proposed, evaluated, and declined during review; they are written
down here so the rationale is auditable and doesn't have to be
re-litigated from scratch each time.

## Trust boundary

docbank is a **single-user tool**. The vault lives under a `0700`
directory owned by the user, its location comes from the user's own
environment (`DOCBANK_HOME`), and every docbank process runs with the
user's privileges. Consequently:

- **In scope:** crashes and power loss at any instant; filesystem bit
  rot; accidental damage to the vault or source trees (a stray symlink
  where a document or blob *file* belongs, a deleted file, a manual
  edit inside `~/.docbank`); concurrent docbank processes; a *stale* or
  *tampered* object being silently vouched for. The vault's own
  *directory structure* is trusted — see the read-guarantee note below.
- **Out of scope:** an adversary with the user's own privileges. Anyone
  who can race a running docbank process while rewriting the user's
  files already executes as the user and does not need docbank as a
  vector. Defenses whose only payoff is against that actor are declined
  by policy.

## Which layer owns which guarantee

| Guarantee | Owner | Mechanism |
|---|---|---|
| A referenced blob is durable | `blob.Write` | tmp → fsync → rename → dir fsync, on every path including dedup |
| A deleted blob stays deleted | `blob.Remove` | shard dir fsync before gc deletes the metadata row, including on the already-missing retry path |
| A stored object is what its name claims *structurally* | `blob.Write` dedup check | no-follow `Lstat`: regular file of the expected size, else replaced with the verified temp file |
| Reads serve only stored blobs | `blob.Open` / `blob.Exists` | no-follow open + regular-file check on the blob itself; the vault's own directory structure above it is trusted per the boundary above (a symlinked shard dir is user-privileged relocation or tampering, not an attack docbank can meaningfully resist) |
| Content matches its hash *byte-for-byte* | `docbank verify` | full re-hash of every blob, on demand |
| No orphan blob file survives | `docbank gc` | reachability query for rows **plus** a directory scan for files that never gained (or lost) their row |
| Imports read the file they classified | ingest | `O_NOFOLLOW` + fstat at open, not the earlier `Lstat`/`WalkDir` classification |
| Mutations act on the path the user named | store | path resolution inside the mutation's transaction (`TrashPath`, `MovePath`, ID-based ingest parentage) |
| Node identity is permanent | schema | `AUTOINCREMENT` ids plus `trash_parent ... ON DELETE SET NULL` — a dangling origin becomes NULL, never a reused id |
| Vault contents are private to the user | `home.Ensure` | the 0700 boundary is enforced on every open, not assumed: layout directories are tightened to 0700, the database file to 0600 (WAL/SHM inherit its mode), and blob files are written 0600 |

## Accepted trade-offs

These were proposed (most more than once) and deliberately not taken.

### No re-hash on the dedup fast path

`blob.Write`'s dedup check verifies structure (regular file, expected
size), not content. Re-hashing the existing blob on every duplicate
import would double the I/O of the most common bulk-import case to
detect exactly one thing the structural check misses: **same-length
corrupt bytes**, i.e. bit rot or tampering.

That detection intentionally lives in `docbank verify`, and for a
reason beyond cost: a corrupt blob is equally wrong for every node
*already pointing at it*. Catching it only when a duplicate import
happens to pass by is not an integrity guarantee — it's a coincidence.
The systematic answer is a scan that covers every blob regardless of
import traffic, which is precisely `verify`'s contract.

### No fd-relative directory traversal on import

`docbank add` walks source trees by path (`filepath.WalkDir`). A
directory swapped for a symlink *between classification and descent*
can redirect the walk — but only by an actor mutating the user's own
source tree mid-import, which is outside the trust boundary above. The
payoff (importing user-readable files into the user's own vault) does
not justify platform-specific `openat`/`fstatat` machinery. The
*accidental* case — a symlink sitting in the tree — is handled: it is
classified, reported, and skipped, and the final open refuses to
follow links regardless.

### No schema migrations before the first release

Schema changes apply via `CREATE TABLE IF NOT EXISTS`, so a vault
created before a schema change keeps its old shape. Rebuild migrations
for SQLite (new table, copy, drop, rename, across the FTS triggers)
are high-risk surgery; writing them while **zero released vaults
exist** is risk with no beneficiary. This holds until the first
release that can leave real vaults behind a schema change, at which
point migration machinery becomes part of the release contract.

### Blocking vault lock

Commands wait indefinitely on the vault flock rather than failing
fast. Ordinary operations hold the shared lock for milliseconds; only
`gc --run` holds it exclusively, briefly. A lock timeout would convert
rare, short waits into user-visible errors. (Surfacing a "waiting for
vault lock" notice is tracked as a CLI UX issue.)
