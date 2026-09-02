---
title: Backup & Restore
description: Create incremental, verifiable snapshots in an immutable repository.
---

# Backup & Restore

Docbank snapshot repositories are append-only directories of immutable,
checksummed files. A snapshot contains the deterministic JSONL description of
the virtual tree plus every catalog-authorized document blob, whether its live
representation is loose or packed. Unchanged metadata and content are reused
across snapshots, so repeated captures add only new repository objects and a
new manifest. Metadata is a complete logical JSONL description per snapshot,
not a row-level delta: an unchanged description is reused by hash, while any
logical change stores one new compressed metadata object.

The command surface initializes repositories, creates incremental snapshots,
lists and verifies them, and restores a proved vault into a separate target.

!!! warning "Repositories are not encrypted"
    Snapshot metadata and document content are compressed but not encrypted.
    Protect the repository with filesystem permissions and encrypted storage,
    especially before placing it on removable or cloud-synchronized media.
    Snapshot pruning and repository retention commands are also not exposed yet.

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

Restart the daemon after changing `config.toml`; configuration is read only at
daemon startup.

## Initialize a repository

```bash
docbank backup init [--repo DIR] [--json]
```

Initialization creates the repository layout and its random identity. It
refuses an existing non-empty or already initialized destination rather than
silently adopting unrelated files. The repository must be configured or
supplied explicitly. A relative CLI `--repo` is resolved from the invoking
shell's working directory before it is sent to the daemon.

## Create a snapshot

```bash
docbank backup create [--repo DIR] [--tag LABEL] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

Creation runs inside the one vault-owning daemon. It briefly pauses mutations
while opening a deferred SQLite read transaction, then normal writes resume
into the WAL while JSONL and document content stream from the pinned
point-in-time view. GC, trash empty, verification, and packed-storage
maintenance queue until capture finishes so they cannot remove content the
snapshot still requires. Every loose or packed content stream must reach
verified EOF before its bytes are accepted into the repository. A failure never
publishes a partial snapshot manifest; rerun the command after addressing the
error.

During an interactive run, `--progress auto` draws an in-place bar for each
stage (freeze, metadata, attachments, and seal), including item and byte
counts when available. Redirected output uses throttled, newline-terminated
progress instead, so logs remain readable. Force either behavior with
`--progress bar` or `--progress plain`. Progress goes to stderr; `--json`
suppresses it and writes one snapshot object to stdout for automation.

`--jobs 1` serializes blob readers for repositories on spinning disks or NAS
storage. Zero uses Kit's CPU-based default. `--tag` is a free-form label shown
by `backup list`. `--force-unlock` is recovery for a known-dead repository lock,
not a way to override another running backup.

## List snapshots

```bash
docbank backup list [--repo DIR] [--json]
```

The table reports immutable snapshot ID, creation time, logical file/blob
counts, bytes newly added to the repository, and tag. `--json` returns the same
typed snapshot summaries as `GET /api/v1/backup/snapshots`, including the
metadata format and parent snapshot ID.

## Verify repository integrity

```bash
docbank backup verify [SNAPSHOT] [--repo DIR] [--all] [--quick] [--jobs N]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
```

With no snapshot argument, verification proves the latest snapshot. Pass an
immutable snapshot ID to prove one historical snapshot, or `--all` to prove
every manifest. A full verification resolves repository indexes and pack
footers, materializes the snapshot's JSONL metadata, reads and SHA-256 verifies
every referenced content object, and checks Docbank's recorded logical totals.
Content shared by several selected snapshots is read only once during one
verification pass. Every finding is reported with the affected snapshot, and
the command exits non-zero if any problems were found.

`--quick` checks manifests, indexes, pack structure, metadata, and logical
references without reading document content. Its `bytes_read` therefore still
includes metadata bytes. It is useful after each capture,
but it does not prove the storage medium has retained every content byte; run
full verification regularly. `--jobs 1` avoids concurrent reads on spinning
disks and latency-sensitive network storage. The progress and JSON contracts
match `backup create`: progress is written to stderr, and `--json` suppresses
progress so stdout contains one typed report.

Snapshots include the current virtual tree, trash state, stable content
versions, ingest/provenance metadata, watched-source cursors, tags, and
extraction records. Restoring rebuilds and validates that logical authority
before publishing the target.

## Restore and prove a snapshot

```bash
docbank backup restore [SNAPSHOT] --target DIR [--repo DIR] [--overwrite]
                       [--jobs N] [--force-unlock]
                       [--store-map OWNER_PRIVATE_FILE]
                       [--progress auto|bar|plain] [--json]
