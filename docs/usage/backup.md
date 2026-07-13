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
new manifest.

The current command surface initializes repositories, creates snapshots, and
lists them. Repository verification and user-facing restore are the next
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

Creation runs inside the one vault-owning daemon. It briefly holds the
operation gate while opening a deferred SQLite read transaction, then releases
the gate: normal writes resume into the WAL while JSONL and document content
stream from the pinned point-in-time view. Every loose or packed content stream
must reach verified EOF before its bytes are accepted into the repository. A
failure never publishes a partial snapshot manifest; rerun the command after
addressing the error.

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

## Repository placement

Keep the repository outside `$DOCBANK_HOME`. It is independent archive state,
not a live-vault subdirectory. Its files are write-once, making a completed
repository suitable for `rsync`, `rclone`, cloud-drive sync, filesystem
snapshots, or removable media. Sync after `backup create` completes; never edit
repository packs, indexes, manifests, locks, or configuration by hand.

!!! info "Planned — verification and restore commands"
    `docbank backup verify` and `docbank backup restore` are not exposed yet.
    Until they land, snapshot creation and listing are usable, but the CLI does
    not yet provide the complete recovery workflow. Manual filesystem snapshots
    remain documented under [Vault Lifecycle](lifecycle.md).
