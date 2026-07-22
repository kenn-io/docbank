---
title: Organizing & Tagging
description: Browsing, moving, renaming, and tagging in the virtual tree.
---

# Organizing & Tagging

The folder structure you see in `ls` and `tree` is virtual — rows in
SQLite, not directories on disk. That makes reorganization instant and
transactional no matter how large the files are: moving a 4 GB scan
archive is one metadata update.

## Browsing

```bash
docbank ls /taxes          # one directory, with stable selectors, sizes, timestamps
docbank tree /taxes        # whole subtree, with id:N selectors in brackets
docbank cat /taxes/w2.pdf  # stream file bytes to stdout
```

Use `ls --json` for a directory envelope containing the resolved directory
and its ordered children. `tree --json` returns the root plus a flat,
deterministic pre-order list whose entries carry absolute paths and depths;
this avoids parsing indentation when a script needs to walk a subtree.

Node selectors appear everywhere deliberately. A path is a live coordinate
that can change during reorganization; `id:42` continues to name the same node.
Commands that target an existing node accept either form:

```bash
docbank cat id:42
docbank mv id:42 /taxes/2026/w2.pdf
docbank rm id:42
docbank restore id:42
```

Use paths when the coordinate itself is your intent, and `id:N` when you mean
the object regardless of its current name. JSON uses numeric node IDs, and the
[HTTP API](../architecture/http-api.md) is ID-first throughout.

Live-tree commands such as `ls`, `tree`, `mv`, `rm`, `put`, `edit`, `revert`,
version pruning, and tag assignment reject a trashed selector. Read-only
content, version, and audit inspection remains available by stable ID while a
node is in trash; use `restore id:N` before changing it again.

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
the tag revision; they also bump affected nodes' revisions. Rename and delete
condition their change on the inspected tag revision, so a concurrent stale
decision fails rather than silently overwriting newer metadata. CLI assignment
paths resolve in the same transaction as the change, so an ancestor move cannot
make the command tag a node that has already left the requested path. Repeating
an existing assignment or missing unassignment is an idempotent no-op.

Canonical UUID-shaped selectors are always interpreted as stable IDs. A tag
whose display name happens to look like a UUID remains addressable through its
own generated ID, not through that ambiguous name.

Deleting a tag removes its complete assignment set but never deletes a node or
document content. Recreating the same name receives a new UUID. Trashed nodes
retain their tag assignments and appear as `trashed` in `tag nodes`; path-based
assignment commands intentionally address live nodes only. When `trash empty`
permanently deletes tagged nodes, each affected tag revision advances before
those assignments are removed.

## Atomic bulk reorganization

Use `mv batch` when a reorganization must either happen completely or leave the
tree untouched. The command reads a bounded JSON plan from a file, or from
standard input with `-`:

```json
{
  "moves": [
    {"source": "/inbox/final.pdf", "destination": "/filed/draft.pdf"},
    {"source": "id:42", "destination": "/inbox/final.pdf"}
  ]
}
```

```bash
docbank mv batch reorganization.json
```

Every source is interpreted against the tree as it existed at the start of the
transaction. A batch destination is the exact final coordinate; an existing
directory is not shorthand for “move into this directory.” Destination parents
are resolved in the planned final tree, so one item can move beneath a directory
that another item moves in the same batch. To move a document into `/filed`
while retaining `a.pdf`, name `/filed/a.pdf` explicitly. This makes file and
directory swaps unambiguous.
The complete final tree is then checked for missing parents, duplicate sibling
names, and cycles before any row changes. If any selector, revision, or final
coordinate is invalid, nothing moves.

A path source means “the node at this coordinate when the transaction runs.”
An `id:N` source means “this exact node”; the CLI resolves it before submission
and binds the plan to its current revision. Receipts preserve plan order and
return each node's stable ID, prior path, final path, and resulting revision.
Plans accept at most 1,000 moves so validation and responses remain bounded.

Ordinary repeated `docbank mv` commands are still independent transactions;
use `mv batch` when partial completion is not acceptable.

Next: find what you filed with [Searching](searching.md), or manage
deletion and recovery with [Trash, GC, Repack & Verify](trash-and-gc.md).
