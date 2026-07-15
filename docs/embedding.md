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

    "go.kenn.io/docbank"
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

An `OpenContent` stream is not authoritative until it reaches terminal `io.EOF`
or `Verify` succeeds. Early `Close` does not drain the stream. `Vault.Close`
waits for active operations and streams, closes storage, and releases the vault
lock.

## Choose SQLite

The build default preserves performance where CGO is available:

- CGO builds use `github.com/mattn/go-sqlite3`.
- `CGO_ENABLED=0` builds use `modernc.org/sqlite`.

An application may select either adapter explicitly:

```go
import (
    "go.kenn.io/docbank"
    "go.kenn.io/docbank/sqlite/modernc"
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
