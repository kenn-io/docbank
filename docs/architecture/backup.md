---
title: Backup
description: Current manual snapshot requirements and the planned Kit-backed backup integration.
---

# Backup

Docbank has no built-in backup commands in the current release. A coherent
manual filesystem snapshot requires stopping the daemon before copying the
vault so `docbank.db` and the blob catalog cannot change during the copy. See
[Vault Lifecycle](../usage/lifecycle.md#take-a-coherent-manual-snapshot).

The essential archive is the database plus `blobs/`. Configuration is useful
to retain when customized; logs, locks, and runtime records are not archive
state. A restored copy is not trusted until `docbank verify` succeeds.

## Planned Kit integration

!!! info "Planned — Phase 4"
    Docbank will integrate `go.kenn.io/kit/backup`, the shared engine already
    used by msgvault. The application adapter will own WAL checkpointing and a
    pinned read transaction, content enumeration from docbank's `blobs` rows,
    fidelity statistics, and exclusions for logs and staging files.

    Snapshots will contain page-diffed SQLite state plus only-new content in
    sealed, verifiable packs. Restore will materialize `docbank.db` and
    `blobs/`, then compare application statistics with the manifest before the
    result is accepted.

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
