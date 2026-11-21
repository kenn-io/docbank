---
title: Quickstart
description: A ten-minute tour of the docbank CLI.
---

# Quickstart

In about ten minutes, you can import a folder of documents into a temporary
vault, organize and search them, and test a backup and restore. These
examples use a Unix shell. Set `DOCBANK_HOME` first so every command targets a
scratch vault:

```bash
export DOCBANK_HOME=$(mktemp -d)
```

## Import some documents

`add` copies files into the vault without modifying or deleting the originals:

```bash
docbank document add ~/Documents/taxes --dest /
```

```
added: 214  skipped: 0  excluded: 0  failed: 0
```

Directories import recursively. The directory's own name becomes a folder under
`--dest`, keeping the relative structure underneath intact. Without `--dest`,
files land in `/inbox`.

### The daemon

That first `add` command starts the daemon, the background process that owns
and reads your vault. Any data command starts it automatically when needed, and
it exits after a period of inactivity. Run `docbank daemon status` to check on
it, or `docbank daemon stop` to shut it down. See
[Daemon](architecture/daemon.md) for startup and lifetime rules.

If you run the same import again, Docbank skips the files it already has. It
checks content at the destination name and its collision suffixes, as described
in [Importing documents](usage/importing.md):

```
added: 0  skipped: 214  excluded: 0  failed: 0
```

## Browse the tree

List a folder to see its entries, sizes, and modification times:

```bash
docbank document list /taxes
```

```
SELECTOR  KIND  SIZE   MODIFIED              NAME
id:14     dir   0      2026-07-06T21:14:03Z  2024
id:102    dir   0      2026-07-06T21:14:05Z  2025
id:231    file  48211  2026-07-06T21:14:08Z  checklist.pdf
```

To see the full hierarchy along with node IDs, run `tree`:

```bash
docbank document tree /taxes
```

```
/taxes
  2024  [id:14]
    return.pdf  [id:15]
  2025  [id:102]
    return.pdf  [id:103]
  checklist.pdf  [id:231]
```

To browse the same tree interactively, open the terminal browser:

```bash
docbank tui
```

Use the arrow keys or `j` and `k` to move through documents, press **Enter** to
open a directory, and press `/` to search names and extracted text. Press `i`
to check a document's IDs, revision, and content hash, or `a` to view its
permanent audit timeline. Press `x` to review moving the selected revision to
recoverable trash, `T` to browse and restore trash roots, or `O` to inspect the
read-only storage inventory and configured backup recovery points. The TUI does
not expose permanent deletion or maintenance mutations. See the
[interactive terminal browser](usage/tui.md) guide for the full key map.

To stream a file's bytes to stdout, use `cat`. To save a local copy, use `get`,
which verifies the complete download in a private temporary file before writing
the destination:

```bash
docbank document cat /taxes/checklist.pdf
docbank document get /taxes/checklist.pdf /tmp/checklist.pdf
```

Every imported file gets an immutable content version with its own UUID, which
stays valid even if you rename or move the node:

```bash
docbank document versions list /taxes/checklist.pdf
docbank document versions show <version-uuid> --json
docbank document versions cat <version-uuid> > /tmp/checklist-version.pdf
```

To replace the current content while keeping the same file node, use `put`:

```bash
docbank document put ~/Documents/revised-checklist.pdf /taxes/checklist.pdf
docbank document versions list /taxes/checklist.pdf
```

`put` hashes the source file before contacting the daemon, then inspects the
target and uploads the replacement while showing separate progress for each
pass. The upload checks that the target still has the revision `put`
observed, so it fails if the target changed in the meantime instead of
overwriting that change. The previous version remains available through
`docbank document versions cat <old-version-uuid>`.

To adopt an older version as current without erasing the replacement, run
`revert`:

```bash
docbank document revert /taxes/checklist.pdf <old-version-uuid>
```

