---
title: Embed in Go
description: Own one or more independently rooted Docbank vaults inside a Go application, with CGO or pure-Go SQLite.
---

# Embed in Go

Go applications can use Docbank as an in-process document store without starting
or discovering a daemon. An embedded vault uses the same virtual tree, immutable
content versions, content-addressed blob store, packed-storage authority, and
exclusive hierarchy locking as a standalone vault.

Use an embedded vault when the application itself should own document lifecycle.
Use the HTTP API when independent processes need to share one standalone vault.

## Create a vault service

Each root is an independent archive. One process may open several
non-overlapping roots at once. The same root, an ancestor, or a descendant is
rejected while another daemon, embedded vault, or restore target owns it, even
when a filesystem alias spells that tree differently.

```go
package archive

import (
    "bytes"
    "context"
    "io"

    "go.kenn.io/docbank"
)

func StoreSession(ctx context.Context, root string, sessionID string, jsonl []byte) error {
    vault, err := docbank.New(ctx, docbank.Config{Root: root})
    if err != nil {
        return err
    }
    defer vault.Close()

    _, err = vault.Put(
        ctx,
        "/sessions/"+sessionID+".jsonl",
        bytes.NewReader(jsonl),
        docbank.PutOptions{MediaType: "application/x-ndjson"},
    )
    if err != nil {
        return err
    }

    content, err := vault.OpenContent(ctx, "/sessions/"+sessionID+".jsonl")
    if err != nil {
        return err
    }
    defer content.Reader.Close()
    _, err = io.Copy(io.Discard, content.Reader)
    if err != nil {
        return err
    }
    return content.Reader.Verify()
}
```

`Put` creates missing virtual directories. Repeating the same bytes and media
type converges on the current version; changed bytes append an immutable content
version while preserving the node ID. Supply `PutOptions.Expected` when the
caller already knows the SHA-256 and byte count and wants Docbank to reject a
mismatched stream before granting metadata authority.

Use `Create` when an application owns an immutable key and must never replace
different content already stored there. It requires the expected SHA-256 and
size. An exact retry is idempotent; a different byte identity, media type, or
node kind returns `ErrContentConflict` without appending a version:

```go
receipt, err := vault.Create(ctx, "/records/immutable.jsonl", reader,
    docbank.CreateOptions{
        MediaType: "application/x-ndjson",
        Expected: docbank.ContentIdentity{SHA256: digest, Size: size},
    },
)
```

`ContentIdentity` always describes decoded document bytes, regardless of
whether those bytes are stored raw, zstd-compressed, or in a pack. SHA-256 is
the canonical logical identity, not a digest of a physical storage file.

Both write receipts include `Physical`, which distinguishes logical bytes from
their current raw, zstd, or packed representation. Most applications should
treat that field as operational evidence rather than document identity.

`Put` is an idempotent content write, not an integrity repair primitive. Kit's
structural dedup can reuse an existing canonical representation without hashing
it, and packed catalog authority can remain selected over a loose copy. When an
application has trusted bytes for a known SHA-256 and size, `RepairContent`
verifies the complete stream before replacing physical authority. It preserves
every node and historical version reference to that identity. Repairing packed
content makes a verified loose copy authoritative; a later repack reclaims the
now-dead packed bytes.

### Choose loose compression

New content remains loose until an explicit `Pack` call. The standalone daemon
selects zstd when a new loose object is at least 4 KiB and compression saves at
least 10%. Embedded vaults keep raw loose storage by default; an owner can match
the daemon policy explicitly or choose application-specific thresholds:

```go
vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    LooseCompression: docbank.LooseCompressionOptions{
        Enabled:           true,
        MinBytes:          4 << 10,
        MinSavingsPercent: 10,
    },
})
```

Docbank keeps zstd only when the logical size meets `MinBytes` and the completed
encoding saves at least `MinSavingsPercent`; otherwise it publishes raw loose
content. Enabling compression does not proactively migrate or rewrite existing
objects. `RepairContent` preserves an existing loose object's raw or zstd
encoding. It applies this policy only when trusted bytes replace packed or
missing physical authority. The zero value disables compression, preserving the
unchanged raw loose layout, and mixed raw, zstd, and packed content remains
readable through the same verified API. Receipts report the chosen physical
encoding and stored size without changing the logical SHA-256 or size.

