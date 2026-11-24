---
title: Integrity and trust
description: What Docbank defends against, which layer owns each integrity guarantee, and the trade-offs that were considered and not taken.
---

# Integrity and trust

Docbank verifies stored content and metadata within a single-user trust
boundary. Its checks catch crashes, corruption, stale state, and accidental
damage, though they cannot protect you from someone who can rewrite both the
vault and its expected evidence.

This page owns that trust boundary and the rationale for accepted trade-offs.
The mechanism table identifies which component performs each check.

## Trust boundary

Docbank is a **single-user tool**. Your vault lives in a `0700` directory that
you own, its location comes from your environment (`DOCBANK_HOME`), and every
Docbank process runs with your privileges. That boundary determines what
Docbank defends against:

- **In scope.** Crashes and power loss at any moment, filesystem bit rot,
  accidental damage to the vault or source trees (such as a stray symlink where
  a document or blob *file* belongs, a deleted file, or a manual edit inside
  `~/.docbank`), concurrent Docbank processes, and a *stale* or *tampered*
  object being silently accepted as valid. Docbank trusts the vault's own
  *directory structure*, as noted in the read guarantee below.
- **Out of scope.** An adversary who already has your privileges. Anyone who
  can race a running Docbank process while rewriting your files already runs as
  you and does not need Docbank to reach them. Docbank declines defenses whose
  only benefit is against that actor.

## Which layer owns which guarantee

| Guarantee | Owner | Mechanism |
|---|---|---|
| A referenced blob is durable | `blob.WriteContext` | raw or zstd tmp → fsync → publish → dir fsync, on every path including dedup |
| A deleted blob stays deleted | `blob.Remove` | gc deletes the metadata row and records the exact loose object for retirement in one transaction; removal then fsyncs the shard dir, including on the already-missing retry path, and clears the record only after it succeeds |
| A stored object is what its name claims *structurally* | `blob.WriteContext` dedup check | Kit performs a no-follow identity check; a wrong-sized object, symlink, or special file fails closed and is left unchanged for explicit recovery |
| Reads serve only stored blobs | `blob.OpenContext` / `blob.Exists` | no-follow open + regular-file check on the blob itself; the vault's own directory structure above it is trusted per the boundary above (a symlinked shard dir is user-privileged relocation or tampering, not an attack Docbank can meaningfully resist) |
| Metadata and audit authority are internally consistent | `docbank vault verify` | relational validation plus independent replay and reconciliation of canonical audit history |
| Permanent audit evidence and retained bytes agree | `docbank audit verify` | independent replay, an optional exact-prefix proof against externally recorded lineage/scope evidence, and a re-hash of every unique protected blob |
| Content matches its hash *byte-for-byte* | `docbank vault verify`; `POST /nodes/{id}/verify` | full-vault re-hash on demand, or a revision-bound fresh read of one file through the mixed store |
| An embedded repair preserves logical identity | `Vault.RepairContent` | trusted bytes are fully checked against the existing logical SHA-256 and size before loose physical authority replaces raw, zstd, or packed authority; nodes and versions remain unchanged |
| No orphan blob file survives | `docbank vault gc` | reachability query for rows **plus** a directory scan for files that never gained (or lost) their row |
| Imports read the file they classified | ingest | `O_NOFOLLOW` + fstat at open, not the earlier `Lstat`/`WalkDir` classification |
| Remote writes match the writer's bytes | `POST /uploads` | required SHA-256/size declarations are compared with Kit's streamed result before any blob row or node authority commits |
| Mutations act on the node the caller named | store | id-addressed mutations (`Move`, `Trash`, `Restore`) require an `If-Match` revision precondition; path-addressed mutations (`MkdirPath`, `MovePath`, `TrashPath`, backing `POST /path/mkdir`, `/path/move`, and `/path/trash`) resolve and mutate inside one store transaction. Either way, there is no path-race window between resolving a name and acting on it |
| Node identity is permanent | schema | `AUTOINCREMENT` ids plus `trash_parent ... ON DELETE SET NULL`. A dangling origin becomes NULL, never a reused id |
| Vault contents are private to the user | `home.Ensure` | the 0700 boundary is enforced on every open, not assumed: layout directories are tightened to 0700, the database file to 0600 (WAL/SHM inherit its mode), and blob files are written 0600 |

## Accepted trade-offs

Each of the following ideas was proposed, most of them more than once, and
declined.

### No re-hash on the dedup fast path

