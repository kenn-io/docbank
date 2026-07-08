---
title: CLI Reference
description: Every docbank command, flag, output format, and error behavior.
---

# CLI Reference

All commands operate on the vault at `~/.docbank` (override with
`DOCBANK_HOME`; see [Configuration](configuration.md)). Errors go to
stderr and produce a non-zero exit code. Virtual paths are absolute,
`/`-separated, and case-sensitive.

Commands hold a shared inter-process lock while running, so concurrent
docbank invocations are safe; `gc` takes the lock exclusively and briefly
blocks other commands. See
[Concurrency & Locking](architecture/locking.md).

## docbank add

```
docbank add <path>... [--dest <virtual-dir>]
```

Imports files or directory trees into the vault. Sources are copied,
never modified or deleted.

| Flag | Default | Meaning |
|------|---------|---------|
| `--dest` | `/inbox` | Virtual destination directory; created (with parents) if missing |

- A directory argument imports recursively: its basename becomes a
  directory under `--dest` and relative structure is preserved.
- Symlinks and other non-regular files are skipped and reported as
  failures; they do not abort the run.
- Name collisions with different content auto-suffix:
  `report.pdf` → `report (2).pdf`.
- Re-running an import converges: a file whose content already exists
  under any candidate name in the destination is skipped, so an
  interrupted bulk import can simply be re-run. See
  [Importing Documents](usage/importing.md).

Output is a one-line summary plus one stderr line per failed file:

```
added: 12  skipped: 3  failed: 1
failed: /src/broken.pdf: opening /src/broken.pdf: permission denied
```

Exit is non-zero if any file failed. A missing or unreadable top-level
source argument aborts the run with an error (per-file failures inside a
directory tree do not).

## docbank ls

```
docbank ls [path]
```

Lists a virtual directory (default `/`). Columns: `ID`, `KIND`
(`dir`/`file`), `SIZE` (bytes; 0 for directories), `MODIFIED` (UTC,
RFC 3339), `NAME`. Fails with `not a directory` when the path names a
file.

## docbank tree

```
docbank tree [path]
```

Prints the subtree rooted at `path` (default `/`), two-space indented,
each entry suffixed with its node ID in brackets. Fails without output if
`path` names a file.

## docbank cat

```
docbank cat <path>
```

Streams the file's stored bytes to stdout. Fails with `not a file` for
directories.

## docbank mv

```
docbank mv <src-path> <dest-path>
```

Moves or renames a node. Metadata only — bytes never move. The
destination is interpreted like POSIX `mv`:

- If `dest-path` names an existing directory, the source moves **into**
  it, keeping its name.
- Otherwise `dest-path`'s parent must exist, and its basename becomes
  the new name (rename, or move-and-rename).
- If `dest-path` names an existing **file**, the move fails with
  `name already exists` — docbank never overwrites.

Directory moves carry the whole subtree. A move that would place a
directory under its own descendant fails with `move would create a
cycle`. On success prints `moved [<id>] <new-path>`.

## docbank rm

```
docbank rm <path>
```

Soft-deletes: moves the node — and, for a directory, its entire subtree —
to the trash. Nothing is permanently removed and no bytes are reclaimed.
The freed name is immediately reusable. Prints:

```
trashed [15] /taxes/2024/return.pdf (restore with: docbank restore 15)
```

## docbank restore

```
docbank restore <id>
```

Returns a trashed node (by ID — see `docbank trash list`) to its original
location, re-suffixing its name if a live node now occupies it. If the
original parent directory was itself permanently deleted, the node is
restored under `/`. Prints `restored [<id>] <path>`.

## docbank search

```
docbank search <query>...
```

Full-text search over live node names (FTS5). Every whitespace-separated
term is matched as a prefix; FTS operator syntax in the query is escaped,
not interpreted. Results are ranked best-first (BM25, ties broken by
name) and capped at 50. Output columns are `ID` and `PATH`; no matches
prints `no matches`.

!!! info "Planned — Phase 2"
    Search currently covers node names only. Extracted document text
    (PDF text layers, office formats) joins the index when the extraction
    workers land. See [Searching](usage/searching.md).

## docbank trash

```
docbank trash list
docbank trash empty [--older-than <age>]
```

`list` shows restorable trashed nodes: `ID`, `TRASHED AT`, `NAME`. Only
trash roots are listed — trashing a directory produces one entry, and
restoring it brings the whole subtree back.

`empty` permanently deletes trashed nodes; their blobs become `gc`
candidates unless referenced elsewhere. `--older-than` accepts Go
durations (`12h`, `30m`) plus a day suffix (`30d`); negative ages are
rejected. Without the flag, everything in the trash is deleted. Prints
`deleted <n> trashed node(s)`.

## docbank gc

```
docbank gc [--run]
```

Garbage-collects unreachable blobs — content referenced by no live node,
no trashed node, and no recorded prior version. Dry-run by default:

```
3 candidate blob(s), 1204882 byte(s) reclaimable
dry run — pass --run to delete
```

With `--run`, blob files are deleted first, then their metadata rows;
prints `reclaimed <n> blob(s), <bytes> byte(s)`. A crash mid-GC leaves
rows without files, which the next `gc --run` reconciles and `verify`
reports in the meantime. `gc` holds the vault lock exclusively, so it
never races a concurrent import.

## docbank verify

```
docbank verify
```

Re-hashes every stored blob against its recorded SHA-256. Reports one
line per problem — `missing: <hash>` (row without file),
`corrupt: <hash>` (hash mismatch), or `unreadable: <hash>` (I/O error) —
then a summary `<n> blob(s) ok, <n> problem(s)`. Exits non-zero if any
problem was found.

## docbank version

```
docbank --version
```

Prints the build version and commit (`dev (unknown)` for untagged local
builds; release builds inject both via `-ldflags`).

## Planned commands

!!! info "Planned — later phases"
    The following are designed but not yet implemented; they will appear
    here with exact semantics when they ship. `docbank edit` and
    `docbank versions` (Phase 2, [Editing & Versions](architecture/editing-and-versions.md));
    `docbank serve` and `docbank extract` (Phase 2,
    [HTTP API](architecture/http-api.md)); `docbank tui` (Phase 3);
    `docbank backup init|create|list|verify|restore` (Phase 4,
    [Backup](architecture/backup.md)).
