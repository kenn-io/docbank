---
title: Backup
description: Verifiable, incremental backups via the shared kit engine.
---

# Backup

!!! info "Planned — Phase 4"
    The backup engine itself exists and is battle-tested — it's
    msgvault's, extracted into `go.kenn.io/kit` precisely so docbank
    could reuse it — but docbank's wiring (`internal/backupapp` and the
    `docbank backup` commands) is not yet implemented.

## The engine

docbank reuses `kit/backup` and `kit/pack`: page-diffed SQLite snapshots
plus only-new content files packed into sealed, verifiable packs.
Incremental by construction — a snapshot after adding ten documents
captures ten blobs and the changed database pages, nothing else. The
repository format, manifest discipline, and verification model are
documented in msgvault's
[backup format specification](https://msgvault.io/architecture/backup-format/);
docbank inherits them wholesale.

The engine is generalized around an application seam (`backup.App`):
docbank provides freeze (WAL-checkpoint + pinned read transaction),
content enumeration (every row in `blobs`), stats for the fidelity
proof (node/blob/tag counts and date range), and exclusions (`logs/`,
`blobs/tmp/`). Restore materializes `docbank.db` + `blobs/` and proves
the restored stats byte-match the manifest.

## Commands

Mirroring msgvault:

```
docbank backup init <repo-path>
docbank backup create
docbank backup list
docbank backup verify [--snapshot <id>]
docbank backup restore <snapshot> <dest>
```

A NAS-mounted directory is the expected repo target; any path works.

## Backup reachability ≠ GC reachability

Deliberately, backup captures **every** `blobs` row — including blobs
that are already GC candidates (for example, after `trash empty` but
before `gc --run`). A backup taken mid-deletion-pipeline preserves the
regret window; candidates age out of new snapshots only after GC
actually reclaims them. The rule composes with
[editing](editing-and-versions.md) the obvious way: prior versions are
reachable rows, so backups always include the full edit history that
exists at snapshot time.
