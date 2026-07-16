---
title: Embedding Docbank in Go
description: Own one or more independently rooted Docbank vaults inside a Go application, with CGO or pure-Go SQLite.
---

# Embedding Docbank in Go

Go applications can use Docbank as an in-process document store without
starting or discovering a daemon. An embedded vault uses the same virtual tree,
immutable content versions, content-addressed blob store, packed-storage
authority, and exclusive hierarchy locking as a standalone vault.

Use an embedded vault when the application itself should own document
lifecycle. Use the HTTP API when independent processes need to share one
standalone vault.

## Open a vault

Each root is an independent archive. One process may open several non-overlapping
roots at once; overlapping roots are rejected so two owners cannot mutate the
same storage tree.

```go
package archive

import (
    "bytes"
    "context"
    "io"

    "go.kenn.io/docbank/pkg"
)

func StoreSession(ctx context.Context, root string, sessionID string, jsonl []byte) error {
    vault, err := docbank.Open(ctx, docbank.OpenOptions{Root: root})
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

`vault.ID()` returns the archive's stable UUID. JSONL backup and restore
preserve that identity even when the restored vault has a different filesystem
root; applications can therefore distinguish logical archives without treating
paths as identity.

An `OpenContent` stream is not authoritative until it reaches terminal `io.EOF`
or `Verify` succeeds. Early `Close` does not drain the stream. `Vault.Close`
waits for active operations and streams, closes storage, and releases the vault
lock.

## List and pack content

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

Pages contain directories first and files second, name-sorted within each
kind. A zero limit uses `DefaultChildrenLimit`; one call cannot exceed
`MaxChildrenLimit`. The total and page come from one metadata snapshot, but a
caller that needs a complete stable traversal must avoid concurrent tree
mutations between page calls.

Ordinary `Put` calls publish loose content. Call `Pack` explicitly when the
embedded owner is ready to move authorized loose blobs into managed immutable
packs:

```go
report, err := vault.Pack(ctx, docbank.PackOptions{MaxBytes: 256 << 20})
if err != nil {
    return err
}
if report.BudgetExhausted {
    // Run another bounded pass when scheduling allows.
}
```

`MaxBytes` is a soft committed raw-byte budget: the pass finishes the blob that
crosses the budget, seals its pack, and stops. Zero is unlimited. The report
includes packing, reconciliation, missing/corrupt content, and orphan cleanup
outcomes; embedded applications should surface those fields rather than
treating a nil error alone as a complete health report. Packing changes only
physical representation. `OpenContent` keeps the same verified read contract.

## Choose SQLite

The build default preserves performance where CGO is available:

- CGO builds use `github.com/mattn/go-sqlite3`.
- `CGO_ENABLED=0` builds use `modernc.org/sqlite`.

An application may select either adapter explicitly:

```go
import (
    "go.kenn.io/docbank/pkg"
    "go.kenn.io/docbank/pkg/sqlite/modernc"
)

vault, err := docbank.Open(ctx, docbank.OpenOptions{
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

Do not point a daemon and an embedded application at the same root. `Open`
holds the same exclusive hierarchy lock as the daemon for the entire vault
lifetime and fails if the requested root overlaps another active vault or
restore target.

The standalone CLI remains daemon-first and never opens storage directly.
Embedding is a distinct application ownership mode, not a second privileged
path into a daemon-owned vault. External agents should continue to use the
[authenticated HTTP contract](agents/integration.md).
