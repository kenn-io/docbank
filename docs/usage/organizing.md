---
last_edited: 2026-10-31
title: Organizing and tagging
description: Browse, move, rename, and tag documents in the virtual tree.
---

# Organizing and tagging

You can move, rename, and tag documents without rewriting their stored bytes.
The folders shown by `document list` and `document tree` are database entries,
and even moving a 4 GB archive changes only metadata. See
[Storage](../architecture/storage.md) for how the folder tree relates to stored
content.

## Browsing

```bash
docbank document list /taxes          # one directory, with stable selectors, sizes, timestamps
docbank document tree /taxes        # subtree (depth 4, 1,000 entries; --all for everything), with id:N selectors in brackets
docbank document cat /taxes/w2.pdf  # stream file bytes to stdout
docbank document get /taxes/w2.pdf ./w2.pdf  # verify, then atomically publish a local file
```

For machine-readable output, `docbank document list --json` returns a directory
envelope containing the resolved directory and its ordered children.
`docbank document tree --json` returns the root plus a flat pre-order list whose
entries carry absolute paths and depths, so a script can walk a subtree without
parsing indentation.

Each file or folder has a stable node ID. Its path can change when you
reorganize the tree, while `id:42` continues to select the same entry. Commands
that target an existing node accept either a path or `id:N`, except
`document restore`, which accepts only `id:N`:

```bash
docbank document cat id:42
docbank document get id:42 ./document.pdf
docbank document move id:42 /taxes/2026/w2.pdf
docbank document remove id:42
docbank document restore id:42
```

Use a path to select whatever is at that location, and use `id:N` to select a
specific file or folder regardless of its current name. JSON uses numeric node
IDs, and the [HTTP API](../architecture/http-api.md) uses IDs as its primary
selectors.

Live-tree commands such as `document list`, `document tree`, `document move`,
`document remove`, `document put`, `document edit`, `document revert`, version
pruning, and tag assignment reject a trashed selector. You can still read a
trashed node's content, versions, and audit history by stable ID. To change a
trashed node again, restore it first with `docbank document restore id:N`.

## Moving and renaming

`docbank document move` works like POSIX `mv`:

```bash
docbank document move /inbox/scan.pdf /taxes/2026          # /taxes/2026 is a dir → move into it
docbank document move /taxes/2026/scan.pdf /taxes/2026/w2.pdf   # dest doesn't exist → rename
docbank document move /inbox/receipts /archive             # directories move with their subtree
```

Before committing a move, Docbank enforces these rules:

- Moving onto an existing live file or directory name fails with
  `name already exists`. Rename or trash the occupant first.
- A directory cannot move under its own descendant.
- Empty names, `.`, `..`, and names containing `/` or NUL are rejected. Names
  are Unicode-normalized (NFC) so visually identical names cannot coexist, and
  they are compared case-sensitively.

Each move increases the revision, which is a change counter, on the node and on
both affected directories. HTTP clients send the revision they inspected in
`If-Match` so the daemon can reject a decision based on older state.

## Trashed names don't block

Sibling names need to be unique among **live** nodes only. After you run
`docbank document remove /inbox/draft.pdf`, you can immediately import or move a
new `draft.pdf` into `/inbox`. You can still restore the trashed file later; if
its old name is occupied at restore time, it gets a suffix.

## Tags

Tags organize documents independently of their current paths. Each tag has a
stable UUID, and its name can change without breaking assignments or agent-held
references.

