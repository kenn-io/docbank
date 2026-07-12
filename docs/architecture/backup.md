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

!!! info "Planned — Phase 4"
    Command/API orchestration and packed catalog publication on restore have
    not landed. The completed adapter is internal and does not make any
    `docbank backup` command available yet.

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
