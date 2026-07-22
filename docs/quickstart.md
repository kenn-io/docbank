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

### The daemon

That `add` just auto-started the `docbank` daemon in the background —
every data command does, if one isn't already running. You don't need
to think about it day to day; `docbank daemon status` shows it if you're
curious, and `docbank daemon stop` shuts it down (it also exits on its
own after a period of inactivity). See [Daemon](architecture/daemon.md).

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

`cat` streams a file's bytes to stdout. For a durable local file, `get` first
verifies the complete download in private staging and only then publishes it:

```bash
docbank cat /taxes/checklist.pdf
docbank get /taxes/checklist.pdf /tmp/checklist.pdf
```

!!! info "Release availability"

    `docbank get` is newer than v0.10.0. Build from source to use it until the
    next release is tagged.

!!! info "New version-command vocabulary"

    `docbank version` and the explicit `docbank versions list|show|cat`
    commands are newer than v0.7.0. Build from source to use them until the
    next release is published.

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

Reversion creates another immutable head that records its source. It does not
copy the source blob or rewind history, so both the replacement and the original
remain addressable.

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

The human CLI accepts the current tag name or UUID. Canonical UUID-shaped input
is always treated as an ID, so stable authority cannot later be captured by a
display name. `docbank tag list` shows revisions and assignment counts; `tag
rename` changes only the display name, and `tag delete` removes assignments
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

Backup repositories are separate from the live vault. Captures are
incremental: unchanged content is reused across snapshots. This scratch loop
creates a repository, captures the vault, verifies the repository, and restores
the latest snapshot into a different vault:

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

The restored target is independently usable; the running source vault is not
replaced. See [Backup & Restore](usage/backup.md) for progress modes, snapshot
selection, overwrite rules, and the exact proof returned by restore.

That is the core document workflow. Continue with
[Vault Lifecycle](usage/lifecycle.md) for maintenance and upgrades, explore
[Docbank for Agents](agents.md) for automation, or use the
[CLI Reference](cli-reference.md) for exact command semantics.
