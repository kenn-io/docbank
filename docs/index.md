---
title: Your documents. Your agents. One system.
description: Docbank is a self-sovereign document storage system for you and your agents, with indexed retrieval, stable identity, verifiable content, incremental recovery, and audited history.
---

<p class="eyebrow">DOCUMENT STORAGE FOR YOU AND YOUR AGENTS</p>

# Your documents. Your agents. One system.

Docbank gives you and your agents one authoritative place to file, find,
organize, version, and verify the documents you depend on. You keep the
authority: the vault and its history live on your own machine rather than
inside a provider account. Stable identities let people and agents reorganize
documents without losing track of them, every stored byte can be checked, and
incremental backups can be verified before you rely on them. Work directly
from the CLI, automate through the authenticated HTTP API, or embed Docbank in
a Go application. People can browse the same authority in the local web
application or focused terminal interface.

Install the latest release on Linux or macOS:

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

[Windows and source-build instructions](setup.md)

<p class="hero-actions">
  <a class="md-button md-button--primary" href="setup/">Start your vault</a>
  <a class="md-button" href="quickstart/">Ten-minute tour</a>
  <a class="md-button" href="agents/">Build agent workflows</a>
</p>

## What Docbank gives you

<div class="feature-grid">
  <section>
    <h3>Indexed local retrieval</h3>
    <p>Search document names with ranked results and browse a virtual tree without putting a cloud service between you and your archive.</p>
  </section>
  <section>
    <h3>Identity beyond the path</h3>
    <p>Move and rename documents while stable IDs and retained version history continue to identify them.</p>
  </section>
  <section>
    <h3>Integrity you can check</h3>
    <p>Every content version has a SHA-256 identity, and <code>docbank verify</code> re-hashes every stored byte on demand.</p>
  </section>
  <section>
    <h3>Recovery you can rehearse</h3>
    <p>Incremental backups reuse unchanged content, verify independently, and restore into a separate vault for inspection.</p>
  </section>
</div>

The ordinary workflow stays direct:

!!! note "Release availability"
    Tag-filtered search and `docbank get` are newer than v0.10.0;
    `docbank web` is newer than v0.10.1. Build from source to use them until
    the next release is tagged.

```bash
docbank add ~/Documents/taxes --dest /taxes    # import a folder; sources untouched
docbank tree /taxes                            # browse the virtual tree
docbank web                                    # open the local read-only browser
docbank search "insurance"                     # ranked name and verified plain-text search
docbank search "return" --tag taxes             # narrow the same ranking by stable tag identity
docbank get /taxes/2026/return.pdf ./return.pdf # verify, then publish a complete local file
docbank put revised.pdf /taxes/2026/return.pdf # add a new immutable version
docbank versions list /taxes/2026/return.pdf   # inspect retained history
docbank rm /inbox/junk.pdf                     # move to recoverable trash
docbank verify                                 # re-prove stored content
docbank backup create --repo ~/Backups/docbank # incremental snapshot
```

## Own the authority, not just the disk

Self-sovereignty here is practical: the vault, catalog, history, and recovery
path are under your control. Everything lives on your machine in an inspectable
layout. Import copies files and never touches the sources, so moving a Dropbox
or Google Drive export into Docbank is safe to attempt and repeat until it is
complete.

### Why move beyond Dropbox or Google Drive?

Cloud drives are good at synchronization and sharing. They also make a provider
account part of the authority for your archive: access, retained history, and
recovery depend on the service and its policies. Docbank is built for the copy
whose integrity and continued availability you control yourself. It does not
sync files to every device or create share links; those are deliberate
boundaries, not hidden omissions.

### Why not just put everything on a NAS?

A NAS is useful storage, and some NAS products add checksums, snapshots, search,
or replication. Those capabilities depend on the particular appliance and
filesystem, and folders still make a path do too many jobs at once. Docbank adds
a document-level contract: stable identity across moves, retained versions,
recoverable deletion, content verification, permanent audited scopes, and one
authenticated interface for people and software.

A NAS can be a good home for a Docbank [backup repository](usage/backup.md) when
you protect it with filesystem permissions and encrypted storage. The
distinction is simple: storage answers where the bytes live; Docbank records
which document they belong to, what happened to it, and whether the vault and
its backups still prove out.

## One authority for people and agents

The standalone CLI, agents, and scripts use the same authenticated loopback API;
none has a privileged shortcut into the vault. Stable IDs survive renames,
downloads carry digest evidence, and revisions expose conflicting edits.
Version pruning, trash emptying, and garbage collection begin with dry-run
previews. Generate the live contract with `docbank openapi --json`, read
[Docbank for Agents](agents.md), or follow the
[Agent Integration Guide](agents/integration.md) through a complete verified
filing workflow.

A Go application can instead own independently rooted vaults in-process through
the [embedded API](embedding.md), with the same storage model and exclusive
ownership rules.

## Guarantees you can inspect

<div class="feature-grid">
  <section>
    <h3>Immutable content</h3>
    <p>Every retained version keeps a verified SHA-256 identity, and bytes are durable before the catalog references them.</p>
  </section>
  <section>
    <h3>Deliberate lifecycle</h3>
    <p>Trash, permanent deletion, and space reclamation are separate, explicit decisions rather than side effects.</p>
  </section>
  <section>
    <h3>Verified backup &amp; restore</h3>
    <p>Incremental snapshots restore into a separate vault and are verified before they are trusted.</p>
  </section>
  <section>
    <h3>Audited history</h3>
    <p>Opt a directory into permanent, tamper-evident history that Docbank can independently re-verify. See <a href="usage/audited-history/">Permanent Audited History</a>.</p>
  </section>
</div>

## Two ways to run it

- **Standalone.** A local document system: the CLI and a daemon on your
  machine, with one authority per vault. Start with the
  [Quickstart](quickstart.md).
- **Embedded.** A Go module: independently rooted vaults in-process, with
  the same storage model and lifecycle guarantees, on CGO or pure-Go
  SQLite. See [Embed in Go](embedding.md).

## Status

Docbank is alpha software. The latest release includes archives and
checksum-enforcing installers for Linux, macOS, and Windows on amd64 and
arm64. Implemented and tested today: the core store and ingest
pipeline, the virtual-tree CLI, the authenticated daemon API, stable
content versions with verified replacement, reversion, pruning, and
lookup by content hash (`refs`), tags, permanent audited history with
independent verification, loose and packed storage with explicit
maintenance, whole-vault integrity verification, incremental backup
create/verify/restore, a read-only web application and TUI, and the embedded
Go API. Docbank is not yet a
stable 1.0; the [Roadmap](roadmap.md) gives the product direction.

Docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io), the communications archive. Where msgvault
preserves an immutable record of your messages, Docbank manages working
documents: files you still organize, retrieve, and build workflows around.

## Where to go next

- [Setup](setup.md): install the binary and create the vault
- [Quickstart](quickstart.md): a ten-minute tour of the CLI
- [Vault Lifecycle](usage/lifecycle.md): operate, snapshot, and recover safely
- [Web Application](usage/web.md): browse and search the local vault
- [Docbank for Agents](agents.md): the automation contract
- [Embed in Go](embedding.md): vaults inside your own application
- [Troubleshooting](troubleshooting.md): diagnose failures without risking the vault
- [CLI Reference](cli-reference.md): every command, flag, and output format
- [How Docbank Works](architecture/overview.md): the architecture, guided

## License

Copyright 2026 Kenn Software LLC.

Docbank is open-source software licensed under the [Apache License, Version
2.0](license.md).
