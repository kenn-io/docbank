---
title: Changelog
description: Release history.
---

# Changelog

docbank is pre-release; nothing has been versioned or announced yet.

## Unreleased

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
