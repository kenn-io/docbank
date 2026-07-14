---
title: CLI Reference
description: Every docbank command, flag, output format, and error behavior.
---

# CLI Reference

All commands operate on the vault at `~/.docbank` (override with
`DOCBANK_HOME`; see [Configuration](configuration.md)). Errors go to
stderr and produce a non-zero exit code. Virtual paths are absolute,
`/`-separated, and case-sensitive.

Every data command below (`add`, `ls`, `tree`, `cat`, `mv`, `rm`,
`restore`, `search`, `trash`, `gc`, `verify`) talks to the `docbank`
daemon over its HTTP API rather than opening the vault itself; if none
is running, the command auto-starts one in the background. `docbank
daemon status` and `docbank daemon stop` never auto-start. See
[Daemon](architecture/daemon.md) and
[Concurrency & Locking](architecture/locking.md).

## docbank add

```
docbank add <path>... [--dest <virtual-dir>] [--exclude <rule>]... [--progress auto|bar|plain] [--json]
docbank add <path>... --preflight [--exclude <rule>]... [--json]
```

Imports files or directory trees into the vault. Sources are copied,
never modified or deleted.

| Flag | Default | Meaning |
|------|---------|---------|
| `--dest` | `/inbox` | Virtual destination directory; created (with parents) if missing |
| `--exclude` | none | Prune a matching entry name anywhere, or a relative path within each source; repeatable |
| `--preflight` | false | Inventory source metadata without opening file content or changing the vault |
| `--json` | false | Emit only the terminal preflight or ingest report as JSON; suppress progress |
| `--progress` | `auto` | Human ingest progress: `auto`, `bar`, or durable `plain` lines |

- A directory argument imports recursively: its basename becomes a
  directory under `--dest` and relative structure is preserved.
- An explicitly named symlink to a directory is followed as the import root;
  its supplied basename and provenance spelling are retained. Symlinks inside
  that tree, symlinks to files, and other non-regular files are skipped and
  reported as failures; they do not abort the run.
- Name collisions with different content auto-suffix:
  `report.pdf` → `report (2).pdf`.
- Re-running an import converges: a file whose content already exists
  under any candidate name in the destination is skipped, so an
  interrupted bulk import can simply be re-run. See
  [Importing Documents](usage/importing.md).

Run `--preflight` before a large import. It reports regular-file and directory
counts, logical bytes, pack-eligible files, larger loose-only files, files over
the ingest ceiling, exclusions, skipped non-regular entries, filesystem errors,
and the largest extension groups. The scan reads filesystem metadata only: it
does not open cloud placeholders, create the destination, record an ingest, or
write blobs. `--json` retains a bounded set of detailed findings and file-type
groups for agents and scripts.

Exclusion rules are deliberately simple and shared by preflight and import. A
bare entry name such as `.git` or `node_modules` matches at any depth. A path
containing `/`, such as `project/cache`, matches that relative path and its
descendants within every supplied source. Rules are not shell globs and must be
relative; absolute paths and `..` escapes are rejected. Rule form is preserved:
`cache` is a name at any depth, while `cache/` and `./cache` mean only the
root-relative `cache` entry. Each `--exclude` value is literal and commas are
ordinary filename characters; repeat the flag to supply multiple rules.

An ordinary import first scans source metadata for file and byte totals, then
shows ingest progress on stderr. `auto` uses a redrawable bar on a terminal and
durable periodic lines when redirected; `--progress plain` forces durable
lines. The scan is advisory because sources may change before they are opened.
The command ends with a one-line stdout summary plus one stderr line per failed
file:

```
added: 12  skipped: 3  excluded: 2  failed: 1
failed: /src/broken.pdf: opening /src/broken.pdf: permission denied
```

Exit is non-zero if any file failed. A missing or unreadable top-level
source is reported as a failure and the command continues with remaining
source arguments, just as it does for failures inside a directory tree.
`--json` suppresses progress and returns the same terminal report shape as the
HTTP JSON endpoint, so stdout remains safe for automation.

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