An eligible write temporarily needs scratch space for both the raw object and
its compressed candidate before Docbank chooses one for durable publication.
`LooseBacklog` reports how much indexed loose content remains eligible for an
explicit pack pass, split into raw and compressed object counts. It is useful
for scheduling; it does not make packing automatic.

`vault.ID()` returns the archive's stable UUID. JSONL backup and restore
preserve that identity even when the restored vault has a different filesystem
root; applications can therefore distinguish logical archives without treating
paths as identity.

An `OpenContent` stream is not authoritative until it reaches terminal `io.EOF`
or `Verify` succeeds. Early `Close` does not drain the stream. `Vault.Close`
waits for active operations and streams, closes storage, and releases the vault
lock.

`OpenContent` and `OpenVersionContent` wrap `ErrContentUnavailable` when the
catalog-authorized physical content cannot be opened or its physical size
disagrees with metadata. Metadata lookup failures retain their existing
`ErrNotFound`, `ErrNotFile`, or `ErrClosed` classification instead. A canceled
physical open can match both `context.Canceled` and `ErrContentUnavailable`, so
callers that distinguish cancellation should check the context error first.

## Traverse and mutate the tree

`Children` exposes the live virtual tree without materializing an unbounded
directory. Resolve a directory with `Stat`, then advance through its direct
children with `Limit` and `Offset`:

```go
manifests, err := vault.Stat(ctx, "/manifests")
if err != nil {
    return err
}
for offset := 0; ; {
    page, err := vault.Children(ctx, manifests.ID, docbank.ChildrenOptions{
        Limit:  500,
        Offset: offset,
    })
    if err != nil {
        return err
    }
    for _, child := range page.Items {
        // Inspect this bounded page.
        _ = child
    }
    offset += len(page.Items)
    if offset >= page.Total || len(page.Items) == 0 {
        break
    }
}
```

Pages contain directories first and files second, name-sorted within each kind.
A zero limit uses `DefaultChildrenLimit`; one call cannot exceed
`MaxChildrenLimit`. The total and page come from one metadata snapshot, but a
caller that needs a complete stable traversal must avoid concurrent tree
mutations between page calls.

Use `Walk` for a complete stable traversal. It pins one SQLite snapshot before
returning and yields the selected root and its descendants in bounded pages:

```go
walker, err := vault.Walk(ctx, "/sessions", docbank.WalkOptions{PageSize: 500})
if err != nil {
    return err
}
defer walker.Close()

for {
    page, err := walker.Next(ctx)
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    for _, entry := range page {
        // entry.Path is canonical within the pinned snapshot.
        _ = entry
    }
}
```

The zero page size uses `DefaultWalkPageSize`, and no page can exceed
`MaxWalkPageSize`. Traversal expands an indexed ordered frontier incrementally;
setup does not materialize the selected subtree, and each returned node requires
at most two sibling range seeks and one child seek. The second sibling seek is
needed only when an include-trash walk exhausts duplicate node IDs for one path
and advances to the next name. Canonical paths are limited to
`MaxWalkPathBytes`, and absolute hierarchy depth is limited to `MaxWalkDepth`.
Later tree mutations do not enter the pinned snapshot. `Walker.Close` is
required even after `io.EOF`: it idempotently releases the read transaction,
dedicated connection, and vault lifecycle lease. A concurrent `Vault.Close`
waits for every walker and content reader to close.

`MovePath`, `TrashPath`, and `Restore` return the resulting node and canonical
path. Their optional positive `IfRevision` rejects stale mutations;
`IfRevision == 0` is unconditional. `EmptyTrash` previews or deletes at most a
finite number of trash roots: a zero `MaxRoots` uses
`DefaultTrashEmptyMaxRoots`, and `More` asks the owner to schedule another
batch.

