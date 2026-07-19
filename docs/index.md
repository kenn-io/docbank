---
title: Keep the documents. Change the filing system.
description: Local-first document system of record for people and agents, with stable identities, verified bytes, revision-safe automation, and proven recovery.
---

<p class="eyebrow">LOCAL-FIRST DOCUMENT SYSTEM OF RECORD</p>

# Keep the documents. Change the filing system.

Docbank gives people and agents one trustworthy place to work with documents.
People get a permanent, verifiable home for the PDFs, scans, notes,
spreadsheets, and records they keep for life. Agents get an authenticated
contract with stable identities, verified bytes, conflict-safe mutations, and
explicit lifecycle controls — without opening SQLite or interpreting a storage
directory. Content is immutable and deduplicated; the virtual tree above it is
yours to reshape. Run a personal archive, connect an agent over HTTP, or embed
independently rooted vaults inside a Go system.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="setup/">Start your archive</a>
  <a class="md-button" href="agents/">Build agent workflows</a>
  <a class="md-button" href="embedding/">Embed in Go</a>
</p>

## Built for people. Ready for agents.

An agent needs more than permission to read and write a folder. It needs to
know which document it acted on, whether the bytes are exact, whether its view
became stale, and what a destructive operation would do before it runs.
Docbank makes those questions part of the product contract.

<div class="feature-grid">
  <section>
    <h3>Identity that survives reorganization</h3>
    <p>Stable node IDs survive moves and renames. Every immutable content version has its own UUID, SHA-256 identity, and size.</p>
  </section>
  <section>
    <h3>Bytes proven, not assumed</h3>
    <p>Remote writes declare hash and size. Reads carry recorded content authority plus a digest computed while streaming.</p>
  </section>
  <section>
    <h3>Mutations that detect stale decisions</h3>
    <p>Revisions and <code>If-Match</code> turn conflicting read-modify-write work into an explicit response instead of a silent overwrite.</p>
  </section>
  <section>
    <h3>Operations made for automation</h3>
    <p>OpenAPI, JSON output, bounded reads, structured errors, dry runs, progress events, audit evidence, and verified backups replace prose scraping.</p>
  </section>
</div>

The CLI is a client of the same authenticated loopback API exposed to agents;
it has no privileged shortcut into the vault. Generate the live contract with
`docbank openapi --json`, read [Docbank for Agents](agents.md), or follow the
[Agent Integration Guide](agents/integration.md) through a complete verified
filing workflow.

Install with one command (Linux and macOS; see [Setup](setup.md) for
Windows and source builds):

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

## Why docbank exists

Ordinary folders make a document's location part of its identity: move a
file and every reference to it breaks; reorganize and you lose track of
what you had. Cloud drives add a second coupling — your archive's
integrity, history, and continued existence depend on an account in good
standing. Neither makes durable content identity, independent
verification, and provider-independent retention part of its native
contract. For records that outlive laptops, jobs, and subscriptions — tax
filings, contracts, medical records, research — that is the wrong
foundation.

## Own your documents

Everything docbank holds lives on your machine in an inspectable layout: a
blob store plus a SQLite database. Each document's content is deduplicated
under one logical SHA-256 identity, stored loose or inside managed packs
under the vault's catalog authority. Loose content hashes directly with
standard tools; packed content is retrieved and verified through docbank
itself — `docbank verify` re-proves every byte on demand, and reads carry
digest evidence. Import copies and never touches sources, so migrating a
Dropbox or Google Drive export into docbank is safe to attempt and repeat
until it is complete. A backup repository you have verified and
test-restored means your archive's survival no longer depends on any
company's goodwill.

One honest boundary: docbank is an archive and system of record, not a
sync-and-share tool. It doesn't put files on every device or make share
links — it makes the copy that must survive trustworthy.

## What docbank is

The vault owns the bytes. Content is deduplicated under its SHA-256
identity, and the organizing structure is a **virtual tree stored in
SQLite**: moves, renames, and reorganization are metadata transactions
that never rewrite content. A standalone vault is owned by one daemon,
and the CLI, agents, and scripts all use its authenticated HTTP API. A Go
application can instead embed independently rooted vaults in-process — no
daemon, same storage model and exclusive-ownership rules.