There is no hard-delete flag. GC cannot collect a trashed document because the
trash entry remains a restorable reference. Permanent metadata deletion,
unreachable-content collection, and packed-space reclamation are the separate
`trash empty --run`, `gc --run`, and `storage repack` operations.

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
docbank search <query>... [--limit <n>]
```

Full-text search over live node names (FTS5). Every whitespace-separated
term is matched as a prefix; FTS operator syntax in the query is escaped,
not interpreted. Results are ranked best-first (BM25, ties broken by
name). The default limit is 50 and `--limit` accepts 1–1000. When more
matches exist, the command says that the result is truncated rather than
silently implying completeness. Output columns are `ID` and `PATH`; no
matches prints `no matches`.

!!! info "Planned — Phase 2b"
    Search currently covers node names only. Extracted document text
    (PDF text layers, office formats) joins the index when the extraction
    workers land. See [Searching](usage/searching.md).

## docbank trash

```
docbank trash list
docbank trash empty [--older-than <age>] [--run]
```

`list` shows restorable trashed nodes: `ID`, `TRASHED AT`, `NAME`. Only
trash roots are listed — trashing a directory produces one entry, and
restoring it brings the whole subtree back.

`empty` reports how many trash roots are eligible but does not delete by
default. Pass `--run` to permanently delete them; their blobs then become
`gc` candidates unless referenced elsewhere. `--older-than` accepts Go
durations (`12h`, `30m`) plus a day suffix (`30d`); negative ages are
rejected. Without that filter, every trash root is eligible.

## docbank gc

```
docbank gc [--run]
```

Garbage-collects unreachable blobs — content referenced by no live node,
no trashed node, and no recorded prior version. Dry-run by default:

```
3 candidate blob(s), 0 untracked file(s), 1204882 loose byte(s) reclaimable
dry run — pass --run to delete
```

Packed candidates are reported separately as stored bytes pending repack;
removing their catalog authority does not claim that immutable pack space was
already reclaimed. With `--run`, loose blob files are deleted first, then their
metadata rows; output separately reports removed blob records, reclaimed loose
files, and reclaimed bytes. A crash mid-GC leaves
rows without files, which the next `gc --run` reconciles and `verify`
reports in the meantime. The daemon's maintenance gate holds off
concurrent mutations while `gc --run` runs, so it never races a
concurrent import (see [Concurrency & Locking](architecture/locking.md)).
GC does not invoke repack, and no automatic GC/repack scheduler exists today.

## docbank storage status

```
docbank storage status [--json]
```

Reports the daemon's physical storage inventory: loose blob count and bytes,
live packed blobs and their stored/raw bytes, pack count, and immutable packed
bytes pending repack. The command is read-only. `--json` emits the same fields
as the authenticated `GET /api/v1/storage` endpoint.

## docbank storage pack

```
docbank storage pack [--max-bytes <bytes>] [--json]
```

Explicitly converts authorized loose blobs into immutable Kit pack files. The
operation runs through the authenticated daemon and holds the vault maintenance
gate; reads remain available, while imports and other mutations wait. Packing
does not change document identity or blob read authority, and mixed loose and
packed storage remains valid after an interruption.

`--max-bytes` is a soft raw-byte work budget. The blob that crosses the budget
is committed before the operation stops, and output says the budget was
exhausted. Check `storage status` and rerun if loose blobs remain; crossing the
budget does not itself prove that more eligible work exists. Zero (the default)
is unlimited. `--json` includes packing, repair, deferral, and reconciliation
counters from the shared Kit lifecycle engine.

## docbank storage repack

```
docbank storage repack [--min-age <duration>] [--min-dead-bytes <bytes>]
                       [--max-bytes <bytes>] [--json]
```

Rewrites eligible sparse packs with only their live blobs, atomically changes
catalog authority, and retires the old immutable pack files after active
readers release them. Packs with no live mappings are retired regardless of
age. A partially live pack is eligible when at most half its entries remain and
it satisfies both selection thresholds. Defaults are `--min-age 24h` and
`--min-dead-bytes 8388608`; use explicit smaller positive values for immediate
manual compaction.

`--max-bytes` is a soft live raw-byte budget. Zero is unlimited and makes a
source-content error fail the operation immediately; a positive budget lets
Kit continue with independent eligible source packs and return their combined
errors after committed work. The report's `bytes_repacked` is live raw content
rewritten, not a claim about filesystem bytes reclaimed. Compare `storage
status` before and after when exact inventory change matters.

Pack retirement can be deferred when another process holds a source pack open,
most commonly through a Windows handle that does not permit deletion. The
repack response then uses `pack_retirement_deferred`: replacement catalog
authority has already committed, so do not restore the old mapping or assume
the rewrite rolled back. Release the external file lock and run `docbank
storage pack`; its reconciliation pass removes the orphaned source pack.

## docbank verify

```
docbank verify
```

Re-hashes every stored blob against its recorded SHA-256. Reports one
line per problem — `missing: <hash>` (row without file),
`corrupt: <hash>` (hash mismatch), or `unreadable: <hash>` (I/O error) —
then a summary `<n> blob(s) ok, <n> problem(s)`. Exits non-zero if any
problem was found.

## docbank backup

```text
docbank backup init [--repo <dir>] [--json]
docbank backup create [--repo <dir>] [--tag <label>] [--jobs <n>]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
docbank backup list [--repo <dir>] [--json]
docbank backup verify [snapshot] [--repo <dir>] [--all] [--quick] [--jobs <n>]
                      [--force-unlock] [--progress auto|bar|plain] [--json]
