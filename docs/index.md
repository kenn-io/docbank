---
title: A permanent home for the documents of your life
description: Docbank gives the documents you keep for life one permanent home on your own machine — easy to file, easy to find, with version history, provable integrity, and verified backups.
---

<p class="eyebrow">EVERY DOCUMENT YOU KEEP FOR LIFE, ON YOUR MACHINE</p>

# A permanent home for the documents of your life

Tax returns, contracts, medical records, scans of things you can't
replace — Docbank keeps them all in one place on your own machine. File
anything, find it again in seconds, reorganize without breaking anything,
and go back to earlier versions when you need them. It's built so that
decades from now, everything is still there — and you can prove it.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="setup/">Start your archive</a>
  <a class="md-button" href="quickstart/">Ten-minute tour</a>
  <a class="md-button" href="agents/">Build agent workflows</a>
</p>

## What you get

<div class="feature-grid">
  <section>
    <h3>Find what you filed</h3>
    <p>Ranked search across every name in your archive, plus a tree you can browse. Filed once means findable later.</p>
  </section>
  <section>
    <h3>Reorganize without fear</h3>
    <p>Move and rename freely. Documents keep their identity, their history follows them, and nothing breaks or goes missing.</p>
  </section>
  <section>
    <h3>Go back to earlier versions</h3>
    <p>Replace a file and the old versions stay until you decide otherwise. Deleting sends to trash first, never straight to gone.</p>
  </section>
  <section>
    <h3>Backups you can actually trust</h3>
    <p>Incremental backups you can verify and practice restoring before the day you need them.</p>
  </section>
</div>

Install with one command (Linux and macOS; see [Setup](setup.md) for
Windows and source builds):

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

Then filing looks like this:

```bash
docbank add ~/Documents/taxes --dest /taxes    # import a folder; sources untouched
docbank tree /taxes                            # see what you have
docbank search "insurance"                     # find a document by name
docbank put revised.pdf /taxes/2026/return.pdf # newer version; the old one stays
docbank versions /taxes/2026/return.pdf        # every version, on record
docbank rm /inbox/junk.pdf                     # to the trash, recoverable
docbank verify                                 # prove every stored byte is intact
docbank backup create --repo ~/Backups/docbank # incremental, verifiable backup
```

## Why not just folders?

Ordinary folders make a document's location its identity: move a file and
everything pointing at it breaks; reorganize and you lose track of what
you had. Cloud drives add a second problem — your archive's history and
continued existence depend on an account in good standing. For records
that outlive laptops, jobs, and subscriptions — tax filings, contracts,
medical records, research — that is the wrong foundation.

Docbank keeps everything on your machine in a layout you can inspect:
your documents plus the catalog that organizes them. Importing copies
your files and never touches the originals, so migrating a Dropbox or
Google Drive export is safe to attempt and repeat until it is complete.
`docbank verify` re-proves every stored byte on demand. And a backup
repository you have verified and practiced restoring means your archive's
survival no longer depends on any company's goodwill.

One honest boundary: docbank is an archive, not a sync-and-share tool. It
doesn't put files on every device or make share links — it makes the copy
that must survive trustworthy.

## Ready for your agents, too

The same vault welcomes automation. Agents and scripts file, retrieve,
reorganize, verify, and run maintenance through the same authenticated
API the CLI uses — there is no privileged shortcut into the vault.
Documents keep stable IDs that survive renames, reads carry verified
bytes, conflicting edits are detected instead of silently overwritten,
and destructive operations offer dry runs. Read
[Docbank for Agents](agents.md), or follow the
[Agent Integration Guide](agents/integration.md) through a complete
filing workflow.

## The guarantees

<div class="feature-grid">
  <section>
    <h3>Immutable content</h3>
    <p>Every version keeps a verified SHA-256 identity, and bytes are durable before the catalog references them.</p>
  </section>
  <section>
    <h3>Deliberate lifecycle</h3>
    <p>Trash, permanent deletion, and space reclamation are separate, explicit decisions — never side effects.</p>
  </section>
  <section>
    <h3>Verified backup &amp; restore</h3>
    <p>Incremental snapshots restore into a separate vault and are verified before they are trusted.</p>
  </section>
  <section>
    <h3>Audited history</h3>
    <p>Opt a directory into permanent, tamper-evident history that docbank can independently re-verify. See <a href="usage/audited-history/">Permanent Audited History</a>.</p>
  </section>
</div>

## Two ways to run it

- **Standalone.** A personal archive: the CLI and a daemon on your
  machine, one authority per vault. Start with the
  [Quickstart](quickstart.md).
- **Embedded.** A Go module: independently rooted vaults in-process, with
  the same storage model and lifecycle guarantees, on CGO or pure-Go
  SQLite. See [Embed in Go](embedding.md).

## Status

docbank is alpha software. The current release is v0.6.0, with archives
and checksum-enforcing installers for Linux, macOS, and Windows on amd64
and arm64. Implemented and tested today: the core store and ingest
pipeline, the virtual-tree CLI, the authenticated daemon API, stable
content versions with verified replacement, reversion, pruning, and
lookup by content hash (`refs`), tags, permanent audited history with
independent verification, loose and packed storage with explicit
maintenance, whole-vault integrity verification, incremental backup
create/verify/restore, and the embedded Go API. docbank is not yet a
stable 1.0; the [Roadmap](roadmap.md) gives the product direction.

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
