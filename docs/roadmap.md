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
| 0 | Extract msgvault's pack/backup and packed-CAS engines into `go.kenn.io/kit` | **Implemented** (Docbank uses `kit` v0.9.2) |
| 1 | Core: store, blob store, ingest pipeline, full CLI | **Implemented** |
| 2a | Infrastructure: daemon, HTTP API, daemon-first CLI, self-update, release pipeline | **Implemented** |
| 2b | Features: versioned editing, full audit, tags, watched inboxes, text extraction, ingest provenance | Designed |
| 3 | Primary kit-ui web portal and focused operator TUI | Designed |
| 4 | Backup commands over the kit engine | **Implemented**; representative-corpus hardening continues |

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
- Inter-process vault locking (`flock` on Unix, `LockFileEx` on Windows)
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
- Daemon background-task supervision with shared cancellation, bounded
  shutdown before storage closes, failure capture, and observable
  `docbank jobs` / `GET /api/v1/jobs` status. Watched inbox behavior remains
  Phase 2b work on top of this implemented lifecycle.
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
- Tag-driven release pipeline building archives plus `SHA256SUMS` for Linux,
  macOS, and Windows on amd64 and arm64
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
- Full-audit directory scopes with sticky membership, complete authoritative
  change and content-version retention, tamper-evident history, maintenance
  refusal, and portable JSONL fidelity
  ([design](architecture/audited-history.md))
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

## Phase 3 — Human applications

The kit-ui web portal is the primary human interface over the authenticated
daemon API: virtual-tree browsing, search, import, metadata/provenance, audited
history timelines and comparisons, trash, storage, backup, and observable jobs.
Application-neutral tree, timeline, diff, evidence, and job components should
be reusable by Msgvault and later tools.

The focused TUI is the terminal companion: tree navigation, search, ingest and
job progress, metadata/evidence, move/trash/restore, storage and backup state,
plus a compact tree/history/detail audit browser. Rich document comparison
belongs in external tools or the web portal. Neither client has privileged
operations — anything either does, the API can do.

## Phase 4 — Backup (implemented)

`docbank backup init|create|list|verify|restore` against the kit engine
([design](architecture/backup.md)).

The internal Kit application adapter, loose/packed capture proof, and atomic
packed restore publication are implemented. Every new internal capture uses
Docbank's deterministic logical JSONL artifact, which round-trips directory
structure, stable node IDs, content membership, trash state, versions,
provenance, tags, and extraction state into a fresh current-schema database.
Historical SQLite page-map snapshots remain restorable. Authenticated daemon
API and CLI orchestration for repository initialization, snapshot creation,
listing, verification, and confined packed restore are implemented. Remaining
Phase 4 work is representative-corpus hardening and eventual retention policy,
not a missing recovery command.

## Deferred beyond v1

OCR of scans, embeddings/AI tagging, at-rest encryption of the live store,
encryption for backup repositories, importing attachments out of msgvault,
multi-user/sharing, and an MCP server wrapping the API.