docbank backup restore [snapshot] --target <dir> [--repo <dir>] [--overwrite]
                       [--jobs <n>] [--force-unlock]
                       [--progress auto|bar|plain] [--json]
```

Initializes an immutable Kit repository, captures a verified JSONL-native
snapshot through the daemon, lists snapshot history, independently proves
repository integrity, and restores a proved vault. `--repo` overrides
`[backup] repo`; one of them is required. `create` briefly quiesces mutations
only while pinning its logical view, then streams loose or packed content while
normal daemon work resumes. `--jobs 1` serializes repository readers;
`--force-unlock` is only for a repository lock whose owner is known to be gone.
`create` and `verify` draw per-stage progress bars on a terminal and durable
progress lines when redirected; `--progress` can force either form. `verify`
checks the latest snapshot by default, one named snapshot positionally, or all
snapshots with `--all`; `--quick` skips content reads. `restore` targets the
latest snapshot by default and requires a separate `--target`; non-empty
targets require `--overwrite`, which merges rather than clearing unrelated
files. Compatible content is restored packed, with verified loose fallbacks
reported explicitly. Every subcommand supports typed `--json` output;
long-running operations suppress progress in that mode. See
[Backup](usage/backup.md).

## docbank daemon

```
docbank daemon run
docbank daemon start
docbank daemon status [--json]
docbank daemon restart
docbank daemon stop
```

`daemon run` runs the daemon in the foreground, logging to stderr, until
signaled or stopped; it's usually invoked by `daemon start` in the
background, and is useful directly for debugging. `daemon start` spawns
it detached in the background, logging JSON to `$DOCBANK_HOME/logs/`.
`daemon status` reports whether a daemon is running (pid, address,
version, uptime) without starting one; `--json` emits `{"running": bool,
"pid", "address", "version", "started_at"}` for agents. `daemon restart`
stops the daemon if one is running (tolerating it not already running),
then starts it again, printing `restarted: ...` or `started (was not
running): ...` accordingly. `daemon stop` gracefully stops the running
daemon (or prints `no daemon running`) without starting one. Every data
command auto-starts a daemon if none is running — `daemon start` exists
for explicit control (long-running background use, inspecting logs
before running commands). `daemon start`, `daemon restart`, and
auto-start all converge the same way: a running daemon whose version or
API protocol does not match the invoking binary is stopped and replaced
(printed as `replaced daemon <old> (pid N) with <new>: ...`), so after
any of them succeeds, the one running daemon is current. See
[Daemon](architecture/daemon.md).

## docbank update

```
docbank update [--check] [--yes] [--force]
```

Checks GitHub for a newer release and, unless `--check`, installs it:
stops a running daemon, replaces the binary, and restarts the daemon
from the new executable (rolling back to a restart of the old daemon on
install failure). `--check` prints the current and latest versions and
stops there. `--yes` skips the install confirmation prompt (required in
non-interactive use, since there is no default without a terminal to
prompt on). `--force` bypasses the cached check (release metadata is
refetched) and allows replacing an unversioned dev build; it does not
reinstall a release that is already current. Refuses to install a
release with no published SHA256 checksum.

## docbank openapi

```
docbank openapi [--json]
```

Prints the HTTP API's OpenAPI document — YAML by default, `--json` for
JSON. Needs no running daemon and no vault: routes are registered
against an offline server instance and never invoked. For agents and
API client generation; see [HTTP API](architecture/http-api.md).

## docbank version

```
docbank --version
```

Prints the build version and commit (`dev (unknown)` for untagged local
builds; release builds inject both via `-ldflags`).

## Environment variables

`DOCBANK_HOME` selects the vault (see [Configuration](configuration.md)).
`DOCBANK_LOG_LEVEL` sets the daemon's log level (`debug`, `info`,
`warn`, `error`; default `info`) for both `docbank daemon run` and
background-spawned daemons.

## Planned commands

!!! info "Planned — later phases"
    The following are designed but not yet implemented; they will appear
    here with exact semantics when they ship. `docbank edit` and
    `docbank versions` (Phase 2b, [Editing & Versions](architecture/editing-and-versions.md));
    `docbank extract` (Phase 2b, [HTTP API](architecture/http-api.md));
    and `docbank tui` (Phase 3).
