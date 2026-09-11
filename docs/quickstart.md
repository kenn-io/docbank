---
title: Quickstart
description: A ten-minute tour of the docbank CLI.
---

# Quickstart

Import documents, find and organize them, then test recovery in a temporary
vault. These examples use a Unix shell. Set `DOCBANK_HOME` first so the commands
use a scratch vault:

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

### The daemon

The first `add` command starts the daemon, the background process that owns
and reads the vault. Data commands start it automatically when needed. Use
`docbank daemon status` to inspect it and `docbank daemon stop` to stop it; it
also exits after a period of inactivity. See [Daemon](architecture/daemon.md)
for startup and lifetime rules.

Repeat the same import to see the completed files skipped. Docbank checks
content at the destination name and its collision suffixes; see
[Importing Documents](usage/importing.md) for the matching rules:

```
added: 0  skipped: 214  failed: 0
```

## Browse the tree

```bash
docbank ls /taxes
```

```
SELECTOR  KIND  SIZE   MODIFIED              NAME
id:14     dir   0      2026-07-06T21:14:03Z  2024
id:102    dir   0      2026-07-06T21:14:05Z  2025
id:231    file  48211  2026-07-06T21:14:08Z  checklist.pdf
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

For an interactive view of the same tree, open the terminal browser:

```bash
docbank tui
```

Use the arrow keys or `j`/`k` to select a document, Enter to open a directory,
and `/` to search names and extracted text. Press `i` to inspect the document's
IDs, revision, and content hash, or `a` for its permanent audit timeline.
Press `x` to review moving the selected revision to recoverable trash, or `T`
to browse and restore trash roots. Press `O` for the read-only storage inventory
and configured backup recovery points. The TUI does not expose permanent
deletion or maintenance mutations. See the
[interactive terminal browser](usage/tui.md) guide for the complete key map.

`cat` streams a file's bytes to stdout. To save a local file, use `get`. It
verifies the complete download in a private temporary file before publishing
the destination:

```bash
docbank cat /taxes/checklist.pdf
docbank get /taxes/checklist.pdf /tmp/checklist.pdf
```

Every imported file also has a stable immutable content-version UUID:

```bash
docbank versions list /taxes/checklist.pdf
docbank versions show <version-id> --json
docbank versions cat <version-id> > /tmp/checklist-version.pdf
```

The version ID survives node renames and moves. Replace the current content
without changing the stable file node:

```bash
docbank put ~/Documents/revised-checklist.pdf /taxes/checklist.pdf
docbank versions list /taxes/checklist.pdf
```

`put` hashes the source before contacting the daemon, then inspects the target
and uploads it, showing separate progress for both file passes. The upload
requires that freshly observed target revision, so a concurrent change fails
instead of being overwritten. The prior version remains available through
`docbank versions cat <old-version-id>`. Adopt it as current without
erasing the replacement:

```bash
docbank revert /taxes/checklist.pdf <old-version-id>
```

Reverting creates a new current version that records the older version it
uses. Docbank reuses the stored bytes and preserves both earlier versions.
See [Editing & Versions](architecture/editing-and-versions.md) for retention
and revision rules.

For text and other editor-friendly files, `edit` verifies a private copy, opens
the blocking command from `VISUAL` or `EDITOR`, and creates a replacement only
when the result changed. GUI editors need their wait flag, for example
`VISUAL='code --wait'`.

```bash
docbank edit /notes/draft.md
```

## Organize independently with tags

Tags survive path changes because assignments use stable tag and node IDs:

```bash
docbank tag create taxes
docbank tag assign taxes /taxes/checklist.pdf
docbank tag nodes taxes
```

The human CLI accepts the current tag name or UUID. Input shaped like a
canonical UUID is always treated as an ID, even if a tag has that string as its
display name. `docbank tag list` shows revisions and assignment counts.
`tag rename` changes only the display name. `tag delete` removes assignments
without deleting any document.

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

Search matches the start of each word in document names and supported current
text. Every term must match:

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
docbank trash empty --older-than 30d         # dry run: report old trash
docbank trash empty --older-than 30d --run   # permanently delete old trash
```

## Reclaim and verify

Emptying the trash deletes tree entries, but the underlying bytes remain
until you garbage-collect. `gc` is a dry run by default; it removes loose files
directly and marks unreachable packed payload as pending repack:

```bash
docbank gc
```

```
3 candidate blob(s), 0 untracked file(s), 1204882 loose byte(s) reclaimable
dry run — pass --run to delete
```

```bash
docbank gc --run
docbank storage status     # shows dead packed payload, if any
docbank storage repack     # compacts eligible sparse packs
docbank verify            # re-hash every stored blob
```

```
211 blob(s) ok, 0 problem(s)
```

## Prove recovery

A backup repository stores recovery snapshots outside the live vault. Each
capture reuses unchanged content from earlier snapshots. The commands below
perform this recovery test:

1. Create temporary backup and restore directories.
2. Initialize a repository and capture the vault.
3. List and verify the snapshots.
4. Restore the latest snapshot into a separate vault.
5. Browse and verify the restored vault.

```bash
export DOCBANK_BACKUP=$(mktemp -d)
export DOCBANK_RESTORE=$(mktemp -d)

docbank backup init --repo "$DOCBANK_BACKUP"
docbank backup create --repo "$DOCBANK_BACKUP"
docbank backup list --repo "$DOCBANK_BACKUP"
docbank backup verify --repo "$DOCBANK_BACKUP"
docbank backup restore --repo "$DOCBANK_BACKUP" --target "$DOCBANK_RESTORE"

DOCBANK_HOME="$DOCBANK_RESTORE" docbank tree /
DOCBANK_HOME="$DOCBANK_RESTORE" docbank verify
```

You can use the restored vault independently. Restore does not replace the
running source vault. See [Backup & Restore](usage/backup.md) for progress
modes, snapshot selection, overwrite rules, and the exact proof returned by
restore.

That is the core document workflow. Continue with
[Capabilities](capabilities.md) for the complete product map, see the real
interfaces in the [Visual Tour](tour.md), continue with
[Vault Lifecycle](usage/lifecycle.md) for maintenance and upgrades, explore
[Docbank for Agents](agents.md) for automation, or use the
[CLI Reference](cli-reference.md) for exact command semantics.
