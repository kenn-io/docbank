---
title: Quickstart
description: A ten-minute tour of the docbank CLI.
---

# Quickstart

This walkthrough exercises every core command against a scratch vault.
Point `DOCBANK_HOME` at a temporary directory so you can experiment
freely:

```bash
export DOCBANK_HOME=$(mktemp -d)
```

## Import some documents

`add` copies files into the vault. Sources are never modified or deleted.

```bash
docbank add ~/Documents/taxes --dest /taxes
```

```
added: 214  skipped: 0  failed: 0
```

Directories import recursively: the directory's own name becomes a
folder under `--dest`, and the relative structure underneath is
preserved. Without `--dest`, files land in `/inbox`.

Re-running the same import is safe — already-imported files (matched by
content, not name) are skipped:

```
added: 0  skipped: 214  failed: 0
```

## Browse the tree

```bash
docbank ls /taxes
```

```
ID   KIND  SIZE    MODIFIED                        NAME
14   dir   0       2026-07-06T21:14:03.2211Z       2024
102  dir   0       2026-07-06T21:14:05.9871Z       2025
231  file  48211   2026-07-06T21:14:08.1298Z       checklist.pdf
```

`tree` prints the whole hierarchy with node IDs:

```bash
docbank tree /taxes
```

```
/taxes
  2024  [14]
    return.pdf  [15]
  2025  [102]
    return.pdf  [103]
  checklist.pdf  [231]
```

`cat` streams a file's bytes to stdout:

```bash
docbank cat /taxes/checklist.pdf > /tmp/checklist.pdf
```

## Reorganize

Moves and renames are metadata-only; the stored bytes never move.

```bash
# Rename in place (destination doesn't exist; its parent does)
docbank mv /taxes/checklist.pdf /taxes/filing-checklist.pdf

# Move into an existing directory, keeping the name
docbank mv /taxes/filing-checklist.pdf /taxes/2025
```

```
moved [231] /taxes/2025/filing-checklist.pdf
```

## Search

Names are indexed with SQLite FTS5; every term is a prefix match:

```bash
docbank search tax check
```

```
ID   PATH
231  /taxes/2025/filing-checklist.pdf
```

## Trash and recovery

`rm` is soft deletion — the node (and its subtree, for directories) moves
to the trash and its name becomes reusable:

```bash
docbank rm /taxes/2024/return.pdf
```

```
trashed [15] /taxes/2024/return.pdf (restore with: docbank restore 15)
```

```bash
docbank trash list        # what's recoverable
docbank restore 15        # put it back where it was
docbank trash empty --older-than 30d   # permanently delete old trash
```

## Reclaim and verify

Emptying the trash deletes tree entries, but the underlying bytes remain
until you garbage-collect. `gc` is a dry run by default:

```bash
docbank gc
```

```
3 candidate blob(s), 1204882 byte(s) reclaimable
dry run — pass --run to delete
```

```bash
docbank gc --run
docbank verify            # re-hash every stored blob
```

```
211 blob(s) ok, 0 problem(s)
```

That's the complete Phase 1 surface. See the
[CLI Reference](cli-reference.md) for exact semantics of every command.
