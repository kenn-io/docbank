---
title: Trash, GC, Repack & Verify
description: The explicit deletion and physical-reclamation lifecycle.
---

# Trash, GC, Repack & Verify

`docbank rm` is always a soft deletion. There is no `rm --hard`, and neither GC
nor repack runs automatically. Permanent deletion and physical reclamation are
separate operator decisions, so the window for regret is as wide as you want
it to be:

```mermaid
flowchart LR
    A[live node] -- "docbank rm" --> B[trash]
    B -- "docbank restore" --> A
    B -- "docbank trash empty --run" --> C[tree metadata gone; blob may be unreachable]
    C -- "docbank gc --run" --> D[blob authority removed]
    D -- "loose storage: same GC run" --> E[loose file removed]
    D -- "packed storage" --> F[dead immutable pack range]
    F -- "docbank storage repack" --> G[sparse source pack retired]
```

## Stage 1: Trash (`rm`, `restore`, `trash list`)

`docbank rm <path>` marks the node — and its whole subtree for
directories — as trashed. The tree entry disappears from `ls`, `tree`,
and `search`; the name becomes reusable; the bytes are untouched. Running GC
after `rm` does nothing to that content because trash remains a live,
restorable reference.

```bash
docbank trash list
```

```
ID   TRASHED AT                     NAME
15   2026-07-06T21:40:11.0021Z      return.pdf
88   2026-07-05T09:12:44.8810Z      old-drafts
```

Only trash *roots* are listed: trashing a directory produces one entry,
and `docbank restore <id>` brings the entire subtree back to its
original location. If a live node has since taken the name, the restored
node is suffixed (`return.pdf` → `return (2).pdf`); if the original
parent was itself permanently deleted, the node is restored under `/`.

Trashing a subtree stamps every node with the same trash time, so a
nested directory trashed *before* its parent keeps its own independent
trash entry — restoring the parent doesn't resurrect things you trashed
separately.

## Stage 2: Empty the trash

```bash
docbank trash empty                        # dry run: everything
docbank trash empty --older-than 30d       # dry run: items trashed ≥30 days ago
docbank trash empty --older-than 30d --run # permanently delete those items
```

The command is a dry run unless `--run` is present. An executed run
permanently deletes the selected tree entries. The document bytes are still
on disk and may still be referenced by another node or version. Only content
with no remaining reference becomes a GC candidate.

## Stage 3: Garbage collection (`gc`)

Unreachable blob authority is removed only by explicit GC. A blob is
*reachable* — and therefore never collected — while any of these reference it:

- a live node,
- a trashed node (trash is always restorable in full), or
- a recorded prior version of an edited document
  (see [Editing & Versions](../architecture/editing-and-versions.md)).

```bash
docbank gc          # dry run: candidate count and reclaimable bytes
docbank gc --run    # remove unreachable authority and loose files
```

For loose blobs, the reported reclaimable count is the number of bytes that GC
can unlink immediately. A packed blob becomes logically dead when GC removes
its catalog authority, but its stored bytes remain in the immutable pack until
repack compacts that container; GC reports those bytes separately as pending
repack rather than claiming they were reclaimed.

`gc --run` runs behind the daemon's maintenance gate, so a concurrent
import can never dedup against a blob that's being deleted (see
[Concurrency & Locking](../architecture/locking.md)). Files are removed
before their rows: a crash in between leaves rows-without-files, which
the next `gc --run` reconciles and `verify` flags in the meantime.
Orphan blobs from interrupted ingests are reclaimed the same way.

## Stage 4: Repack packed storage (`storage repack`)

GC cannot remove one range from an immutable pack file. After `gc --run`, dead
packed payload appears in `storage status` as `dead_packed_bytes`. An explicit
`docbank storage repack` rewrites eligible sparse packs with their live blobs
and retires the old source packs. Empty packs are retired directly.

Repack is not part of `rm`, `trash empty`, or `gc`, and there is currently no
background maintenance scheduler. This is intentional: repacking may rewrite
unrelated live blobs that share the same pack, so its timing and selection
thresholds remain an independent storage-policy decision.

## Verify

```bash
docbank verify
```

Re-hashes every stored blob against its recorded SHA-256 and reports
`missing`, `corrupt`, or `unreadable` per problem blob, exiting non-zero
if anything is wrong. Corruption is something you detect on your
schedule, not something you discover the day you need the document. Run
it after moving the vault between disks, before deleting original
sources, and periodically from cron.
