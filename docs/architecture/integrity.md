---
title: Integrity & Trust
description: What docbank defends against, which layer owns each integrity guarantee, and the trade-offs that were considered and deliberately not taken.
---

# Integrity and trust

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
| A stored object is what its name claims *structurally* | `blob.Write` dedup check | Kit performs a no-follow identity check; a wrong-sized object, symlink, or special file fails closed and is left unchanged for explicit recovery |
| Reads serve only stored blobs | `blob.Open` / `blob.Exists` | no-follow open + regular-file check on the blob itself; the vault's own directory structure above it is trusted per the boundary above (a symlinked shard dir is user-privileged relocation or tampering, not an attack docbank can meaningfully resist) |
| Metadata and audit authority are internally consistent | `docbank verify` | relational validation plus independent replay and reconciliation of canonical audit history |
| Permanent audit evidence and retained bytes agree | `docbank audit verify` | the same independent replay plus stable lineage/scope terminal evidence and a re-hash of every unique protected blob |
| Content matches its hash *byte-for-byte* | `docbank verify`; `POST /nodes/{id}/verify` | full-vault re-hash on demand, or a revision-bound fresh read of one file through the mixed store |
| No orphan blob file survives | `docbank gc` | reachability query for rows **plus** a directory scan for files that never gained (or lost) their row |
| Imports read the file they classified | ingest | `O_NOFOLLOW` + fstat at open, not the earlier `Lstat`/`WalkDir` classification |
| Remote writes match the writer's bytes | `POST /uploads` | required SHA-256/size declarations are compared with Kit's streamed result before any blob row or node authority commits |
| Mutations act on the node the caller named | store | id-addressed mutations (`Move`, `Trash`, `Restore`) require an `If-Match` revision precondition; path-addressed mutations (`MovePath`, `TrashPath`, backing `POST /path/move` and `/path/trash`) resolve and mutate inside one store transaction — either way, no path-race window between resolving a name and acting on it |
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

### Fail closed on an invalid canonical object

An existing wrong-sized file, symlink, or special file at a blob's canonical
path is evidence of damage or manual modification, not a valid deduplication
target. `blob.Write` returns a content-mismatch error and leaves that object
unchanged; it does not replace a path whose identity may have raced or been
tampered with.

Recovery is explicit: stop the daemon, move the suspect object outside the
vault for diagnosis, restart the daemon, and re-add the content from a trusted
source (or restore it from a verified backup). The durable write happens before
the existing node is recognized as an idempotent ingest, so re-adding repairs a
missing loose copy without creating a duplicate document. Run `docbank verify`
afterward to confirm the vault.

### No fd-relative directory traversal on import

`docbank add` walks source trees by path (`filepath.WalkDir`). A
directory swapped for a symlink *between classification and descent*
can redirect the walk — but only by an actor mutating the user's own
source tree mid-import, which is outside the trust boundary above. The
payoff (importing user-readable files into the user's own vault) does
not justify platform-specific `openat`/`fstatat` machinery. The
*accidental* case — a symlink sitting in the tree — is handled: it is
classified, reported, and skipped, and the final open refuses to
follow links regardless. The one explicit exception is a source argument whose
final component is a symlink to a directory, which is common for cloud-storage
roots. Docbank resolves that link before walking and continues on the resolved
path, so retargeting the user-facing link after resolution cannot redirect the
walk. Descendant symlinks are still skipped, while virtual naming and provenance
retain the spelling the user supplied.

### Pre-release schema freedom

Docbank has not yet established a public storage compatibility boundary. Until
the first public release, incompatible schema and JSONL changes replace the
development shape directly while the format identifier remains version 1.
Developer vaults created by earlier commits are disposable; do not build
migrations, compatibility decoders, cutovers, or downgrade fences for them.

The first public release freezes that v1 contract. Any later incompatible
change must begin by defining an explicit compatibility policy and real
released-vault fixtures; that work is deliberately deferred until it is needed.

### One vault lock holder

The daemon holds the portable vault file lock exclusively for its whole lifetime,
acquired non-blocking at startup — a second daemon is refused
immediately, never queued. Ordinary commands don't touch the lock at
all: they are HTTP clients of the daemon. Maintenance (`gc --run`,
`verify`, `trash empty`) is serialized against ordinary mutations by
the daemon's in-process maintenance gate. See
[Ownership & Concurrency](locking.md).

Next: [Backup & Recovery](backup.md) covers the snapshot architecture these
guarantees extend to; [Troubleshooting](../troubleshooting.md) applies them
when something looks wrong.