```

Restore selects the latest snapshot by default; pass an immutable snapshot ID
to recover a historical point. The CLI resolves `--target` from its working
directory before sending an absolute server path to the daemon. The target
must be separate from both the running vault and the immutable repository.
Direct paths, parents, descendants, and symlink aliases that overlap either
one are rejected. Filesystem-identity checks also reject differently cased or
Unicode-normalized spellings that identify the same tree on filesystems where
those names are equivalent.

Every restore pins the target directory and takes its vault-tree lock before
writing, including for a fresh or empty target. That excludes a second restore,
a restore to any ancestor or descendant, and a daemon rooted anywhere in the
same tree. Replacing the target pathname while restore is running cannot
redirect publication. A successful restore leaves the ordinary `vault.lock` as
part of the usable vault. A failed restore also retains that stable advisory
file after releasing it: retries ignore `vault.lock` when deciding whether the
target contains payload, and retaining the pathname avoids split-lock races
between old and newly created lock files.

A new or empty target needs no destructive flag. A non-empty target is refused
unless `--overwrite` is explicit. Overwrite is a merge: files absent from the
snapshot remain in place. The old database and SQLite sidecars remain intact
until all repository content has been read and verified, the replacement
database passes `integrity_check`, and its logical statistics match the
manifest. Only then is the database published. A failed or cancelled restore
does not publish `docbank.db` for a new target and does not replace an existing
database. The built-in primary's ownership marker follows the same boundary:
ordinary failures restore the prior marker, and an interrupted handoff is
reconciled against a durable fingerprint of the prior database and the
validated identity of whichever database was actually published when the vault
is opened or the restore is retried. An unrelated file named `docbank.db` is
never opened for mutation merely to decide which side won.

Compatible repository packs are copied, verified, durably published, and
granted catalog authority by default. A pack or object that exceeds Docbank's
current storage policy is restored as a verified loose blob instead; the
result reports the loose count and grouped fallback reasons. This is a
representation choice, not an integrity failure.

Snapshots also carry a non-secret `docbank-placement-v1` description of source
store identities and per-hash placement. Default restore deliberately ignores
that topology and rebuilds every verified blob under a fresh local primary
store ID and ownership epoch. No source path, endpoint, credential, bucket,
binding, or ownership epoch is inherited.

`--store-map` explicitly maps source store IDs to binding profiles already
loaded by the daemon performing the restore. The TOML file must be an
owner-private regular file and may select a new empty namespace or an explicit
takeover. Mapped bytes are independently read back before target authority is
recorded. Unmapped bytes remain local. A `remote_only` restore revokes primary
catalog authority in the staged database but leaves its physical staging files
for garbage collection after that database is published, so a failed overwrite
cannot damage the existing vault. Audited bytes also retain primary authority
unless the mapping explicitly selects both `remote_only` and
`allow_audited_remote_only`. See
[Multi-store Storage](storage.md#backup-and-restore) for the file format and
trust boundary.

Restore also verifies that mapped filesystem stores do not overlap the live
source vault or backup repository. Before publication it gives an otherwise
empty target a minimal owner-private `config.toml` containing the mapped
binding profiles, so the restored vault can prove and read remote-only
authority after restart. An existing target configuration is preserved and
must already define the same mappings exactly.

Interactive restore shows metadata, document, extras, SQLite integrity, and
manifest-statistics progress as separate stages.
`--json` suppresses progress and returns one report containing physical layout
counts and explicit `content_verified`, `sqlite_integrity`, and
`manifest_stats` proof fields. The terminal report also inspects the restored
vault's physical inventory while its target-tree coordination is still held.
It reports loose files, live packed blobs, pack count, and any logically dead
packed bytes that still occupy immutable pack files. Those bytes have not been
reclaimed. Inspect and repack the restored target explicitly, rather than the
currently running source vault:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank storage status
DOCBANK_HOME=~/Restores/docbank-test docbank storage repack
```

This reporting never performs maintenance automatically. If the proved
restore succeeded but this ancillary inventory cannot be read, the report
remains successful and carries a `storage_warning` instead of claiming that
the committed restore failed.

A successful report means the target is a complete vault, but it does not
automatically replace or start it. Inspect it under its own home first:

```bash
DOCBANK_HOME=~/Restores/docbank-test docbank verify
DOCBANK_HOME=~/Restores/docbank-test docbank tree /
DOCBANK_HOME=~/Restores/docbank-test docbank daemon stop
```

## Backups move forward only

A backup records every kind of authority the writing release knows about. An
older release refuses a snapshot that contains a record kind it does not
understand rather than restoring a vault with silent gaps, so restore with the
release that wrote the backup or a newer one.

## Repository placement

Keep the repository outside `$DOCBANK_HOME`. It is independent archive state,
not a live-vault subdirectory. Its files are write-once, making a completed
repository suitable for `rsync`, `rclone`, cloud-drive sync, filesystem
snapshots, or removable media. Sync after `backup create` completes; never edit
repository packs, indexes, manifests, locks, or configuration by hand.
