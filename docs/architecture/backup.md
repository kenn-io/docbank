---
title: Backup
description: Current manual snapshots and the staged Kit-backed backup integration.
---

# Backup

Docbank has no built-in backup commands in the current release. A coherent
manual filesystem snapshot requires stopping the daemon before copying the
vault so `docbank.db` and the blob catalog cannot change during the copy. See
[Vault Lifecycle](../usage/lifecycle.md#take-a-coherent-manual-snapshot).

The essential archive is the database plus `blobs/`. Configuration is useful
to retain when customized; logs, locks, and runtime records are not archive
state. A restored copy is not trusted until `docbank verify` succeeds.

## Kit integration status

The internal `backupapp` adapter now supplies Kit with docbank's frozen logical
view: every authoritative `blobs` row, representation-neutral fidelity stats,
canonical restore paths, and mixed loose/packed content reads. Integration
tests prove loose create/verify/restore and capture directly from a packed live
store. Physical pack tables are excluded from fidelity stats so a restore may
choose a different representation without changing the archive.

Docbank also has a deterministic, versioned JSONL representation of its logical
metadata. It contains the complete virtual directory tree and file records,
including stable IDs, content hashes, timestamps, trash coordinates, prior
versions, ingest provenance, tags, and extracted text. It intentionally omits
FTS rows and physical pack mappings: search indexes are rebuilt by importing
nodes, while restore grants physical authority only after content has been
verified and published. Import targets must be fresh current-schema databases;
a malformed or referentially incomplete stream leaves the pristine target
unchanged.

Capture reads loose and packed blobs through Kit's bounded-memory stream. The
archive may grant authority to copied bytes only after terminal EOF verifies
their stored framing, decoded length, and SHA-256 identity; opening a stream or
closing it early is not a successful copy.

Snapshots taken from a packed vault retain source pack metadata in their
captured database. Docbank therefore fails closed if such a snapshot is sent
through Kit's loose-only restore path: loose files plus stale pack authority
would make the restored vault unreadable. Docbank's restore wrapper inseparably
owns the application adapter and packed target: it publishes verified repository
packs into the target and atomically replaces all captured pack records and
mappings before the staged vault becomes visible. Integration coverage opens
every restored blob through the same mixed store used by a live vault.

!!! info "Planned — Phase 4"
    Command/API orchestration has not landed. The completed capture and restore
    adapters are internal and do not make any `docbank backup` command
    available yet.

    Kit currently captures the frozen SQLite database as its metadata artifact.
    The next integration step is to make the logical JSONL stream the archive's
    application metadata and rebuild a fresh current-schema database during
    restore. Until that lands, the JSONL codec alone does not change the bytes
    in a Kit snapshot.

    The planned command family is `docbank backup init|create|list|verify|restore`.
    Exact flags will enter the CLI reference only when implemented.

    Backup reachability will intentionally be broader than GC reachability:
    every `blobs` row will be captured, including a row that has become a GC
    candidate but has not yet been reclaimed. This preserves the deletion
    pipeline's regret window inside the snapshot.

## Boundary with packed storage

Backup and live packed storage share Kit's physical formats and verification
primitives, but docbank remains responsible for which catalog rows belong in a
snapshot. Kit does not infer application liveness or reach into docbank SQL.