```bash
docbank add ~/Documents/taxes --dest /taxes    # bulk import, resumable
docbank tree /taxes                            # browse the virtual tree
docbank search "insurance"                     # ranked name search
docbank put revised.pdf /taxes/2026/return.pdf # new version, priors kept
docbank versions /taxes/2026/return.pdf        # stable content history
docbank rm /inbox/junk.pdf                     # trash, recoverable
docbank verify                                 # prove the bytes are intact
docbank backup create --repo ~/Backups/docbank # incremental snapshot
```

## Four commitments

<div class="feature-grid">
  <section>
    <h3>Immutable content</h3>
    <p>Every version keeps a verified SHA-256 identity; bytes are durable before the database references them.</p>
  </section>
  <section>
    <h3>Deliberate lifecycle</h3>
    <p>Trash, permanent deletion, garbage collection, and pack compaction are separate, explicit decisions.</p>
  </section>
  <section>
    <h3>Verified backup &amp; restore</h3>
    <p>Incremental snapshots restore into a separate vault and are verified before they are trusted.</p>
  </section>
  <section>
    <h3>Audited history</h3>
    <p>Opt a directory into permanent, tamper-evident history, then prove current authority extends evidence you recorded earlier.</p>
  </section>
</div>

- **Immutable content.** Blob writes are durable (fsync discipline) before
  the database ever references them, and every content version keeps a
  verified SHA-256 identity you can re-check at any time with `verify`.
- **Deliberate lifecycle.** `rm` is always recoverable trash. Emptying
  trash, collecting unreachable content, and compacting packed space are
  separate explicit operations — never side effects. Import never modifies
  or deletes source files.
- **Verified backup & restore.** Incremental snapshot repositories support
  create, list, verify, and confined restore into a separate vault, so a
  backup is proven before it is trusted.
- **Audited history.** Opt a directory into permanent, tamper-evident
  history: every change — ingest, replacement, reversion, moves, trash and
  restore, tag changes — is recorded and `audit verify` independently replays
  it, checks every protected blob, and proves current authority extends a
  previously recorded evidence bundle.
  Enroll a vault's first audit scope with `docbank audit enable` (newer
  than the v0.5.0 release; build from source to use it today); see
  [Permanent Audited History](usage/audited-history.md).

## Two ways to run it

- **Standalone.** A personal archive: the CLI and a daemon on your
  machine, one authority per vault. Start with the
  [Quickstart](quickstart.md).
- **Embedded.** A Go module: independently rooted vaults in-process, with
  the same storage model and lifecycle guarantees, on CGO or pure-Go
  SQLite. See [Embed in Go](embedding.md).

And for automation: agents and scripts use the same authenticated HTTP
contract the CLI uses, with IDs that survive renames and revision
preconditions for safe read-modify-write. See
[Docbank for Agents](agents.md).

## Status

docbank is alpha software. The current release is v0.5.0, with archives
and checksum-enforcing installers for Linux, macOS, and Windows on amd64
and arm64. Implemented and tested today: the core store and ingest
pipeline, the virtual-tree CLI, the authenticated daemon API, stable
content versions with verified replacement, reversion, pruning, and
lookup by content hash (`refs`), tags, permanent audit enrollment and node
history with protected-content evidence, loose and packed storage with
explicit maintenance, whole-vault integrity verification, incremental
backup create/verify/restore, and the embedded Go API. The public audit
workflow is newer than v0.5.0 and not yet in a tagged release; it is available
from a source build. docbank is not yet a stable 1.0; the
[Roadmap](roadmap.md) gives the product direction.

docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io) (communications archive) and fotobank
(photo/video archive). Where msgvault preserves an immutable record of
your messages, docbank manages working documents: files you still
organize, retrieve, and build workflows around.

## Where to go next

- [Setup](setup.md) — install the binary and create the vault
- [Quickstart](quickstart.md) — a ten-minute tour of the CLI
- [Vault Lifecycle](usage/lifecycle.md) — operate, snapshot, and recover safely
- [Docbank for Agents](agents.md) — the automation contract
- [Embed in Go](embedding.md) — vaults inside your own application
- [Troubleshooting](troubleshooting.md) — diagnose failures without risking the vault
- [CLI Reference](cli-reference.md) — every command, flag, and output format
- [How Docbank Works](architecture/overview.md) — the architecture, guided
