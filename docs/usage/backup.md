---
title: Backup
description: Create incremental, verifiable snapshots in an immutable repository.
---

# Backup

Docbank snapshot repositories are append-only directories of immutable,
checksummed files. A snapshot contains the deterministic JSONL description of
the virtual tree plus every catalog-authorized document blob, whether its live
representation is loose or packed. Unchanged metadata and content are reused
across snapshots, so repeated captures add only new repository objects and a
new manifest. Metadata is a complete logical JSONL description per snapshot,
not a row-level delta: an unchanged description is reused by hash, while any
logical change stores one new compressed metadata object.

The current command surface initializes repositories, creates snapshots,
lists them, and verifies repository integrity. User-facing restore is the next
backup slice; the underlying verified restore engine is already integrated.

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

## Repository placement

Keep the repository outside `$DOCBANK_HOME`. It is independent archive state,
not a live-vault subdirectory. Its files are write-once, making a completed
repository suitable for `rsync`, `rclone`, cloud-drive sync, filesystem
snapshots, or removable media. Sync after `backup create` completes; never edit
repository packs, indexes, manifests, locks, or configuration by hand.

!!! info "Planned — restore command"
    `docbank backup restore` is not exposed yet. Until it lands, the built-in
    workflow can capture and independently verify snapshots but cannot
    materialize one through the user-facing CLI. Manual filesystem snapshots
    remain documented under [Vault Lifecycle](lifecycle.md).
