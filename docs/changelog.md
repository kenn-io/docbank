---
title: Changelog
description: Release history.
---

# Changelog

docbank is pre-1.0; interfaces and storage migrations may still evolve.

## Unreleased

- The store extends audited recording to filesystem ingest, content
  reversion, in-scope moves and renames, reversible trash and restore, and
  tag creation, assignment, rename, and deletion; backup restore and JSONL import
  independently replay and validate that history.
- Enrollment of a vault's first audit scope is available through
  `docbank audit enable` and `docbank audit status`, with a preview-first,
  token-acknowledged workflow; additional scopes remain unavailable.
- Bounded per-node audit history exposes canonical path, content, tag, and
  provenance changes, while `docbank audit verify` independently replays the
  authority, checks protected bytes, and proves current chains extend a
  separately recorded evidence report.

## v0.5.0 (2026-07-17)

- The embedded Go API is exposed directly from the module root.
- Verification classifies unavailable embedded content separately, and
  embedded content opens report missing or physically size-mismatched bytes
  as `ErrContentUnavailable` without conflating metadata lookup failures.
- The store records audited content replacements and inherited audited node
  creation.

## v0.4.0 (2026-07-17)

- Embedded Go applications can page immutable content history with `Versions`
  and open any historical version through the verified read contract with
  `OpenVersionContent`.
- The initial audit enrollment authority persists for durable integrity
  verification, with portable JSON representations of audit authority data.
- Provenance is stabilized as audit authority with canonical record shapes
  and collection ordering.

## v0.3.0 (2026-07-16)

- Edit documents interactively with verified, versioned content replacement.
- List, replace, revert, and explicitly prune immutable content versions.
- Add, remove, list, and use stable user-facing tags.
- Look up documents by authoritative content hash.
- Embed independently rooted vaults in Go applications with selectable SQLite
  drivers, packing, and bounded child traversal.
- Preflight large imports without modifying content and stream ingest
  progress to the terminal.
- View supervised daemon background jobs and their status.
- Stable vault and document identities are preserved across ingest and
  metadata operations, and daemon reliability is strengthened by supervising
  background work.
- Explicitly named cloud-storage roots are honored during ingest.

## v0.2.1 (2026-07-13)

- Create, list, verify, and restore incremental backups with portable JSONL
  metadata.
- Inspect packed storage, pack loose objects, reclaim sparse packs, and
  recover safely from interrupted pack retirement.
- Upload remote content with digest checks and verify content identity over
  HTTP.
- Run the complete Docbank CLI and daemon lifecycle natively on Windows,
  with Windows CI covering the complete CLI and internal suite on amd64 and
  arm64.
- Stream verified content with separate limits for loose and packed objects
  to keep memory and disk usage predictable.
- Install checksum-verified binaries for Linux, macOS, and Windows on amd64
  and arm64; the shell and PowerShell installers verify archives against
  `SHA256SUMS` and reject missing, ambiguous, or invalid checksums.

## v0.2.0 (2026-07-13)

- Create incremental backups with authoritative JSONL metadata and
  daemon-owned snapshots; verify repositories and restore through a
  confined, exclusive recovery workflow.
- Inspect packed storage, pack loose objects, and reclaim sparse packs with
  explicit maintenance commands.
- Upload remote content with digest checking and retrieve content through
  verifiable streaming reads.
- Stream content and backups within bounded resource limits, with separate
  limits for loose and packed objects.
- Run the complete Docbank lifecycle natively on Windows: daemon discovery
  and detached startup, owner-private vaults, no-follow ingest, overlapping
  vault/restore exclusion, backup, restore, and self-update use native
  Windows primitives.
- Install using prebuilt artifacts and installers for Linux, macOS, and
  Windows on amd64 and arm64.
- Preserve recoverability when obsolete pack files cannot be retired
  immediately.

## v0.1.0 (2026-07-09)

Release hardening:

- `trash empty` is now a dry run unless `--run` explicitly authorizes
  permanent metadata deletion, matching `gc`'s mutation boundary; daemon
  protocol negotiation prevents an older same-version daemon from
  interpreting that dry run as the legacy destructive request
- `search --limit` controls the bounded result set and both the API and CLI
  report when more matches exist
- Updated the shared `go.kenn.io/kit` dependency to v0.4.0

Phase 2a (Infrastructure) complete:

- `docbank daemon` (`run`/`start`/`status`/`restart`/`stop`) on
  `go.kenn.io/kit` lifecycle primitives: discovery, auto-start, idle
  shutdown, exclusive vault lock for the daemon's lifetime
- Huma v2 HTTP API under `/api/v1`: stat, list, content, search,
  create-directory, ingest, move, trash/restore, trash-empty, gc, verify
- Every CLI data command rewritten as an HTTP client of the daemon, with
  auto-start on first use; no command opens the vault directly anymore
- `config.toml` (`[server]`, `[web]`), with bind/key validation at
  daemon startup
- `docbank update`: self-update from GitHub releases, coordinating
  daemon stop/replace/restart
- `docbank openapi`: offline OpenAPI document
- Tag-driven release workflow: Linux (amd64/arm64) and macOS (arm64)
  archives with SHA256 checksums

Phase 1 (Core) complete:

- Virtual tree store (SQLite, FTS5) with schema-enforced invariants
- Content-addressed blob store with durable write discipline
- Idempotent recursive import with collision suffixing and provenance
- Full core CLI: `add`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`,
  `search`, `trash list|empty`, `gc`, `verify`
- Trash → empty → GC deletion pipeline with `verify` integrity checking
- Inter-process vault locking (shared for commands, exclusive for `gc`)
