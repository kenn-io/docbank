---
title: Backup and restore
description: Create incremental snapshots in a backup repository, verify them, and restore a vault from them.
---

# Backup and restore

Create a backup snapshot to recover the vault independently of its live
storage. A snapshot includes the folder tree, retained document versions, and
their retained derivatives, whether stored individually or in a pack. It
excludes unreferenced bytes waiting for garbage collection. Restore verifies
the snapshot before publishing a usable vault at a separate target.

On 64-bit systems, backup and restore can read existing packed content larger
than 512 MiB without rewriting the packs. Reading older compressed content can
require memory proportional to the full uncompressed file size.

A repository is a directory of backup files that Docbank adds to without
changing existing files. Each snapshot has a manifest, the record of which
files belong to it. Repeated captures reuse unchanged content by hash.

Each snapshot also includes a complete metadata description in JSONL, with one
JSON record per line. Docbank reuses this description when it is unchanged. A
metadata change stores one new compressed description rather than a set of
individual row changes. See [Backup architecture](../architecture/backup.md)
for the repository format.

!!! warning "Repositories are not encrypted"
    Snapshot metadata and document content are compressed but not encrypted.
    Protect the repository with filesystem permissions and encrypted storage,
    especially before placing it on removable or cloud-synchronized media.

Use `backup forget` to remove selected recovery points and `backup prune` to reclaim their storage. Embedded applications can use the [same cleanup operations](../embedding.md#remove-recovery-points-and-reclaim-storage).

## Quick start

```bash
# One time
docbank backup init --repo ~/Backups/docbank

# Capture the live vault through its daemon
docbank backup create --repo ~/Backups/docbank --tag before-reorganization

# Inspect the snapshot history
docbank backup list --repo ~/Backups/docbank

# Prove the latest snapshot, including every referenced content byte
docbank backup verify --repo ~/Backups/docbank

# Recover and prove it without touching the running vault
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank-test
```

Set a default repository to omit `--repo`:

```toml
# $DOCBANK_HOME/config.toml
[backup]
repo = "~/Backups/docbank"
```

Restart the daemon after changing `config.toml`. Configuration is read only at
daemon startup.

## Initialize a repository

```bash
docbank backup init [--repo DIR] [--json]
```

Initialization creates the repository layout and its random identity. It
refuses an existing non-empty or already initialized destination instead of
adopting unrelated files. The repository must be configured or supplied
explicitly. A relative CLI `--repo` is resolved from the invoking shell's
working directory before it is sent to the daemon.

## Create a snapshot

```bash
docbank backup create [--repo DIR] [--tag LABEL] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

The daemon captures one consistent view of the vault. It briefly pauses
changes while opening a SQLite read transaction. Normal writes then resume
into SQLite's write-ahead log (WAL), while backup reads the earlier view.

GC, trash empty, verification, and packed-storage maintenance wait until the
capture finishes. This keeps them from removing content needed by the snapshot.
The daemon accepts a document into the repository only after reading and
verifying all of its bytes. If capture fails, it publishes no partial snapshot
manifest. Address the error, then rerun the command.

During an interactive run, `--progress auto` draws an in-place bar for each
stage (freeze, metadata, attachments, and seal), including item and byte counts
when available. Redirected output uses throttled, newline-terminated progress
instead, so logs remain readable. Force either behavior with `--progress bar`
or `--progress plain`. Progress goes to stderr. `--json` suppresses it and
writes one snapshot object to stdout for automation.

`--jobs 1` serializes blob readers for repositories on spinning disks or NAS
storage. Zero uses Kit's CPU-based default. `--tag` is a free-form label shown
by `backup list`. `--force-unlock` is recovery for a known-dead repository lock,
not a way to override another running backup.

## List snapshots

```bash
docbank backup list [--repo DIR] [--json]
```

The table reports each snapshot's ID, creation time, logical file/blob counts,
total blob bytes, bytes newly added to the repository, recorded coverage, and
tag. `--json` returns the same typed
snapshot summaries as `GET /api/v1/backup/snapshots`, including the metadata
format and parent snapshot ID.

## What each snapshot contains

Create and list output include recorded coverage: photo assets and files, distinct verified photo bytes, albums and members, hidden and trashed photos, checkout definitions and entries, and retained change receipts. Browser sessions receive snapshot lists without coverage. JSON summaries expose these under `coverage`, with `version: 1`, `included`, and `excluded`; top-level `blob_bytes` records all verified blob bytes. List shows one row per snapshot and one shared exclusions line. Older snapshots show `unrecorded` in the coverage columns and omit the JSON field. Snapshots with a newer coverage version show `unsupported` and an ID-specific warning; JSON includes `coverage_warning` and omits `coverage`. Their base statistics and IDs remain available for cleanup. Restore recounts version-1 coverage before publication, preserves the recorded exclusions, and rejects unsupported versions with upgrade guidance. Verification checks manifest consistency and physical content, and reports unsupported coverage versions; it does not recount photo coverage.

Upgrade Docbank before restoring snapshots that record coverage version 1 with a binary predating that coverage format. Older binaries refuse publication with a statistics mismatch; the manifest’s physical reader version does not express this application requirement.

The private search miss log stays out of backups and coverage counts. Coverage lists browser sessions, hidden unlock sessions, transient storage operations, checkout destination bindings, and rebuildable search indexes, photo timelines, quality signals, `photo_technical_metadata`, vector indexes, and pending photo suggestion text as excluded. Suggestion dismissals and accepted-field decisions are backed up but not counted in coverage; restore describes the undecided fields again after fresh consent. Hidden passcodes and lockout decisions remain included. Ingest provenance, saved-query runs, and term-report history remain included too. The trashed-photo count includes any asset with at least one trashed managed file, even when another member remains live. Restored checkouts are unbound until you explicitly rebind them.

## Remove recovery points and reclaim storage

```bash
docbank backup forget SNAPSHOT [SNAPSHOT...] [--repo DIR] [--dry-run] [--allow-empty] [--force-unlock] [--json]
docbank backup prune [--repo DIR] [--dry-run] [--force-unlock] [--json]
```

Use exact IDs from `backup list`; unknown or misspelled IDs return 404. Forget refuses the last recovery point unless you pass `--allow-empty`, and refuses a parent required by a retained incremental snapshot. Forget removes manifests; prune separately removes unused packs and rewrites packs with less than half their encoded content still live. Dry runs take the same repository lock and report the planned work. Text output starts with `dry run: nothing removed`.

These commands require the master daemon credential. Browser sessions cannot run them. Cleanup errors with selected, planned, or completed work include partial reports in the API problem's `backup_forget` or `backup_prune` field. The CLI prints the report in text or JSON before returning the cleanup error and exiting nonzero. Text lists selected and forgotten snapshot IDs, planned pack removals and rewrites, and removed pack names. Items absent from completed lists remain pending or unconfirmed; bytes written alone do not prove a rewrite completed. Planned bytes are not net savings, and cleanup does not securely erase content.

## Recover on a fresh installation

Use a temporary fresh home to run the recovery daemon, and restore into a separate directory. The target must be disjoint from that home and the backup repository.

```bash
DOCBANK_HOME=~/Recovery/control docbank daemon start
DOCBANK_HOME=~/Recovery/control docbank backup restore --repo ~/Backups/docbank --target ~/Recovery/vault
DOCBANK_HOME=~/Recovery/control docbank daemon stop
DOCBANK_HOME=~/Recovery/vault docbank info
```

Restore validates photo relationships and recorded coverage before publishing the database, verifies managed content, clears sessions, and leaves checkouts unbound. Keep the backup repository available until you've checked the restored vault.

## Verify repository integrity

```bash
docbank backup verify [<snapshot>] [--repo DIR] [--all] [--quick] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

With no snapshot argument, Docbank verifies the latest snapshot. Pass a
snapshot ID to check a historical snapshot, or `--all` to check every manifest.
A full verification performs these checks:

1. Read the repository indexes and pack footers.
2. Reconstruct the snapshot's JSONL metadata.
3. Read every referenced content object and verify its SHA-256 hash.
4. Check recorded totals against manifest attachment totals and report unsupported coverage versions. Restore separately recounts version-1 coverage from the staged database before publication.

Content shared by several selected snapshots is read only once during one
verification pass. Every finding is reported with the affected snapshot, and
the command exits non-zero if any problems were found.

`--quick` checks manifest statistics and coverage versions, indexes, pack structure, metadata, and logical
references without reading document content. Its `bytes_read` therefore still
includes metadata bytes. It is useful after each capture, but it does not prove
the storage medium still holds every content byte. Run full verification
regularly. `--jobs 1` avoids concurrent reads on spinning disks and
latency-sensitive network storage. The progress and JSON contracts match
`backup create`: progress is written to stderr, and `--json` suppresses
progress so stdout contains one typed report.

Snapshots include the folder tree, trash state, retained versions, ingest and
provenance records, watched-source cursors, tags, saved queries, and extraction
records. They also include source metadata, renditions, visual previews, and
stored embedding results. Restore rebuilds and validates these records without
calling processing providers. See
[Backup architecture](../architecture/backup.md#which-retained-records-must-round-trip)
for retained records and rebuildable indexes.

Snapshots keep photo suggestion dismissals and accepted-field decisions but omit suggestion text. After restore and fresh processing consent, only undecided fields are described again.

## Restore and prove a snapshot

```bash
docbank backup restore [<snapshot>] --target DIR [--repo DIR] [--overwrite]
                       [--jobs N] [--force-unlock]
                       [--store-map OWNER_PRIVATE_FILE]
                       [--progress auto|bar|plain] [--json]
```

Restore selects the latest snapshot by default. Pass a snapshot ID to recover a
historical point. The CLI resolves `--target` from its working directory before
sending an absolute server path to the daemon. The target must be separate from
both the running vault and the repository. Direct paths, parents, descendants,
and symlink aliases that overlap either one are rejected. Filesystem-identity
checks also reject differently cased or Unicode-normalized spellings that
identify the same tree on filesystems where those names are equivalent.

### How does restore protect the target?

Every restore pins the target directory and takes its vault-tree lock before
writing, including for a fresh or empty target. That excludes a second restore,
a restore to any ancestor or descendant, and a daemon rooted anywhere in the
same tree. Replacing the target pathname while restore is running cannot
redirect publication. A successful restore leaves the ordinary `vault.lock` as
part of the usable vault. A failed restore also leaves that advisory file in
place after releasing it. Retries ignore `vault.lock` when deciding whether the
target contains payload, and keeping the pathname avoids split-lock races
between old and newly created lock files.

A new or empty target needs no destructive flag. A non-empty target is refused
unless you pass `--overwrite`. Overwrite is a merge: files absent from the
snapshot remain in place. The old database and SQLite sidecars remain intact
until all repository content has been read and verified, the replacement
database passes `integrity_check`, and its logical statistics match the
manifest. Only then is the database published. Failure or cancellation before
publication leaves an existing database intact and creates no `docbank.db` for
a new target. If cleanup fails after publication, restore reports an error and
keeps the new database and its ownership marker. Earlier failures restore the
prior marker. An interrupted handoff is
reconciled when the vault is opened or the restore is retried, against a
durable fingerprint of the prior database and the validated identity of
whichever database was published. An unrelated file named `docbank.db` is never
opened for mutation to decide which side won.

### Where does restored content go?

By default, restore copies compatible repository packs, verifies them, writes
them durably, and records them as readable locations in the catalog. If a pack
or object exceeds Docbank's current storage policy, restore writes it as a
verified individual blob file instead. The result reports the loose count and
grouped fallback reasons. This is a representation choice, not an integrity
failure.

Individual files use the standalone daemon's compression policy, including
when restoring through the embedded API:
files of at least 4 KiB use zstd when it saves at least 10%. This includes
large files reconstructed from backup chunks. The restored database and host
extras remain ordinary files. Restoring an individual file temporarily needs
space for its uncompressed bytes and its compressed candidate; the unused
copy is removed when that file finishes. An overwrite restore reuses
existing verified files in their current encoding.

Snapshots also carry a non-secret `docbank-placement-v1` description of source
stores and the hashes held by each. Default restore places all verified content
in a fresh local primary store. It gives that store a new ID and ownership
epoch, the value used to distinguish one store owner from the next. It does not
inherit source paths, endpoints, credentials, buckets, bindings, or ownership
epochs.

`--store-map` maps source store IDs to binding profiles already loaded by the
daemon performing the restore. The TOML file must be an owner-private regular
file and may select a new empty namespace or a takeover. Mapped bytes are read
back independently before target authority is recorded. Unmapped bytes remain
local. A `remote_only` restore revokes primary catalog authority in the staged
database but leaves its physical staging files for garbage collection after
that database is published, so a failed overwrite cannot damage the existing
vault. Audited bytes also retain primary authority unless the mapping selects
both `remote_only` and `allow_audited_remote_only`. See
[Multi-store storage](storage.md#backup-and-restore) for the file format and
trust boundary.

The mapping path is resolved on the daemon host, not on the caller's host.
Mapping-file read and parse errors never include the file contents.

The restore API and explicit `repo` paths accept only loopback clients, because
the daemon opens and writes those server-local paths. A network client of a
daemon bound to a non-loopback address can create, list, and verify snapshots
in the configured `[backup] repo`; run restores on the daemon host, for example
with `docker exec`.

Backups exclude `config.toml`. Reconfigure processing profiles and other local
settings on the restored target before using them. Preserve the target's
storage bindings described below.

Restore also verifies that mapped filesystem stores do not overlap the live
source vault or backup repository. Before publication it gives an otherwise
empty target a minimal owner-private `config.toml` containing the mapped
binding profiles, so the restored vault can prove and read remote-only
authority after restart. An existing target configuration is preserved and
must already define the same mappings exactly.

### What does the restore report mean?

Interactive restore shows metadata, document, extras, SQLite integrity, and
manifest-statistics progress as separate stages. `--json` suppresses progress
and returns one report containing physical layout counts and the
`content_verified`, `sqlite_integrity`, and `manifest_stats` proof fields. The
final report also inspects the restored vault's physical inventory while its
target-tree coordination is still held. It reports loose files, live packed
blobs, pack count, and any logically dead packed bytes that still occupy
immutable pack files. Those bytes have not been reclaimed. Inspect and repack
the restored target, not the currently running source vault:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank storage status
DOCBANK_HOME=~/Restores/docbank-test docbank storage repack
```

Reading this inventory does not run maintenance. If restore succeeds but the
inventory read fails, the report still records a successful restore and adds
a `storage_warning`.

A successful report means the target is a complete vault. Restore does not
start it or replace the running vault with it. Inspect it under its own home
first:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank verify
DOCBANK_HOME=~/Restores/docbank-test docbank tree /
DOCBANK_HOME=~/Restores/docbank-test docbank daemon stop
```

## Backups move forward only

Restore with the release that wrote the backup or a newer one. Older releases
reject snapshots containing record kinds they do not understand instead of
restoring a vault without those records.

## Repository placement

Keep the repository outside `$DOCBANK_HOME`. It is independent archive state,
not a live-vault subdirectory. Its files are write-once, so a completed
repository is suitable for `rsync`, `rclone`, cloud-drive sync, filesystem
snapshots, or removable media. Sync after `backup create` completes. Never edit
repository packs, indexes, manifests, locks, or configuration by hand.
