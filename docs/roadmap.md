---
title: Roadmap
description: What is implemented today and what each phase adds.
---

# Roadmap

docbank ships in phases, each independently useful. This page is the
authoritative record of what exists versus what is designed — anything
marked planned appears elsewhere in these docs only under an explicit
"Planned" admonition.

| Phase | Scope | Status |
|-------|-------|--------|
| 0 | Extract msgvault's pack/backup and packed-CAS engines into `go.kenn.io/kit` | **Implemented** (`kit` v0.7.0) |
| 1 | Core: store, blob store, ingest pipeline, full CLI | **Implemented** |
| 2a | Infrastructure: daemon, HTTP API, daemon-first CLI, self-update, release pipeline | **Implemented** |
| 2b | Features: versioned editing, tags, watched inboxes, text extraction, ingest provenance | Designed |
| 3 | TUI file browser | Designed |
| 4 | Backup commands over the kit engine | Designed |

## Implemented (Phase 1)

- Virtual tree store with schema-enforced invariants (single root,
  live-sibling name uniqueness, no cycles, NFC name normalization,
  revision bumps)
- Content-addressed blob store with full fsync durability discipline
- Idempotent, resumable bulk import with collision suffixing and
  provenance
- FTS5 name search, ranked and operator-safe
- Trash / restore / `trash empty`, explicit unreachable-content GC, and
  separate packed-space reclamation
- `gc` (dry-run default) and `verify`
- Inter-process vault locking (shared/exclusive flock)
- CLI: `add`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`, `search`,
  `trash`, `gc`, `verify`

## Phase 2a — Infrastructure (implemented)

- `docbank daemon` (`run`/`start`/`status`/`restart`/`stop`): a single
  daemon owns the vault; discovery, auto-start, idle shutdown, and
  PID-reuse-safe lifecycle on `go.kenn.io/kit` primitives
  ([design](architecture/daemon.md))
- Huma v2 HTTP API under `/api/v1` implementing stat, list, content,
  search, create-directory, ingest, move, trash/restore, trash-empty,
  gc, and verify — the CLI's data commands are HTTP clients of this
  surface, with no other path into the vault
  ([design](architecture/http-api.md))
- The vault lock becomes a single exclusive holder (the daemon) instead
  of Phase 1's per-command shared/exclusive split; an in-daemon
  maintenance gate replaces `gc`'s own exclusive acquisition
  ([design](architecture/locking.md))
- `config.toml` for the daemon's listen address, API key, idle timeout,
  and web placeholder toggle ([design](configuration.md))
- `docbank update`: self-update from GitHub releases via
  `kit/selfupdate`, coordinating daemon stop/replace/restart
- `docbank openapi`: offline OpenAPI document for agents and client
  generation
- Tag-driven release pipeline building archives plus `SHA256SUMS` for
  Linux (amd64/arm64) and macOS (arm64)
- A handwritten placeholder web page at `/`, naming the vault and
  linking to `/docs`
- Internal mixed loose/packed blob storage on `kit/packstore`: docbank's
  `blobs` rows remain the read-authority boundary, existing loose vaults
  open without conversion, and GC/verify operate through the shared
  physical store

`docbank storage status` reports loose, live-packed, and dead-packed inventory,
and `docbank storage pack` performs explicit optionally budgeted packing through
the daemon. `docbank storage repack` compacts eligible sparse packs and retires
dead source files. This is the complete ordinary storage-maintenance surface;
Kit unpack remains internal to tests, migrations, or a future purpose-built
recovery workflow rather than a planned user command. Ordinary ingests continue
to publish loose blobs; startup never performs an implicit migration.

## Phase 2b — Features (designed)

Implemented foundation: file-node API responses expose immutable SHA-256
identity, content streams carry catalog identity plus a freshly computed digest
trailer, and a revision-bound single-node endpoint verifies stored bytes. This
evidence contract now backs file-granular multipart upload: remote writers must
declare SHA-256 and size, and receive the stable node plus server-computed
identity only after both match.

- Editing surfaces: `PUT` content, `docbank edit`/`put`/`revert`, and
  `versions` listing ([design](architecture/editing-and-versions.md))
- Tags surfaced in CLI, search filters, and the API; `POST /batch/move`
  bulk reorganization
- Watched inbox directories with a stability window, landing imports
  under `/inbox/<date>/`
- Text extraction workers (PDF text layer, plain text/markdown, office
  formats) feeding content search
- External integration surface: generalized ingest provenance —
  today's `provenance` table records the original filesystem path and
  mtime; `source_kind` / `source_ref` / `source_meta` fields extend it
  to non-file origins (a watched inbox, another application's archive)
  as generic fields, never application-specific tables; and node lookup
  by content hash, so an external system of record holding
  `node_id` + hash references can ask "is this already in the vault?" in
  one call. To settle before the refs schema exists: whether external
  references pin nodes against `trash empty`/`gc`, or dangling-ref
  detection stays the referrer's job (docbank guarantees only that node
  ids are never reused)

## Phase 3 — TUI

Bubble Tea file manager over the same store: navigate, search, rename,
move, trash/restore, version list, open-in-default-app. No privileged
operations — anything the TUI does, the API can do.

## Phase 4 — Backup

`docbank backup init|create|list|verify|restore` against the kit engine
([design](architecture/backup.md)).

The internal Kit application adapter, loose/packed capture proof, and atomic
packed restore publication are implemented. Every new internal capture uses
Docbank's deterministic logical JSONL artifact, which round-trips directory
structure, stable node IDs, content membership, trash state, versions,
provenance, tags, and extraction state into a fresh current-schema database.
Historical SQLite page-map snapshots remain restorable. Command/API
orchestration remains.

## Deferred beyond v1

OCR of scans, embeddings/AI tagging, a web UI, at-rest encryption of the
live store (encrypted *backups* come free with the pack layer),
importing attachments out of msgvault, multi-user/sharing, and an MCP
server wrapping the API.
