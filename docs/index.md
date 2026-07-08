---
title: docbank
description: Personal document archive with a virtual tree over content-addressed storage, full-text search, trash and recovery, versioned editing, and an agent-first HTTP API.
---

# docbank

Own the documents that accumulate over a lifetime — PDFs, scans, text
files, spreadsheets, images — in one durable, searchable, reorganizable
vault.

docbank is the third member of a family of personal data tools alongside
[msgvault](https://msgvault.io) (communications archive) and fotobank
(photo/video archive). Where msgvault preserves an immutable historical
record of your messages, docbank manages **living documents**: files you
still rename, refile, and edit.

## How it works

The vault owns the bytes. Documents are ingested into content-addressed
storage (one blob per unique content, named by its SHA-256), and the
organizing structure is a **virtual tree stored in SQLite**. Moves,
renames, and reorganization are metadata transactions that never touch
bytes. Editing a document writes a new blob and keeps the old one as a
prior version — contents are versioned by construction.

```
docbank add ~/Documents/taxes --dest /taxes   # bulk import, resumable
docbank tree /taxes                           # browse the virtual tree
docbank search "insurance"                    # full-text search
docbank mv "/inbox/scan (2).pdf" /taxes/2026  # reorganize, metadata only
docbank rm /inbox/junk.pdf                    # trash, recoverable
docbank gc --run                              # reclaim unreferenced bytes
docbank verify                                # prove the bytes are intact
```

## Principles

- **Never lose a byte.** Blob writes are durable (fsync discipline) before
  the database ever references them. Deletion is soft; nothing is
  reclaimed except by an explicit `gc --run`. `verify` re-hashes
  everything on demand.
- **Ingest never touches sources.** Importing copies; it never deletes or
  modifies the original files.
- **IDs are canonical, paths are convenience.** Every listing shows node
  IDs; trash recovery and (in future) the HTTP API operate on IDs so
  renames can't strand a reference.
- **Agents are first-class.** The planned HTTP API exposes everything the
  CLI and TUI can do, with optimistic-concurrency preconditions designed
  for agent read-modify-write loops. See [HTTP API](architecture/http-api.md).
- **Documents are mutable, history is not.** Editing replaces a node's
  content and records the prior version; old blobs remain retrievable
  until you garbage-collect them. See
  [Editing & Versions](architecture/editing-and-versions.md).

## Status

docbank is pre-release. Phase 1 (store, ingest pipeline, and the full
core CLI) is implemented and tested; the daemon, HTTP API, editing
commands, TUI, and backup are designed but not yet built. The
[Roadmap](roadmap.md) tracks what exists versus what is planned, and every
page in this documentation marks planned behavior explicitly.

## Where to go next

- [Setup](setup.md) — build and install the binary
- [Quickstart](quickstart.md) — a ten-minute tour of the CLI
- [CLI Reference](cli-reference.md) — every command, flag, and output format
- [Design → Overview](architecture/overview.md) — how the pieces fit together
