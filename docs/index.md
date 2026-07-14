---
title: docbank
description: Local-first document archive with immutable content, flexible organization, verified backup and recovery, and an agent-ready HTTP API.
---

<p class="eyebrow">LOCAL-FIRST DOCUMENT ARCHIVE</p>

# Keep the documents. Change the filing system.

docbank gives PDFs, scans, notes, spreadsheets, and images one durable home
without freezing how you organize them. Bytes are immutable and deduplicated;
the tree above them is yours to rename, move, search, trash, and restore.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="setup/">Set up docbank</a>
  <a class="md-button" href="quickstart/">Take the ten-minute tour</a>
  <a class="md-button" href="agents/">Use docbank with agents</a>
</p>

<div class="signal-grid">
  <section><strong>Local</strong><span>SQLite and content-addressed bytes under your control</span></section>
  <section><strong>Durable</strong><span>Crash-ordered writes, verification, trash, and explicit GC</span></section>
  <section><strong>Agent-ready</strong><span>The CLI and automations share one authenticated HTTP contract</span></section>
</div>

docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io) (communications archive) and fotobank
(photo/video archive). Where msgvault preserves an immutable historical
record of your messages, docbank manages **working documents**: files you
still organize, retrieve, and build workflows around.

## Why docbank

Ordinary folders make their paths part of their identity. Sync tools copy that
coupling between machines. Docbank separates the durable document from its
current filing location: content has a verified SHA-256 identity, while a
transactional virtual tree supplies names and paths that can change cheaply.

That gives people a recoverable archive instead of another folder to curate,
and gives agents a bounded interface instead of unrestricted filesystem access.
Imports are repeatable, deletion proceeds through explicit stages, reads can
prove the returned bytes, and incremental snapshots can be verified and
restored before they are trusted.

## How it works

The vault owns the bytes. Documents are ingested into content-addressed
storage (one blob per unique content, named by its SHA-256), and the
organizing structure is a **virtual tree stored in SQLite**. Moves,
renames, and reorganization are metadata transactions that never touch bytes.

!!! info "Planned — versioned editing"
    The same split is designed to support versioned editing without making
    mutable bytes. See [Editing & Versions](architecture/editing-and-versions.md)
    for the planned surface.

!!! info "Planned — full audit"
    Selected directory scopes will retain every authoritative change and
    content version with sticky membership, tamper-evident history, and
    complete backup/restore fidelity. See
    [Audited History](architecture/audited-history.md).

```
docbank add ~/Documents/taxes --dest /taxes   # bulk import, resumable
docbank tree /taxes                           # browse the virtual tree
docbank search "insurance"                    # indexed name search
docbank mv "/inbox/scan (2).pdf" /taxes/2026  # reorganize, metadata only
docbank rm /inbox/junk.pdf                    # trash, recoverable
docbank trash empty --run                     # permanently delete trash metadata
docbank gc --run                              # reclaim loose; mark packed bytes dead
docbank storage repack                        # compact eligible sparse packs
docbank verify                                # prove the bytes are intact
docbank backup create --repo ~/Backups/docbank
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank
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
  the database ever references them. `rm` is always soft; permanent tree
  deletion, unreachable-content GC, and packed-space reclamation are separate
  explicit operations. `verify` re-hashes everything on demand.
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

docbank is alpha software with tagged releases. The core store and ingest
pipeline, virtual-tree CLI, authenticated daemon API, packed storage,
integrity verification, and incremental backup creation, verification, and
restore are implemented and tested. Representative-corpus hardening and
distribution work continue before a stable 1.0 release.

!!! info "Planned — later phases"
    Versioned editing, full audited history, tags, watched inboxes,
    content-text extraction and search, the kit-ui web portal, and the focused
    TUI are designed but not yet built. The
    [Roadmap](roadmap.md) tracks what exists versus what is planned.

## Where to go next

- [Setup](setup.md) — build and install the binary
- [Quickstart](quickstart.md) — a ten-minute tour of the CLI
- [Vault Lifecycle](usage/lifecycle.md) — operate, update, snapshot, and recover safely
- [Docbank for Agents](agents.md) — understand the automation contract
- [Agent Integration](agents/integration.md) — build revision-aware automations
- [Troubleshooting](troubleshooting.md) — diagnose failures without risking the vault
- [CLI Reference](cli-reference.md) — every command, flag, and output format
- [Architecture → Overview](architecture/overview.md) — how the pieces fit together
