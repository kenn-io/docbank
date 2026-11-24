---
title: Trash, garbage collection, repack, and verify
description: How deletion and space reclamation work, from trash through garbage collection and repack.
---

# Trash, garbage collection, repack, and verify

`docbank document remove` moves documents to recoverable trash. To delete them
permanently and reclaim disk space, empty trash, run garbage collection (GC),
and repack eligible packed content. GC removes stored content with no remaining
references, and repack rewrites pack files to remove unused bytes.

There is no `document remove --hard`, and neither GC nor repack runs
automatically:

```mermaid
flowchart LR
    A[live node] -- "docbank document remove" --> B[trash]
    B -- "docbank document restore" --> A
    B -- "docbank trash empty --run" --> C[tree metadata gone; blob may be unreachable]
    C -- "docbank vault gc --run" --> D[blob authority removed]
    D -- "loose storage: same GC run" --> E[loose file removed]
    D -- "packed storage" --> F[dead immutable pack range]
    F -- "docbank storage repack" --> G[sparse source pack retired]
```

## Stage 1: Trash (`document remove`, `document restore`, `trash list`)

Running `docbank document remove <path-or-id>` marks a node as trashed, along
with its entire subtree if it is a directory. The tree entry disappears from
`ls`, `tree`, and `search`, the name becomes reusable, and the underlying bytes
stay untouched. Running GC after `document remove` does nothing to that content
because trash remains a live, restorable reference.

```bash
docbank trash list
docbank trash list --json
```

```
SELECTOR  TRASHED AT           NAME
id:15     2026-07-06T21:40:11Z  return.pdf
id:88     2026-07-05T09:12:44Z  old-drafts
```

Human output uses UTC second precision, while `--json` keeps the full
timestamps for automation.

Only trash *roots* appear in the list. Trashing a directory produces one entry,
and `docbank document restore id:<id>` brings the entire subtree back to its
original location. If a live node has since taken that name, the restored node
gets a suffix (`return.pdf` becomes `return (2).pdf`). If the original parent
was itself permanently deleted, the node is restored under `/`.

Trashing a subtree stamps every node with the same trash time, so a nested
directory trashed *before* its parent keeps its own trash entry. Restoring the
parent leaves separately trashed items in trash unless photo-group recovery
includes them.

In Photos, **Trash rejected photos** in the **⋯** menu moves a reviewed batch
of fully rejected photos and their sidecars to recoverable trash. **Move to
trash** and `docbank photos assets trash <asset-id>` move all asset members
together with an asset revision check. The browser Trash page and paginated TUI
list group independently trashed photo members into one row, while the CLI's
unpaged list keeps its ordinary node listing. Restoring any photo member
restores its whole group, including companions in other folders. If that member
belongs to a trashed folder, restore also recovers the folder's original
subtree.

Recovery follows companions transitively. A recovered folder brings back the
companions of every photo inside it, and any trashed folder holding one of
those companions returns with all its contents. One restore can therefore
recover several folders. The restore response and `docbank document restore`
report only the selected node. List the trash afterward to see what remains.

`trash list --json` returns the roots under `items`. For maintenance
automation, `trash empty --json` returns `candidate_roots`, `retained_roots`,
`held_roots`, `deleted`, and `run`, and stays a dry run unless you pass
`--run`.

## Stage 2: Empty the trash

```bash
docbank trash empty                        # dry run: everything
docbank trash empty --older-than 30d       # dry run: items trashed ≥30 days ago
docbank trash empty --older-than 30d --run # permanently delete those items
```

`trash empty` runs as a dry run unless you pass `--run`. An executed run
permanently deletes the selected tree entries, while the document bytes stay on
disk and may still be referenced by another node or version. Only content with
no remaining reference becomes a GC candidate.

Trash previews and deletion serialize with mutations through the maintenance
gate. Large photo cleanups keep other writes waiting or returning busy for
longer, including during a dry run.

Photo groups are deleted together. A live, too-new, or retained member protects
every connected trash root, including folders containing members. To make a
partial group eligible, trash the remaining members with
`docbank photos assets trash <asset-id>` or detach live companions. The
report's `held_roots` counts eligible roots kept this way, and
`docbank trash empty` prints a `held` line when it is nonzero. Bounded
maintenance finishes a complete group even when it exceeds the root budget; the
dry run reports that expanded group, and `More` reports another eligible group.
Photo and album relationships remain recoverable until permanent deletion, and
file bytes stay in place throughout trash and restore.

### Release email attachment references

