---
title: Importing Documents
description: Bulk import semantics — recursion, idempotency, collision suffixing, and failure handling.
---

# Importing Documents

`docbank add` is the bulk-migration path for decades of accumulated
`Documents/`, Dropbox, and old-drive trees. It is designed to be run
repeatedly against the same sources without ever duplicating a document
or touching the originals.

## What an import does

For each regular file:

1. The content is hashed (SHA-256, streaming) and written to the blob
   store — durably, before any database row references it. Content that
   already exists in the vault is not written twice.
2. One database transaction creates the tree node, its stable revision-one
   `content_create` version, the blob authority, and provenance: the original
   filesystem path and modification time survive any later renames or moves.

Directory arguments walk recursively. The directory's basename becomes a
folder under `--dest`, and everything below keeps its relative structure:

```bash
docbank add ~/old-laptop/Documents --dest /archive
# → /archive/Documents/... mirrors the source tree
```

Trailing slashes and `./`-style paths are normalized; `add docs/` and
`add ./docs` behave identically to `add docs`.

An explicitly named source may be a symlink to a directory. This supports
ordinary platform layouts such as `~/Dropbox` on macOS: docbank resolves that
one root link, retains `Dropbox` as the virtual directory name, and records
provenance using the path the user supplied. Symlinks encountered *inside* the
tree remain skipped and reported, and an explicitly named symlink to a file is
not imported.

## Preflight a large tree

Inventory a source before Docbank opens any file content or changes the vault:

```bash
docbank add ~/Dropbox --preflight \
  --exclude .git \
  --exclude .Trash \
  --exclude project/cache
```

The report separates files currently eligible for packing (through 64 MiB),
larger files that will remain authoritative loose objects, and files above the
current format-v1 ingest ceiling. It also reports logical bytes, directory
count, skipped non-regular entries, filesystem errors, and the largest groups
by lowercase filename extension. Use `--json` for a structured, bounded report.

Preflight is metadata-only. In particular, it does not open cloud-provider
placeholder content merely to estimate the import. That avoids an inventory
silently hydrating an entire cloud tree, but it also means successful preflight
cannot promise that every file will remain readable when the later import
opens it. Re-run preflight after changing exclusions, then pass the exact same
`--exclude` flags to the real `docbank add` command.

Filesystem names and provenance paths must currently be valid UTF-8. On POSIX
filesystems that permit other byte sequences, preflight and ingest report each
such entry with an escaped, printable path; Docbank does not open or import it,
continues with the rest of the tree, and never alters the source.

A bare exclusion such as `.git` prunes that entry name wherever it occurs. A
relative path such as `project/cache` prunes that path and its descendants
within each supplied source. Exclusions do not use glob syntax. A pruned
directory counts as one excluded entry because Docbank deliberately does not
walk it to count hidden descendants. A trailing slash or leading `./` keeps a
single-component rule path-shaped: `cache` matches that name anywhere, whereas
`cache/` and `./cache` match only the source root's `cache` entry. Commas are
literal filename characters, not rule separators; pass `--exclude` repeatedly
for multiple rules.

## Follow a long import

Human-mode `docbank add` performs a metadata-only scan to establish file and
byte totals, then reports content-read progress while it imports:

```bash
docbank add ~/Dropbox --dest /archive --progress plain
```

`auto` (the default) draws a progress bar on a terminal and emits durable
periodic lines when stderr is redirected. `plain` always emits durable lines;
`bar` forces the redrawable form. Progress belongs on stderr and the terminal
summary belongs on stdout. Use `--json` to suppress progress and emit only the
machine-readable terminal report.

The scan totals are an estimate rather than a filesystem lock: a source may
change before Docbank opens it. Byte progress counts content actually read,
while a file counts as done only after its individual blob and metadata
operation returns. Interrupting the command cancels the daemon request. Files
that already completed remain authoritative and make a rerun converge; an
incomplete file never receives node authority.

## Idempotency: safe to re-run

Interrupted a 200,000-file import? Run the same command again. For each
source file, docbank walks the candidate names in the destination
directory — `report.pdf`, `report (2).pdf`, `report (3).pdf`, … — and:

- if any live candidate has the **same content**, the file is counted as
  `skipped` (already imported, even if a prior run imported it under a
  suffix);
- if all existing candidates have different content, the next free
  suffix is used;
- otherwise the first free candidate name is taken.

Re-runs therefore converge instead of duplicating. Identity is content,
not filename: the same bytes under two source names import as two nodes
sharing one stored blob but carrying distinct version UUIDs.

## Collisions

Two different files arriving at the same virtual name don't conflict —
the newcomer is suffixed (`scan.pdf` → `scan (2).pdf`). The provenance
record preserves where each one actually came from.

## Failures don't abort the batch

Unreadable files, permission errors, and non-regular files (symlinks,
sockets, devices) are recorded and reported at the end; the rest of the
import continues. A directory that can't be created in the tree (for
example, its virtual path collides with an existing file) skips that
subtree and continues with the next.

```
added: 4211  skipped: 12  failed: 2
failed: /src/broken.pdf: opening /src/broken.pdf: permission denied
failed: /src/link.pdf: not a regular file or directory (symlinks are skipped)
```

The exit code is non-zero when any file failed, so scripted migrations
can detect partial imports. A missing or unreadable top-level source is
reported the same way, and the command continues with the remaining
source arguments.

## Sources are read-only

Import never deletes or modifies source files, including a followed root
directory symlink. Delete originals yourself
once `docbank verify` and your own spot-checks satisfy you — the same
archive-first, delete-later posture msgvault takes with mailboxes.

## Remote API imports

Authenticated integrations can send one digest-checked file at a time through
`POST /api/v1/uploads`. The server requires the writer's SHA-256 and byte length,
computes both independently while streaming, and creates no node or blob
authority when either differs. See the [HTTP API](../architecture/http-api.md#addendum-post-uploads)
and [Agent Integration Guide](../agents/integration.md#create-and-ingest-safely)
for the exact contract.

!!! info "Planned — watched inboxes"
    The daemon will watch configured directories (scanner output, a "To File"
    folder) and import files automatically once they've been stable for a
    settle period, landing under `/inbox/<date>/` for later filing.