Reverting creates a new current version that records which older version it
uses. Docbank reuses the stored bytes and keeps both earlier versions. See
[Editing and versions](architecture/editing-and-versions.md) for retention and
revision rules.

For text and other editor-friendly files, `edit` verifies a private copy, opens
the blocking command from `VISUAL` or `EDITOR`, and creates a new version only
when the file changed. If you use a GUI editor, include its wait flag, for
example `VISUAL='code --wait'`:

```bash
docbank document edit /notes/draft.md
```

## Organize independently with tags

Tags survive path changes because assignments use stable tag and node IDs:

```bash
docbank tag create taxes
docbank tag assign taxes /taxes/checklist.pdf
docbank tag nodes taxes
```

The human CLI accepts either the current tag name or its UUID. Any input shaped like
a canonical UUID is always treated as an ID, even if a tag uses that string as
its display name. Run `docbank tag list` to see revisions and assignment
counts. `tag rename` changes only the display name, and `tag delete` removes
assignments without deleting any document.

## Reorganize

Moves and renames update metadata only. The stored bytes stay where they are:

```bash
# Rename in place (destination doesn't exist; its parent does)
docbank document move /taxes/checklist.pdf /taxes/filing-checklist.pdf

# Move into an existing directory, keeping the name
docbank document move /taxes/filing-checklist.pdf /taxes/2025
```

```
moved [id:231] /taxes/2025/filing-checklist.pdf
```

## Search

Search matches prepared words and their regular forms in document names and
supported current text. Every remaining word in the query must match. To
require adjacent words, quote a phrase:

```bash
docbank search filing check
```

```
SELECTOR  MATCH  PATH
id:231    name   /taxes/2025/filing-checklist.pdf
```

## Trash and recovery

`remove` performs soft deletion. It moves the node, along with its subtree for
directories, to the trash and frees up its name for reuse:

```bash
docbank document remove /taxes/2024/return.pdf
```

```
trashed [id:15] /taxes/2024/return.pdf (restore with: docbank document restore id:15)
```

```bash
docbank trash list        # what's recoverable
docbank document restore 15        # put it back where it was
docbank trash empty --older-than 30d         # dry run: report old trash
docbank trash empty --older-than 30d --run   # permanently delete old trash
```

## Reclaim and verify

Emptying the trash deletes tree entries, while the underlying bytes stay on
disk until you run garbage collection. `gc` runs in dry-run mode by default.
When you pass `--run`, it removes loose files directly and marks unreachable
packed payload as pending repack:

```bash
docbank vault gc
```

```
3 candidate blob(s), 0 untracked file(s), 1204882 loose byte(s) reclaimable
dry run — pass --run to delete
```

```bash
docbank vault gc --run
docbank storage status     # shows dead packed payload, if any
docbank storage repack     # compacts eligible sparse packs
docbank vault verify            # re-hash every stored blob
```

```
211 blob(s) ok, 0 problem(s)
```

## Prove recovery

A backup repository stores recovery snapshots outside the live vault, and each
capture reuses unchanged content from earlier snapshots. Follow these steps to
run a complete recovery test:

1. Create temporary backup and restore directories.
2. Initialize a backup repository and capture the vault.
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

DOCBANK_HOME="$DOCBANK_RESTORE" docbank document tree /
DOCBANK_HOME="$DOCBANK_RESTORE" docbank vault verify
```

The restored vault works independently and does not replace your running source
vault. See [Backup and restore](usage/backup.md) for progress modes, snapshot
selection, overwrite rules, and the proof that restore returns.

From here, you can explore the rest of the docs:

- [Capabilities](capabilities.md) maps out everything Docbank does today.
- [Visual tour](tour.md) shows the web app and terminal browser in action.
- [Vault lifecycle](usage/lifecycle.md) covers ongoing maintenance and upgrades.
- [Docbank for agents](agents.md) covers agent integration and automation.
- [CLI reference](cli-reference.md) documents every command and flag.