Mailbox imports and explicit EML transfers keep their messages and attachment
versions for retry receipts. Emptying trash skips those nodes and any
containing folder, so they do not block deletion of unrelated trash. Their
receipts cannot be released. See
[Mailbox archives](importing.md#mailbox-archives).

Published email attachments keep their source and child versions. When a
receipt holds trashed roots, `trash empty` does not fail with an error.
Instead, it skips the held trash roots, counts them in `retained_roots`, and
prints `retained N trashed root(s) referenced by email publications or photo checkouts`.
Version pruning fails with an error that names the blocking receipt's operation
ID. Inspect a blocking receipt before you release its references:

```bash
docbank email documents show <operation-id>
docbank email documents relations --parent-version <version-uuid>
docbank email documents release <operation-id> --request-digest <request-digest>
```

Both `show` and `relations` return JSON. Copy `request_digest` from the
inspected receipt into the release command. Releasing removes the receipt and
its links, keeps ordinary child documents, and relinquishes the original
operation's retry guarantee. Repeat the deletion command after releasing all
blocking receipts. Other retention rules still apply.

To inspect a child's incoming relationships, pass `--child-version`. For page
controls, see the [CLI reference](../cli-reference.md#docbank-email-documents).
For publication and retention rules, see the
[embedding guide](../embedding.md#publish-email-attachment-documents).

## Stage 3: Garbage collection (`vault gc`)

GC removes a blob's catalog record only when nothing retains its content. A
blob remains *reachable*, and GC keeps it, while any of these reference it:

- a live node,
- a trashed node (trash is always restorable in full), or
- a retained prior version of an edited document.

Preview-first [version pruning](../architecture/editing-and-versions.md#choosing-retention)
can release a prior version's reference without deleting the current file.

```bash
docbank vault gc          # dry run: candidate count and reclaimable bytes
docbank vault gc --run    # remove unreachable authority and loose files
```

For loose blobs, the reported reclaimable count is the physical number of raw
or zstd bytes that GC can unlink right away, not the decoded document size. A
packed blob becomes logically dead when GC removes its catalog record, while
its stored bytes remain in the immutable pack until repack compacts that
container. GC reports those bytes separately as pending repack.

`vault gc --run` runs behind the daemon's maintenance gate, so a concurrent
import cannot deduplicate against a blob being deleted (see
[Ownership and concurrency](../architecture/locking.md)). GC removes files
before their rows. A crash in between leaves rows without files, which the next
`vault gc --run` reconciles and `verify` flags in the meantime. Orphan blobs
from interrupted ingests are reclaimed the same way.

## Stage 4: Repack packed storage (`storage repack`)

GC cannot remove a single range from an immutable pack file. After
`vault gc --run`, dead packed payload appears in `storage status` as
`dead_packed_bytes`. Running `docbank storage repack` rewrites eligible sparse
packs with their live blobs and retires the old source packs, while empty packs
are retired directly.

You must run repack yourself. `document remove`, `trash empty`, and `vault gc`
do not run it, and there is no automatic repack schedule. Repacking can rewrite
live content that shares a pack with unused bytes, so you choose its timing and
selection thresholds.

## Embedded maintenance

Embedded applications own the same lifecycle, but they schedule finite passes
instead of asking the daemon to drain a full operation. `EmptyTrash` uses
`MaxRoots` as a batch target and finishes complete photo groups even when they
exceed it, with zero selecting `DefaultTrashEmptyMaxRoots`. `GarbageCollect`,
`Verify`, and `Repack` accept a `WorkBudget`. Zero `MaxObjects` selects the
finite `DefaultMaintenanceMaxObjects`, and explicit `MaxObjects` values above
`MaxMaintenanceObjects` are rejected. A positive `MaxBytes` acts as a soft
limit that lets the current object finish. `Pack` keeps its compatible soft
`MaxBytes` option and reports `More` when eligible loose backlog remains.

If a maintenance report returns `More`, schedule another pass with its
non-empty `NextCursor`, and reuse a cursor only with the operation that issued
it. Cursors are opaque continuation positions, not stable snapshots. Repack
cursors keep the mapping, dead-pack, or sparse-pack phase even when that phase
has no blob-hash position, so continuation does not restart completed mapping
work.

Embedded GC considers only a bounded set of unreachable catalog rows and does
not scan loose directories for untracked files. Embedded Verify re-hashes only
a bounded blob page and does not validate the whole metadata catalog. The
daemon commands above still perform full orphan reconciliation and
whole-catalog verification.

Tree, trash, and version-retention rules decide which references remain. GC
reclaims content only after those references are gone, and repack then reclaims
pack space that GC made unused. For the full daemon maintenance commands, see
the [CLI reference](../cli-reference.md).

## Verify

```bash
docbank vault verify
```

Docbank validates metadata and audit history, then reads every stored blob and
checks its SHA-256 hash. It reports `metadata` failures or `missing`,
`corrupt`, and `unreadable` blobs, and exits with a non-zero status if it finds
any problem. Run verification after you move the vault between disks, before
you delete source files, and periodically from a scheduler such as cron.

Next, protect what remains with [Backup and restore](backup.md), and see
[Integrity and trust](../architecture/integrity.md) for what `verify` defends
against.
