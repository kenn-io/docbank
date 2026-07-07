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
| 0 | Extract msgvault's pack/backup engine into `go.kenn.io/kit` | Done; final upstream merge in progress |
| 1 | Core: store, blob store, ingest pipeline, full CLI | **Implemented** |
| 2 | Daemon: `serve` + HTTP API, watched inboxes, text extraction, editing commands | Designed |
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
- Trash / restore / `trash empty` with the three-stage deletion pipeline
- `gc` (dry-run default) and `verify`
- Inter-process vault locking (shared/exclusive flock)
- CLI: `add`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`, `search`,
  `trash`, `gc`, `verify`

## Phase 2 — Daemon + API + Editing

- `docbank serve`: Huma v2 HTTP API with typed OpenAPI contract,
  API-key auth, generated client ([design](architecture/http-api.md))
- Editing surfaces: `PUT` content, `docbank edit`/`put`/`revert`, and
  `versions` listing ([design](architecture/editing-and-versions.md))
- Watched inbox directories with a stability window, landing imports
  under `/inbox/<date>/`
- Text extraction workers (PDF text layer, plain text/markdown, office
  formats) feeding content search
- Tags surfaced in CLI, search filters, and the API
- `config.toml` for daemon settings

## Phase 3 — TUI

Bubble Tea file manager over the same store: navigate, search, rename,
move, trash/restore, version list, open-in-default-app. No privileged
operations — anything the TUI does, the API can do.

## Phase 4 — Backup

`docbank backup init|create|list|verify|restore` against the kit engine
([design](architecture/backup.md)).

## Deferred beyond v1

OCR of scans, embeddings/AI tagging, a web UI, at-rest encryption of the
live store (encrypted *backups* come free with the pack layer),
importing attachments out of msgvault, multi-user/sharing, and an MCP
server wrapping the API.