In the web application, slash-separated names form display groups. For example,
`matter/acme/reviewed` appears as `reviewed` under `matter/acme`. The full name
is still one tag, and assigning it does not assign a parent tag. Use the full
name or UUID in CLI commands. Colors come from stable tag IDs, so renaming a tag
keeps its color, and you do not need to configure groups or colors. See
[web tag controls](web.md#manage-tag-definitions).

![The Docbank web application managing a synthetic vault's stable tag catalog.](https://docbank.ai/assets/generated/web-tag-catalog.png)

```bash
docbank tag create taxes
docbank tag assign taxes /taxes/2026/w2.pdf
docbank tag assign taxes /taxes/2026/return.pdf
docbank tag nodes taxes
docbank tag rename taxes "tax archive"
```

`tag list` shows definitions and assignment counts. `tag show`, `tag rename`,
`tag delete`, `tag assign`, `tag unassign`, and `tag nodes` accept either the
exact current name or the tag UUID. Assignment and definition changes increment
the tag revision and the revisions of affected nodes. Rename and delete apply
only if the tag revision still matches the one inspected, so a stale decision
fails instead of overwriting newer metadata. CLI assignment paths resolve in the
same transaction as the change, so an ancestor move cannot make the command tag
a node that has already left the requested path. Assigning an already assigned
tag or removing an absent assignment changes nothing.

Docbank always reads a selector in canonical UUID form as a tag ID. To address a
tag whose display name looks like a UUID, use its generated ID.

Deleting a tag removes all of its assignments without deleting any node or
document content. A tag recreated under the same name gets a new UUID. Trashed
nodes keep their tag assignments and appear as `trashed` in `tag nodes`, while
path-based assignment commands address live nodes only. When `trash empty`
permanently deletes tagged nodes, each affected tag revision advances before
those assignments are removed.

## Tag a selected set atomically

To tag up to 1,000 selected documents at once in the web app:

1. Select the document checkboxes and choose **Tag selected…**.
2. Pick one tag.
3. Choose **Add** or **Remove**.
4. Press **Apply**.

If any target is missing, trashed, or has changed since selection, the whole
operation fails without changing assignments. Documents whose assignment is
already correct keep their node revisions, and actual changes record the normal
audit events.

If the result is uncertain, choose **Check tag results** to inspect each
selected document. Review current membership, then choose whether to retry
readable documents that still need the change; the retry uses their current
revisions. After a confirmed retry, check again to include documents that could
not be read earlier. After an uncertain change, Docbank refreshes the view when
you close the dialog. If the initial change is refused because a document is
stale, close the dialog, refresh, and select the documents again.

API clients call `POST /api/v1/batch/tags` with one tag ID, an operation UUID,
an assignment choice, and node/revision pairs. Docbank keeps receipts
indefinitely, including after tag or node deletion, and they survive backup and
restore when the backup checkpoint includes them. Receipts contain identities
and revisions rather than filenames or tag names. Replaying a request confirms
the original outcome, not current membership. See the
[batch tag contract](../architecture/http-api.md#batch-tag-assignment).

## Atomic bulk reorganization

When a reorganization must either happen completely or leave the tree untouched,
use `docbank document move batch`. The command reads a JSON plan from a file, or
from standard input with `-`:

```json
{
  "moves": [
    {"source": "/inbox/final.pdf", "destination": "/filed/draft.pdf"},
    {"source": "id:42", "destination": "/inbox/final.pdf"}
  ]
}
```

```bash
docbank document move batch reorganization.json
```

Docbank resolves every source against the tree at the start of the transaction.
Each batch destination is the exact final path, and an existing directory is not
shorthand for moving into that directory. To move a document into `/filed` and
keep the name `a.pdf`, give `/filed/a.pdf` as the destination. That full-path
rule keeps file and directory swaps unambiguous. Destination parents resolve in
the planned final tree, so one item can move beneath a directory that another
item moves in the same batch.

Before changing any entry, Docbank checks the complete proposed tree for missing
parents, duplicate names within a folder, and cycles. If any selector, revision,
or final path is invalid, nothing moves.

A path source targets the node at that coordinate when the transaction runs. An
`id:N` source targets that exact node; the CLI resolves it before submission and
binds the plan to its current revision. Receipts preserve plan order and return
each node's stable ID, prior path, final path, and resulting revision. Plans
accept at most 1,000 moves so validation and responses remain bounded.

Repeated `docbank document move` commands run as independent transactions. Use
`docbank document move batch` when partial completion is not acceptable.

Next: find what you filed with [Searching](searching.md), or manage deletion and
recovery with [Trash, garbage collection, repack, and verify](trash-and-gc.md).
