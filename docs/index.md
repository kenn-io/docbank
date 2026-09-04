---
title: Your documents. Your agents. One system.
description: Docbank is a self-sovereign document storage system for you and your agents, with indexed retrieval, stable identity, verifiable content, incremental recovery, and audited history.
---

<p class="eyebrow">DOCUMENT STORAGE FOR YOU AND YOUR AGENTS</p>

# Your documents. Your agents. One system.

Docbank gives you and your agents one authoritative place to file, find,
organize, version, and verify the documents you depend on. Stable identities
survive reorganization, every stored byte can be checked, and incremental
backups can be proved before you rely on them. The catalog and history remain
under your control instead of inside a provider account; verified content may
stay in the built-in local primary or be deliberately placed in fenced
filesystem and S3-compatible stores.

Work directly from the CLI, browse through the local web application or TUI,
automate through the authenticated HTTP API, or embed independently rooted
vaults in a Go application. Every surface sees the same nodes, revisions,
immutable versions, and content authority.

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

![The Docbank web application browsing a synthetic vault and showing the selected document's stable authority.](https://docbank.ai/assets/generated/web-vault-browser.png)

<p class="image-caption">A real synthetic vault in the local web application. <a href="tour/">See the visual tour.</a></p>

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
  <section>
    <h3>Storage you can place</h3>
    <p>Keep ingest local-first, then deliberately place verified content in filesystem or S3-compatible stores without changing document identity.</p>
  </section>
  <section>
    <h3>Automation that fails clearly</h3>
    <p>Stable IDs, revision preconditions, bounded pages, typed errors, and digest receipts give agents evidence instead of optimistic success.</p>
  </section>
</div>

The ordinary workflow stays direct:

```bash
docbank add ~/Documents/taxes --dest /taxes    # import a folder; sources untouched
docbank tree /taxes                            # browse the virtual tree
docbank web                                    # open the scoped local browser
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

Self-sovereignty here is practical: the vault catalog, history, placement
policy, and recovery path are under your control. The built-in primary is an
inspectable local layout. Optional secondary stores hold verified physical
copies but never become an independent document catalog. Import copies files
and never touches the sources, so moving a Dropbox or Google Drive export into
Docbank is safe to attempt and repeat until it is complete.

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

Docbank is alpha software, not yet a stable 1.0. Release archives and
checksum-enforcing installers cover Linux, macOS, and Windows on amd64 and
arm64. The [Capabilities](capabilities.md) page is the current product map;
the [Roadmap](roadmap.md) describes high-level direction rather than acting as
an implementation tracker.

Docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io), the communications archive. Where msgvault
preserves an immutable record of your messages, Docbank manages working
documents: files you still organize, retrieve, and build workflows around.

## Where to go next

- [Setup](setup.md): install the binary and create the vault
- [Quickstart](quickstart.md): a ten-minute tour of the CLI
- [Capabilities](capabilities.md): the complete human-readable product map
- [Visual Tour](tour.md): real web and terminal interfaces on synthetic data
- [Vault Lifecycle](usage/lifecycle.md): operate, snapshot, and recover safely
- [Web Application](usage/web.md): browse, search, upload, download, and manage recoverable trash
- [Docbank for Agents](agents.md): the automation contract
- [Embed in Go](embedding.md): vaults inside your own application
- [Troubleshooting](troubleshooting.md): diagnose failures without risking the vault
- [CLI Reference](cli-reference.md): every command, flag, and output format
- [How Docbank Works](architecture/overview.md): the architecture, guided
- [Source Metadata](architecture/source-metadata.md): typed facts extracted from verified originals

## License

Copyright 2026 Kenn Software LLC.

Docbank is open-source software licensed under the [Apache License, Version
2.0](license.md).