Use `BatchMove` for an all-or-nothing reorganization of up to
`MaxBatchMoves` nodes. Each source is either a path resolved inside the
transaction or a stable node ID with the revision previously inspected. All
destinations are exact final coordinates interpreted from one initial tree; an
existing directory does not mean “move into.” The complete final tree is
validated before any change, so embedded applications can express file or
directory swaps and nested moves without temporary names or partial completion.

## Maintain physical storage

Ordinary `Put` calls publish loose content. Call `Pack` explicitly when the
embedded owner is ready to move authorized loose blobs into managed immutable
packs:

```go
report, err := vault.Pack(ctx, docbank.PackOptions{MaxBytes: 256 << 20})
if err != nil {
    return err
}
if report.More {
    // Run another bounded pass when scheduling allows.
}
```

`MaxBytes` is a soft committed raw-byte budget: the pass finishes the blob that
crosses the budget, seals its pack, and stops. Zero is unlimited. The report
includes packing, reconciliation, missing/corrupt content, and orphan cleanup
outcomes; embedded applications should surface those fields rather than treating
a nil error alone as a complete health report. Packing changes only physical
representation. `OpenContent` keeps the same verified read contract.

Embedded `GarbageCollect`, `Verify`, and `Repack` calls are resumable bounded
passes. `WorkBudget.MaxObjects == 0` uses the finite
`DefaultMaintenanceMaxObjects`; larger explicit budgets must not exceed
`MaxMaintenanceObjects`. A positive `MaxBytes` adds a soft byte bound, so one
selected object may finish after crossing it. A zero byte bound is unlimited,
but the object bound still limits each pass.

When a report has `More`, pass a non-empty `NextCursor` back in the same
operation's next `WorkBudget`. Treat cursors as opaque and operation-specific;
malformed cursors and cursors from another operation return
`ErrInvalidMaintenanceCursor`. A Repack pass can validly report more work with
an empty cursor when completed mutations themselves reduce the candidate set;
repeat it from an empty cursor. Cursors are continuation positions, not snapshot
tokens: work inserted earlier in canonical order during a cycle waits for a
later cycle started without a cursor.

The bounded embedded contract is intentionally narrower than the standalone
full-run commands. Embedded `Verify` checks a bounded page of blob bytes but
does not perform whole-catalog metadata validation. Embedded `GarbageCollect`
handles bounded unreachable catalog authority but does not enumerate untracked
filesystem files. The daemon's `verify` and `gc` commands retain those full-run
checks.

These methods coordinate physical representation and reclamation; they never
decide application-level liveness. The embedding application decides when nodes
leave trash, when prior versions are pruned, and which logical references
remain. Only then can GC observe a blob as unreachable. There is no background
maintenance scheduler for embedded vaults, so the owner chooses when to resume
these bounded passes.

## Choose SQLite

The build default preserves performance where CGO is available:

- CGO builds use `github.com/mattn/go-sqlite3`.
- `CGO_ENABLED=0` builds use `modernc.org/sqlite`.

An application may select either adapter explicitly:

```go
import (
    "go.kenn.io/docbank"
    "go.kenn.io/docbank/pkg/sqlite/modernc"
)

vault, err := docbank.New(ctx, docbank.Config{
    Root: root,
    SQLite: modernc.Driver{},
})
```

Use `sqlite/mattn.Driver` in a CGO build to select the CGO adapter explicitly.
The adapter is selected when the vault opens; query and transaction operations
then run directly on that driver's `database/sql` pool. Standalone backup and
restore paths use the same adapter boundary rather than silently switching
SQLite implementations.

## Ownership boundary

Do not point a daemon and an embedded application at the same root. `New` holds
the same exclusive hierarchy lock as the daemon for the entire vault lifetime
and fails if the requested root overlaps another active vault or restore target.

The standalone CLI remains daemon-first and never opens storage directly.
Embedding is a distinct application ownership mode, not a second privileged path
into a daemon-owned vault. External agents should continue to use the
[authenticated HTTP contract](agents/integration.md).
