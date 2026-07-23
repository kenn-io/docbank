---
title: Changelog
description: Release history.
---

# Changelog

This page records user-visible changes in every tagged release. Dates come
from the annotated release tags. Docbank remains pre-1.0, so public interfaces
may still evolve, but vaults created by v0.9.0 and later are within the
[storage compatibility boundary](architecture/storage.md#released-upgrades).

## [v0.11.0](https://github.com/kenn-io/docbank/tree/v0.11.0) — 2026-07-23

### New features

- Open and browse a vault in a local web application.
- Explore documents and audited history in a read-only analytical TUI.
- Schedule bounded automatic vault packing.
- Manage multiple permanent audit scopes.
- Inspect nodes, create virtual directories, and download documents from the
  CLI.
- View document provenance through the CLI and embedded Go API.
- Search by directory, tag, media type, and modification time.
- Access physical write receipts through the embedded Go API.

### Improvements

- Reset vaults fail closed when storage safety cannot be confirmed.
- Record shared tag changes across all applicable audit scopes.
- Report effective watched-inbox status.
- Delay watched-file ingestion until files are old enough to be stable.
- Publish verified downloads atomically without leaving partial files.
- Report maintenance lock contention immediately.
- Display concise human-readable timestamps.

## [v0.10.1](https://github.com/kenn-io/docbank/tree/v0.10.1) — 2026-07-22

### New features

- Browse permanent audit history across an entire scope.
- Reorganize multiple documents atomically, so every move succeeds or none do.
- View the currently selected vault with `docbank info`.

### Improvements

- Show physical byte totals in the loose-blob packing backlog.
- Preserve operator-approved Markdown in annotated release tags and make
  release creation an explicit, human-reviewed workflow.

## [v0.10.0](https://github.com/kenn-io/docbank/tree/v0.10.0) — 2026-07-21

### New features

- Add bounded embedded APIs for document ingestion, retrieval, traversal, and
  maintenance.
- Add immutable embedded content-addressed storage APIs.
- Accept stable node IDs as reusable CLI selectors across document operations.

### Improvements

- Compress worthwhile new loose blobs while preserving their logical SHA-256
  identities and verified-read contract.
- Bound `tree` output and report truncation so large vaults remain manageable
  for people and agents.

## [v0.9.0](https://github.com/kenn-io/docbank/tree/v0.9.0) — 2026-07-19

### New features

- Search inside verified UTF-8 plain text, Markdown, JSON, and JSONL content,
  not only document names and metadata.

### Project changes

- License Docbank under the Apache License 2.0.
- Establish v0.9.0 as the first released storage-compatibility boundary.

## [v0.8.1](https://github.com/kenn-io/docbank/tree/v0.8.1) — 2026-07-19

- Publish a patch-level conformance-bootstrap release from the same source as
  v0.8.0. This gave updater checks two releases that both implement
  `docbank version`, allowing the complete update path to be exercised.

## [v0.8.0](https://github.com/kenn-io/docbank/tree/v0.8.0) — 2026-07-19

- Make CLI and daemon version reporting explicit and consistent through
  `docbank version`, `docbank daemon version`, and compatible-daemon checks.

## [v0.7.0](https://github.com/kenn-io/docbank/tree/v0.7.0) — 2026-07-19

- Automatically ingest stable regular files from configured watched inboxes.
- Preserve source identity without modifying the watched files, and append
  later byte changes as immutable versions of the same document.
- Expose watcher lifecycle and failures through supervised daemon jobs.

## [v0.6.0](https://github.com/kenn-io/docbank/tree/v0.6.0) — 2026-07-19

### Permanent audited history

- Permanently enroll a directory tree in auditing with a preview-first,
  acknowledged workflow.
- Record and browse audited filesystem ingest, content replacement and revert,
  moves and renames, trash and restore, and tag creation, assignment, rename,
  and deletion.
- Independently replay protected authority with `docbank audit verify` and
  prove that current chains extend a separately recorded evidence report.
- Preserve and validate audited history through backup, JSONL export, and
  restore.

### Automation

- Return structured receipts for virtual-tree mutations and typed JSON from
  established read commands.
- Publish stable CLI exit codes for usage errors, missing objects, stale state,
  busy resources, and integrity findings.

## [v0.5.0](https://github.com/kenn-io/docbank/tree/v0.5.0) — 2026-07-17

- Expose the embedded Go API directly from the module root.
- Create audit authority through production enrollment.
- Record audited content replacements and inherited audited node creation.
- Classify unavailable embedded content separately during verification, and
  report missing or size-mismatched bytes as `ErrContentUnavailable`.

## [v0.4.0](https://github.com/kenn-io/docbank/tree/v0.4.0) — 2026-07-17

- Page immutable content history and open any historical version through the
  embedded API's verified-read contract.
- Persist the initial audit enrollment authority for durable integrity
  verification.
- Support portable JSON representations of audit authority.
- Stabilize provenance as audit authority with canonical record shapes and
  deterministic collection ordering.

## [v0.3.0](https://github.com/kenn-io/docbank/tree/v0.3.0) — 2026-07-16

### Document workflows

- Edit documents interactively with verified, versioned content replacement.
- List, replace, revert, and explicitly prune immutable content versions.
- Add, remove, list, and use stable user-facing tags.
- Find documents by authoritative content hash.
- Preflight large imports without opening content, stream ingest progress, and
  honor explicitly named cloud-storage roots.

### Embedded and daemon operation

- Embed independently rooted vaults in Go applications with selectable SQLite
  drivers, packing, and bounded child traversal.
- Preserve stable vault and document identities across ingest and metadata
  operations.
- Supervise daemon background jobs and expose their current state.

## [v0.2.1](https://github.com/kenn-io/docbank/tree/v0.2.1) — 2026-07-13

This recovery release carried the v0.2.0 capabilities through a validated
six-platform release build after v0.2.0's tag did not produce a published
GitHub release.

- Create, list, verify, and restore incremental backups with portable JSONL
  metadata.
- Inspect packed storage, pack loose objects, reclaim sparse packs, and recover
  safely from interrupted pack retirement.
- Upload remote content with digest checks and verify content identity over
  HTTP.
- Run the complete Docbank CLI and daemon lifecycle natively on Windows.
- Stream verified loose and packed content within separate resource limits.
- Install checksum-verified archives on Linux, macOS, and Windows, on both
  amd64 and arm64.
- Validate all release builders before tagging and allow a recovery release to
  span an intervening unpublished tag.

## [v0.2.0](https://github.com/kenn-io/docbank/tree/v0.2.0) — 2026-07-13

### Backup and packed storage

- Create daemon-owned incremental snapshots with authoritative JSONL metadata.
- Verify backup repositories and restore through a confined, exclusive
  recovery workflow.
- Inspect packed storage, pack loose objects, and reclaim sparse packs with
  explicit maintenance commands.
- Preserve recoverability when an obsolete pack cannot be retired immediately.

### Portable clients and releases

- Upload remote content with digest checking and retrieve content through
  verifiable, bounded streaming reads.
- Run the complete daemon, CLI, lock, backup, restore, and self-update lifecycle
  with native Windows behavior.
- Build precompiled archives and installers for Linux, macOS, and Windows on
  amd64 and arm64.

## [v0.1.0](https://github.com/kenn-io/docbank/tree/v0.1.0) — 2026-07-09

### Core vault

- Store documents in a virtual SQLite/FTS5 tree backed by a durable
  content-addressed blob store.
- Import directory trees idempotently, preserve provenance, and suffix genuine
  filename collisions without modifying source files.
- Browse, search, move, trash, restore, empty trash, garbage-collect, and
  verify documents from the CLI.
- Keep permanent deletion explicit: `trash empty` and `gc` preview unless the
  operator authorizes the mutation.

### Daemon-first operation

- Run every standalone CLI data command through an auto-started,
  authenticated, loopback-only daemon rather than opening the vault directly.
- Expose the versioned HTTP API for tree, content, search, ingest, mutation,
  reclamation, and verification workflows.
- Coordinate daemon discovery, idle shutdown, and exclusive vault ownership.
- Configure server and web behavior in `config.toml`, generate an offline
  OpenAPI document, and self-update from verified GitHub releases.
- Bound search with `--limit` and report when additional matches exist.
- Publish Linux amd64/arm64 and macOS arm64 archives with SHA-256 checksums.