`blob.WriteContext`'s dedup check verifies structure (that the target is a
regular file with the expected size), not content. Re-hashing an existing blob
on every duplicate import would double the I/O of the most common bulk-import
path to catch only one thing the structural check misses: **same-length corrupt
bytes** from bit rot or tampering.

Instead, `docbank vault verify` handles same-length corruption. A corrupt blob
also affects every node that already points to it, and verification scans every
blob so detection does not wait for a later duplicate import.

### Fail closed on an invalid canonical object

If a wrong-sized file, symlink, or special file already sits at a blob's
canonical path, that points to damage or manual modification rather than a
valid deduplication target. `blob.WriteContext` returns a content-mismatch
error and leaves the object untouched instead of overwriting a path that may
have raced or been tampered with.

Recovery happens as a separate step. In embedded mode, an owner with trusted
bytes calls `RepairContent` for the exact existing logical identity; it
verifies the full stream before replacing physical authority and keeps every
node and historical version reference intact. In standalone mode, you restore
from a verified backup or run an offline recovery procedure. `Put` and `Create`
never silently turn a normal write into a repair. Always verify repaired or
restored bytes before relying on them.

### No fd-relative directory traversal on import

`docbank document add` walks source trees by path with `filepath.WalkDir`.
Swapping a directory for a symlink *between classification and descent* can
redirect the walk, but only if someone modifies your source tree mid-import,
which sits outside the trust boundary above. Adding platform-specific
`openat`/`fstatat` machinery is not worth the complexity when the operation
only imports files you can already read into your own vault. Docbank still
handles the *accidental* case of a symlink sitting in the tree by classifying,
reporting, and skipping it, and the final open refuses to follow links
regardless. The one exception is a source argument whose final component is a
symlink to a directory, which is common for cloud-storage roots. Docbank
resolves that top-level link before walking and traverses the resolved path, so
retargeting the link afterward cannot redirect the walk. Descendant symlinks
are still skipped, while virtual naming and provenance keep the spelling you
supplied.

Watched inboxes enforce a stricter traversal boundary because they run
repeatedly without supervision, and wandering into the live vault would create
a self-ingestion loop. A watcher pins its resolved source with `os.Root`, opens
every descendant relative to held directory descriptors, rejects symlink or
reparse-point components, and refuses to cross a filesystem mount. On Linux, it
requires a kernel-provided mount ID rather than falling back to a device number
that would mistake a bind mount for the source mount. Before ingesting
anything, the watcher also compares every descended directory against the
pinned vault-root identity, catching a vault located physically beneath the
source even when bind aliases hide that relationship from path comparison. The
same identity check stays active on every later scan.

### Released schema upgrades

v0.9.0 established Docbank's first public storage compatibility boundary, and
later layouts store an explicit storage-schema version. When an incompatible
SQLite change lands, released vaults upgrade through one shared logical-cutover
driver. Docbank picks the adapter for the source generation, exports
deterministic metadata-v1 JSONL from the old database, imports and validates it
in a fresh current-schema database, restores physical pack authority, and only
then publishes the replacement. A v0.9.0 source stays beside the new database
as `<database>.v0.9.0.bak`, and later source generations keep their own
version-identified recovery copy.

Docbank pairs the version marker with a physical downgrade fence. The mandatory
identity read that an unversioned v0.9 binary runs at startup fails against any
newer database, which stops an older process from ignoring an unfamiliar
version column and mutating current storage with obsolete rules.

Docbank does not maintain an in-place migration ledger. Unreleased development
layouts stay disposable, while every supported released layout has an exact
fixture and a direct-to-current end-to-end cutover test under both SQLite
implementations. Metadata stays at format v1 until a released logical-format
change requires a deterministic version normalizer.

### One owner for an entire vault tree

Both the daemon and an embedded `Vault` acquire the portable hierarchy lock
non-blocking at startup and hold it exclusively for their entire lifetime. If
another process tries to own the same root, any ancestor, or any descendant,
Docbank refuses it immediately instead of queuing it. Unrelated vault roots can
stay open at the same time. Ordinary CLI commands never touch the lock because
they talk to the daemon as HTTP clients, and the owner's in-process mutation
gate serializes maintenance against ordinary mutations. See
[Ownership and concurrency](locking.md) for details.

Next, see [Backup and recovery](backup.md) for how these guarantees extend to
snapshots, or [Troubleshooting](../troubleshooting.md) when something looks
wrong.
