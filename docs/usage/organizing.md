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

## Tips for bulk reorganization

- `tree` output includes IDs, so you can script against stable
  references while shuffling paths.
- Every `mv` is its own transaction: a failed move in a scripted batch
  leaves everything else applied and consistent.

!!! info "Planned — Phase 2b"
    `POST /batch/move` joins the HTTP API with a `dry_run` mode that
    validates an entire reorganization (collisions, cycles, missing IDs)
    before applying it all-or-nothing in one transaction — designed for
    agent-driven filing. All entries are evaluated from one pre-state to one
    final post-state rather than in request order; duplicate changes to one
    node and invalid final graphs are rejected, while valid nested moves produce
    one canonical atomic delta and net path changes. Tags (orthogonal to the
    tree) are in the schema
    and surface alongside it.
