---
title: Changelog
description: Release history.
---

# Changelog

docbank is pre-1.0; interfaces and storage migrations may still evolve.

## Unreleased

- Windows is a supported vault platform rather than a compile-only target:
  daemon discovery and detached startup, owner-private vaults, no-follow
  ingest, overlapping vault/restore exclusion, backup, restore, and self-update
  use native Windows primitives.
- Windows CI runs the complete CLI and internal suite on amd64 and arm64.
- Releases publish Linux, macOS, and Windows archives for amd64 and arm64. The
  shell and PowerShell installers select the native archive, verify it against
  `SHA256SUMS`, and reject missing, ambiguous, or invalid checksums.

## v0.1.0

Release hardening:

- `trash empty` is now a dry run unless `--run` explicitly authorizes
  permanent metadata deletion, matching `gc`'s mutation boundary; daemon
  protocol negotiation prevents an older same-version daemon from interpreting
  that dry run as the legacy destructive request
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
