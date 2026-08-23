# docbank

[![Go 1.27+](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/kenn-io/docbank/actions/workflows/ci.yml/badge.svg)](https://github.com/kenn-io/docbank/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kenn-io/docbank?include_prereleases)](https://github.com/kenn-io/docbank/releases)

> **Alpha software.** Keep independent copies of irreplaceable material and
> verify backups before relying on them.

**Your documents. Your agents. One system.**

Docbank is a self-sovereign document system for the records you and your
agents need to keep, find, change, and prove. It combines a familiar virtual
tree with stable document IDs, immutable content versions, indexed retrieval,
recoverable deletion, verified backup, and optional permanent audited history.
The vault catalog stays under your control instead of inside a provider
account.

![The Docbank web application browsing a synthetic vault and showing the selected document's stable authority.](https://raw.githubusercontent.com/kenn-io/docbank/docs-assets/screenshots/v0.12.0/web-vault-browser.png)

The standalone CLI, web application, TUI, scripts, and agents all use the same
authenticated daemon contract. Go applications can instead embed independently
rooted vaults in-process through the public module at `go.kenn.io/docbank`.

## Why Docbank?

A path is a useful place to find a file, but a poor long-term identity. Cloud
drives also make account access and provider policy part of the authority for
your archive. Docbank separates those concerns:

- a stable node ID continues to identify a document after moves and renames;
- every content version is immutable and named by a verifiable SHA-256 digest;
- revisions turn stale automation into explicit conflicts instead of silent
  overwrites;
- trash, permanent deletion, garbage collection, and pack reclamation are
  separate decisions;
- incremental backups are verified before restore results are published; and
- physical content can be placed in fenced filesystem or S3-compatible stores
  without making those stores the document catalog.

Docbank is an archive and system of record, not a sync-and-share service. It
does not mirror a working folder across devices or create public share links.

## What you can do

| Need | Docbank capability |
| --- | --- |
| File and find records | Recursive import, verified upload, virtual folders, tags, ranked name and extracted-text search |
| Keep identity through change | Stable node IDs, immutable version UUIDs, verified replacement, reversion, and explicit version pruning |
| Work safely with agents | Authenticated HTTP and OpenAPI, bounded listings, structured errors, revision preconditions, and digest receipts |
| Recover from mistakes | Recoverable trash, revision-bound restore, explicit GC and repack, whole-vault verification |
| Prove recovery | Incremental snapshot repositories, complete content verification, topology-independent restore |
| Retain a permanent record | Preview-first audited scopes with sticky retention and independently replayed evidence |
| Manage physical capacity | Loose and packed storage, automatic bounded packing, and deliberate multi-store placement, repair, salvage, and evacuation |

See the [capability guide](docs/capabilities.md) for the full product map and
the [visual tour](docs/tour.md) for the current web and terminal interfaces.

## Install

Linux or macOS:

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

The installers select the native Linux, macOS, or Windows archive for amd64 or
arm64 and refuse to install it unless its digest matches the release's
`SHA256SUMS`. [GitHub Releases](https://github.com/kenn-io/docbank/releases)
also provides the archives for manual verification.

To build from source, install Go 1.27+, CGO, a C compiler, Node 24+, and npm:

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make install
```

The [setup guide](docs/setup.md) is the toolchain authority for every platform.

## Start a vault

There is no initialization ceremony. The first data command creates the vault
and starts its daemon:

```bash
docbank add ~/Documents --dest /archive
docbank tree /archive
docbank search "tax return"
docbank web
```

Retrieve a complete file only after Docbank verifies it, then inspect or change
the same stable document without rewriting prior content:

```bash
docbank get /archive/Documents/receipt.pdf ./receipt.pdf
docbank versions list /archive/Documents/receipt.pdf
docbank put revised-receipt.pdf /archive/Documents/receipt.pdf
docbank mv /archive/Documents/receipt.pdf /archive/Documents/receipt-2026.pdf
docbank verify
```

Create and prove an incremental recovery point:

```bash
docbank backup init --repo ~/Backups/docbank
docbank backup create --repo ~/Backups/docbank --tag first-import
docbank backup verify --repo ~/Backups/docbank
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank-test
```

The [ten-minute quickstart](docs/quickstart.md) walks through versions, tags,
search, recoverable trash, maintenance, and restore.

## Deployment and trust boundaries

- **Standalone:** one daemon owns a vault; every CLI, browser, TUI, script, and
  external agent goes through its loopback-authenticated API.
- **Embedded:** one Go application owns each independently rooted vault
  in-process, with selectable CGO or pure-Go SQLite.
- **Secondary storage:** Docbank verifies content in configured filesystem and
  S3-compatible stores but does not encrypt it. Protect those namespaces with
  owner access controls and storage encryption appropriate to their operator.
- **Backup:** a stopped copy of the local database and primary blob directory
  is complete only when every retained blob still has primary authority.
  `docbank backup create` remains complete across remote-only placement and is
  the preferred portable recovery path.

## Documentation

- [Documentation homepage](https://docbank.ai) and [visual tour](docs/tour.md)
- [Setup](docs/setup.md) and [quickstart](docs/quickstart.md)
- [Capabilities](docs/capabilities.md) and [vault lifecycle](docs/usage/lifecycle.md)
- [Web application](docs/usage/web.md) and [terminal browser](docs/usage/tui.md)
- [Multi-store storage](docs/usage/storage.md) and [backup & restore](docs/usage/backup.md)
- [Docbank for agents](docs/agents.md) and [integration guide](docs/agents/integration.md)
- [Embed in Go](docs/embedding.md)
- [CLI reference](docs/cli-reference.md) and [architecture overview](docs/architecture/overview.md)

Docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io), the communications archive. Msgvault
preserves an immutable record of messages; Docbank manages working documents
that people and agents still organize, retrieve, version, and use.

## License

Copyright 2026 Kenn Software LLC.

Docbank is licensed under the [Apache License, Version 2.0](LICENSE). See
[NOTICE](NOTICE) for attribution information.
