---
title: Roadmap
description: What is implemented today and what each phase adds.
---

# Roadmap

docbank ships in independently useful increments. This page is a high-level
public view of current capability and product direction, not an execution
ledger. The repository's kata ledger is the source of truth for actionable
work, ordering, blockers, and completion state. Durable future contracts appear
elsewhere only when they materially explain the design and are marked
"Planned."

| Phase | Scope | Status |
|-------|-------|--------|
| 0 | Extract msgvault's pack/backup and packed-CAS engines into `go.kenn.io/kit` | **Implemented** (Docbank uses `kit` v0.11.0) |
| 1 | Core: store, blob store, ingest pipeline, full CLI | **Implemented** |
| 2a | Infrastructure: daemon, HTTP API, daemon-first CLI, self-update, release pipeline | **Implemented** |
| 2b | Features: content versions, versioned editing, full audit, tags, watched inboxes, text extraction, ingest provenance | **In progress**: versions, tags, queryable provenance, watched inboxes, disjoint audit scopes, and bounded plain-text extraction implemented; PDF/Office extraction remains |
| 3 | Primary kit-ui web portal and focused operator TUI | **In progress**: read-only analytical tree/search/detail and audited-history browsing implemented in both |
| 4 | Backup commands over the kit engine | **Implemented**; representative-corpus hardening continues |

## Implemented (Phase 1)

- Virtual tree store with schema-enforced invariants (single root,
  live-sibling name uniqueness, no cycles, NFC name normalization,
  revision bumps)
- Content-addressed blob store with full fsync durability discipline
- Idempotent, resumable bulk import with collision suffixing and
  provenance
- FTS5 search over names and verified UTF-8 text content, ranked and operator-safe
- Trash / restore / `trash empty`, explicit unreachable-content GC, and
  separate packed-space reclamation
- `gc` (dry-run default) and `verify`
- Inter-process vault locking (`flock` on Unix, `LockFileEx` on Windows)
- CLI: `add`, `provenance`, `mkdir`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`, `search`,
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
  `docbank jobs` / `GET /api/v1/jobs` status. Watched inboxes and bounded text
  extraction now run through this implemented lifecycle.
- The vault lock becomes a single exclusive holder (the daemon) instead
  of Phase 1's per-command shared/exclusive split; an in-daemon
  maintenance gate replaces `gc`'s own exclusive acquisition
  ([design](architecture/locking.md))
- `config.toml` for the daemon's listen address, API key, idle timeout,
  and embedded web-application toggle ([design](configuration.md))
- `docbank update`: self-update from GitHub releases via
  `kit/selfupdate`, coordinating daemon stop/replace/restart
- `docbank openapi`: offline OpenAPI document for agents and client
  generation
- Tag-driven release pipeline building archives plus `SHA256SUMS` for Linux,
  macOS, and Windows on amd64 and arm64
- A responsive kit-ui web application at `/`, launched through `docbank web`
  with session-local authentication and read-only tree, search, sorting, and
  current-authority inspection
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

## Phase 2b — Features (in progress)

Implemented foundation: every initial ingest/upload creates a stable UUIDv4
content version and current node pointer. `docbank versions list|show|cat`,
bounded HTTP listing, ID-addressed metadata/byte retrieval, GC reachability,
verification evidence, and deterministic JSONL backup/restore all carry that
identity through loose and packed storage. File-node responses expose current
version plus immutable SHA-256 identity; content streams carry both catalog
identities plus a freshly computed digest trailer. Remote writers must declare
SHA-256 and size, and receive the stable node/version plus server-computed
identity only after both match. `docbank put`, `docbank edit`, and raw
`PUT /nodes/{id}/content` now add a digest-checked `content_replace` head under
an optimistic revision precondition while retaining every prior version.
`docbank revert` and `POST /nodes/{id}/revert` add a metadata-only
`content_revert` head from one prior version without copying its loose or packed
blob. `docbank versions prune` and `POST /nodes/{id}/versions/prune` provide
preview-first individual, age, count, and complete-prior-history selection,
with revert dependency handling and honest GC/repack consequences. `docbank
refs` and `GET /content-references` resolve a SHA-256 identity to
every retaining current, historical, or trashed node/version pair. Stable tags
are available across the CLI, authenticated API, typed client, OpenAPI, and
metadata-v1 backup/restore authority, including bounded forward and reverse
listings. Transactional batch move validates and applies bounded swaps and
nested reorganizations as one final-state operation across the CLI, API, typed
client, and audited history. Permanent audit enrollment is preview-first across the
CLI and API. The first scope creates one vault-wide genesis; later disjoint
scopes reuse it and begin independent scope chains. Sticky membership, supported logical mutations, allocation and
scope chains, status evidence, and JSONL backup/restore validation are
implemented. Canonical audit history is available by node path, stable node ID,
or stable scope ID with bounded, append-stable cursor pagination. Independent verification returns
stable terminal evidence, checks every protected blob, and can prove that
current allocation and scope chains extend an externally recorded bundle.
Daemon-owned watched inboxes recursively observe configured local directories,
wait for stable size and modification time plus an optional minimum source age,
preserve portable source identity, and append later changes to the same stable
node without touching source files. Their effective source, destination, timing
policy, exclusions, and live job state are inspectable together through the CLI
and authenticated API.

- Overlapping audit scopes
  ([current workflow](usage/audited-history.md),
  [model](architecture/audited-history.md))
- Additional text extraction workers for PDF text layers and office formats;
  bounded UTF-8 text, Markdown, JSON, and JSONL extraction is implemented
- External integration surface: embedded immutable creation accepts generic
  source kind, description, opaque reference, and optional modification time,
  and exposes those portable facts through the root Go API. Standalone local
  ingestion still presents its source as a filesystem path. To settle before
  an external-reference schema exists: whether external
  references pin nodes against `trash empty`/`gc`, or dangling-ref
  detection stays the referrer's job (docbank guarantees only that node
  ids are never reused)

## Phase 3 — Human applications

The kit-ui web portal is the primary human interface over the authenticated
daemon API. Its first read-only slice implements responsive virtual-tree
browsing, analytical sorting, name and extracted-text search, and complete
current document authority. Protected nodes also expose their permanent
newest-first audit timeline and the complete stable identity and before/after
state of every event. Future slices cover import, metadata/provenance,
historical comparison, trash, storage, backup, and observable jobs.
Application-neutral tree, timeline, diff, evidence, and job components should
be reusable by Msgvault and later tools.

The focused TUI now has a read-only first slice for virtual-tree navigation,
name and extracted-content search, stable document/version/hash detail, and a
compact node-focused audited-history timeline with complete event inspection.
Later slices add ingest and job progress, metadata/evidence,
move/trash/restore, and storage and backup state. Rich document comparison belongs in
external tools or the web portal. Neither client has privileged operations —
anything either does, the API can do.

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
