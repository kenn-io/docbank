---
title: docbank
description: Personal document archive with a virtual tree over content-addressed storage, full-text search, trash and recovery, and an agent-first HTTP API.
---

<p class="eyebrow">LOCAL-FIRST DOCUMENT ARCHIVE</p>

# Keep the documents. Change the filing system.

docbank gives PDFs, scans, notes, spreadsheets, and images one durable home
without freezing how you organize them. Bytes are immutable and deduplicated;
the tree above them is yours to rename, move, search, trash, and restore.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="setup/">Set up docbank</a>
  <a class="md-button" href="quickstart/">Take the ten-minute tour</a>
  <a class="md-button" href="architecture/http-api/">Build an agent integration</a>
</p>

<div class="signal-grid">
  <section><strong>Local</strong><span>SQLite and content-addressed bytes under your control</span></section>
  <section><strong>Durable</strong><span>Crash-ordered writes, verification, trash, and explicit GC</span></section>
  <section><strong>Agent-ready</strong><span>The CLI and automations share one authenticated HTTP contract</span></section>
</div>

docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io) (communications archive) and fotobank
(photo/video archive). Where msgvault preserves an immutable historical
record of your messages, docbank manages **living documents**: files you
still rename, refile, and edit.

## How it works

The vault owns the bytes. Documents are ingested into content-addressed
storage (one blob per unique content, named by its SHA-256), and the
organizing structure is a **virtual tree stored in SQLite**. Moves,
renames, and reorganization are metadata transactions that never touch bytes.

!!! info "Planned — versioned editing"
    The same split is designed to support versioned editing without making
    mutable bytes. See [Editing & Versions](architecture/editing-and-versions.md)
    for the planned surface.

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

<div class="feature-grid">
  <section>
    <h3>Immutable content</h3>
    <p>SHA-256 identities deduplicate bytes and make integrity independently verifiable.</p>
  </section>
  <section>
    <h3>Mutable organization</h3>
    <p>Moves and renames are SQLite transactions, not filesystem rewrites.</p>
  </section>
  <section>
    <h3>Deliberate deletion</h3>
    <p>Trash, permanent tree deletion, and byte reclamation remain separate decisions.</p>
  </section>
  <section>
    <h3>One authority</h3>
    <p>The daemon owns the vault; humans, scripts, and agents use the same API.</p>
  </section>
</div>

- **Never lose a byte.** Blob writes are durable (fsync discipline) before
  the database ever references them. Deletion is soft; nothing is
  reclaimed except by an explicit `gc --run`. `verify` re-hashes
  everything on demand.
- **Ingest never touches sources.** Importing copies; it never deletes or
  modifies the original files.
- **IDs are canonical, paths are convenience.** Every listing shows node
  IDs; trash recovery and the HTTP API operate on IDs so renames can't
  strand a reference.
- **Agents are first-class.** The HTTP API exposes everything the CLI
  can do — the CLI is itself an HTTP client of it — with
  optimistic-concurrency preconditions designed for agent
  read-modify-write loops. See [HTTP API](architecture/http-api.md).
## Status

docbank is pre-release. Phase 1 (store, ingest pipeline, and the full
core CLI) and Phase 2a (the daemon, the HTTP API, the daemon-first CLI,
and self-update) are implemented and tested.

!!! info "Planned — later phases"
    Editing commands, tags, watched inboxes, text extraction (Phase 2b), the
    TUI (Phase 3), and backup (Phase 4) are designed but not yet built. The
    [Roadmap](roadmap.md) tracks what exists versus what is planned.

## Where to go next

- [Setup](setup.md) — build and install the binary
- [Quickstart](quickstart.md) — a ten-minute tour of the CLI
- [CLI Reference](cli-reference.md) — every command, flag, and output format
- [Architecture → Overview](architecture/overview.md) — how the pieces fit together
