---
title: Design Overview
description: The virtual tree over content-addressed storage, and how the components fit together.
---

# Design Overview

docbank separates **what a document is** from **where it lives and what
it's called**. Bytes go into an immutable content-addressed store;
identity, naming, hierarchy, versions, and history live in SQLite. Every
higher layer — CLI today; daemon, HTTP API, and TUI later — goes through
the same store package, so no surface has privileged operations.

```mermaid
flowchart TD
    CLI["CLI (Phase 1)"] --> STORE
    API["HTTP API (Phase 2)"] --> STORE
    TUI["TUI (Phase 3)"] --> STORE
    subgraph vault ["~/.docbank"]
        STORE["store: SQLite virtual tree<br/>nodes · versions · provenance · FTS"]
        BLOBS["blob store: blobs/&lt;aa&gt;/&lt;sha256&gt;<br/>immutable, deduplicated"]
    end
    STORE -. "references by hash" .-> BLOBS
    INGEST["ingest pipeline"] --> STORE
    INGEST --> BLOBS
    CLI --> INGEST
    API --> INGEST
```

## The two-layer split

**Blobs are immutable.** A blob's name is the SHA-256 of its content;
two ingested copies of the same file are one blob. Blobs are written
durably (temp file → fsync → rename → directory fsync) *before* any
database row references them, so a committed reference always points at
real bytes. Blobs are never modified — "editing" a document means
writing a new blob (see
[Editing & Versions](editing-and-versions.md)).

**The tree is mutable, transactionally.** Nodes (directories and files)
form the hierarchy users see. Moves, renames, trash, restore, and
content replacement are single SQLite transactions over metadata. The
store enforces the tree invariants — single root, live-sibling name
uniqueness, no cycles, validated NFC-normalized names — mostly *in the
schema itself* (partial unique indexes, CHECK constraints), so they hold
against every future writer, not just today's code paths.

This split is what makes docbank cheap to reorganize (moving a folder of
scans is a metadata transaction) and safe to deduplicate (identity is
content, so re-imports converge instead of duplicating).

## Contrast with msgvault

msgvault archives an immutable historical record: a message, once
synced, never changes. docbank manages **living documents** — the whole
point is that you keep renaming, refiling, and editing them. The designs
share the storage discipline (content-addressed bytes, SQLite metadata,
the same durability rules, and the same backup engine from
`go.kenn.io/kit`), but they diverge on mutability:

| | msgvault | docbank |
|---|---|---|
| Content contract | Immutable once archived | Editable; every edit is versioned |
| Organizing structure | Fixed (accounts, folders, threads from source) | Free-form virtual tree, user- and agent-reorganized |
| Deletion | Staged deletion *from the source* (Gmail) | Trash → empty → GC pipeline inside the vault |
| History | The archive *is* the history | `node_versions` chain per document |

## Component responsibilities

- **`internal/store`** — SQLite schema and every tree operation. Typed
  sentinel errors (`ErrNotFound`, `ErrExists`, `ErrCycle`, …) that the
  CLI prints and the future API maps to HTTP status codes.
- **`internal/blob`** — content-addressed file store with the fsync
  discipline; knows nothing about the tree.
- **`internal/ingest`** — the single import pipeline all entry points
  share (CLI now; watcher and HTTP upload later): hash → durable blob →
  one metadata transaction per file.
- **`internal/home`** — vault directory layout and the inter-process
  advisory lock ([Concurrency & Locking](locking.md)).
- **`cmd/docbank`** — thin cobra commands; no business logic.

## Build phases

Each phase ships independently useful software; the
[Roadmap](../roadmap.md) tracks status.

0. **Extraction** — msgvault's pack/backup engine generalized into
   `go.kenn.io/kit` (shared with docbank). Done, pending final merge.
1. **Core** — store, ingest, full CLI. **Implemented.**
2. **Daemon + API** — `serve`, HTTP API, watched inboxes, text
   extraction, editing commands.
3. **TUI** — Bubble Tea file browser over the same store.
4. **Backup** — the kit backup engine wired to docbank's schema.
