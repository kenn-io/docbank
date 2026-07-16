---
title: Organizing the Tree
description: Browsing, moving, and renaming in the virtual tree.
---

# Organizing the Tree

The folder structure you see in `ls` and `tree` is virtual — rows in
SQLite, not directories on disk. That makes reorganization instant and
transactional no matter how large the files are: moving a 4 GB scan
archive is one metadata update.

## Browsing

```bash
docbank ls /taxes          # one directory, with IDs, sizes, timestamps
docbank tree /taxes        # whole subtree, indented, IDs in brackets
docbank cat /taxes/w2.pdf  # stream file bytes to stdout
```

Node IDs appear everywhere deliberately: IDs are the canonical way to
refer to a document. Paths change when you reorganize; IDs never do.
The CLI uses IDs for trash recovery (`docbank restore <id>`), and the
[HTTP API](../architecture/http-api.md) is ID-first throughout.

## Moving and renaming

`docbank mv` follows POSIX `mv` intuition:

```bash
docbank mv /inbox/scan.pdf /taxes/2026          # /taxes/2026 is a dir → move into it
docbank mv /taxes/2026/scan.pdf /taxes/2026/w2.pdf   # dest doesn't exist → rename
docbank mv /inbox/receipts /archive             # directories move with their subtree
```

Rules the store enforces, atomically, per move:

- **No overwrites.** Moving onto an existing live file or directory name
  fails with `name already exists`. Rename or trash the occupant first.
- **No cycles.** A directory cannot move under its own descendant.
- **Names are validated.** Empty, `.`, `..`, and names containing `/` or
  NUL are rejected. Names are Unicode-normalized (NFC) so visually
  identical names can't coexist, and compared case-sensitively.

A move bumps the node's revision and both affected directories' — that's
the change-detection signal the HTTP API's `If-Match` preconditions use.

## Trashed names don't block

Sibling-name uniqueness applies to **live** nodes only. After
`docbank rm /inbox/draft.pdf` you can immediately import or move a new
`draft.pdf` into `/inbox`; the trashed one remains restorable (it gets a
suffix if its old name is occupied at restore time).

## Tags

Tags organize documents independently of their current paths. Each tag has a
stable UUID; its name can change without breaking assignments or agent-held
references.

```bash
docbank tag create taxes
docbank tag assign taxes /taxes/2026/w2.pdf
docbank tag assign taxes /taxes/2026/return.pdf
docbank tag nodes taxes
docbank tag rename taxes "tax archive"
```

`tag list` shows definitions and assignment counts. `tag show`, `tag rename`,
`tag delete`, `tag assign`, `tag unassign`, and `tag nodes` accept either the
exact current name or stable tag UUID. Assignment and definition changes bump
the affected nodes' revisions, so a concurrent stale decision fails rather
than silently overwriting newer metadata. CLI assignment paths resolve in the
same transaction as the change, so an ancestor move cannot make the command
tag a node that has already left the requested path. Repeating an existing
assignment or missing unassignment is an idempotent no-op.

Canonical UUID-shaped selectors are always interpreted as stable IDs. A tag
whose display name happens to look like a UUID remains addressable through its
own generated ID, not through that ambiguous name.

Deleting a tag removes its complete assignment set but never deletes a node or
document content. Recreating the same name receives a new UUID. Trashed nodes
retain their tag assignments and appear as `trashed` in `tag nodes`; path-based
assignment commands intentionally address live nodes only.

## Tips for bulk reorganization

- `tree` output includes IDs, so you can script against stable
  references while shuffling paths.
- Every `mv` is its own transaction: a failed move in a scripted batch
  leaves everything else applied and consistent.

Bulk all-or-nothing moves are not available. Scripts must therefore treat each
current `mv` as an independently committed operation.
