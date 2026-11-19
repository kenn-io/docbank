<h1 align="center">docbank</h1>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.27.4+-00ADD8?logo=go" alt="Go 1.27.4+"></a>
  <a href="https://github.com/kenn-io/docbank/actions/workflows/ci.yml"><img src="https://github.com/kenn-io/docbank/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/kenn-io/docbank/releases"><img src="https://img.shields.io/github/v/release/kenn-io/docbank?include_prereleases" alt="Release"></a>
  <a href="https://docbank.ai"><img src="https://img.shields.io/badge/Docs-docbank.ai-blue" alt="Docs"></a>
</p>

<p align="center">
  <a href="https://docbank.ai">Documentation</a> ·
  <a href="docs/setup.md">Setup Guide</a> ·
  <a href="docs/quickstart.md">Quickstart</a>
</p>

**Agent-native document system of record**

Docbank is an open-source document vault for people, applications, and agents.
It runs on your own machine. It imports files, email, photos, and recordings,
makes their text searchable, and keeps every version you save. When you need to
hand records to someone else, it exports the versions you checked.

<p align="center">
  <img src="https://docbank.ai/assets/generated/web-vault-browser.png" alt="The Docbank web application browsing a synthetic vault.">
</p>

Use it from the command line, a local web app, a terminal browser, or an
authenticated HTTP API, or connect an agent with `docbank mcp`. They all talk
to the daemon, a local background process that owns the vault. A Go application
can instead [embed a vault of its own](docs/embedding.md).

> **Alpha software.** Keep your own copies of anything irreplaceable, and
> verify a backup before you rely on it.

## What you can do

| Task | Guide |
| --- | --- |
| Import and organize records | [Importing](docs/usage/importing.md) and [tagging](docs/usage/organizing.md) |
| Find a document | [Searching](docs/usage/searching.md) and [search by meaning](docs/usage/search.md) |
| Extract a document's text and read it | [Document processing](docs/usage/document-processing.md) |
| Read email and follow attachments | [Web email reader](docs/usage/web.md#read-archived-email) |
| Browse photos and organize albums | [Photo assets](docs/usage/photos.md) |
| Export documents and search reports | [Verified bundles](docs/usage/export-bundles.md) and [search reports](docs/usage/search-exports.md) |
| Keep earlier content versions | [Editing and versions](docs/architecture/editing-and-versions.md) |
| Automate filing and retrieval | [Agent integration](docs/agents/integration.md) |
| Recover deleted documents | [Trash and recovery](docs/usage/trash-and-gc.md) |
| Create and verify backups | [Backup and restore](docs/usage/backup.md) |
| Keep a permanent record of changes | [Audited history](docs/usage/audited-history.md) |
| Add filesystem or S3-compatible storage | [Multi-store storage](docs/usage/storage.md) |

Docbank does not synchronize a working folder across devices or create public
share links. The [capability guide](docs/capabilities.md) lists what it does
today, and the [roadmap](docs/roadmap.md) lists what is planned.

## Install

Linux or macOS:

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

The installers pick the native Linux, macOS, or Windows archive for amd64 or
arm64. They refuse to install an archive whose digest does not match the
release's `SHA256SUMS`. You can also download the archives from
[GitHub Releases](https://github.com/kenn-io/docbank/releases) and verify them
yourself.

To build from source, install Go 1.27.4 or newer, CGO, a C compiler, Node 24+, and npm:

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make install
```

The [setup guide](docs/setup.md) lists the build requirements for each platform.
For a stock image configured through environment settings and mounted secrets,
see [Run in a container](docs/usage/containers.md).

## Start a vault

The first data command creates the vault and starts its daemon:

```bash
docbank document add ~/Documents --dest /archive
docbank document tree /archive
docbank search "tax return"
docbank web
```

For empty search results, see the [search hints](docs/usage/searching.md#why-did-a-text-search-find-nothing).

Retrieve a file, list its versions, replace its content, and rename it. Docbank
verifies a file before handing it back. Replacing content adds a version and
leaves the earlier ones in place, and a rename keeps the document's ID:

```bash
docbank document get /archive/Documents/receipt.pdf ./receipt.pdf
docbank document versions list /archive/Documents/receipt.pdf
docbank document put revised-receipt.pdf /archive/Documents/receipt.pdf
docbank document move /archive/Documents/receipt.pdf /archive/Documents/receipt-2026.pdf
docbank vault verify
```

PDF text is indexed locally on import, with no setup. With no configured
profile, the Text tab shows retained PDF page text. Configured profiles keep
their own text and processing state. Office text and OCR use [configured
processing profiles](docs/usage/document-processing.md).

Create a backup, verify it, and restore it into a separate directory to check:

```bash
docbank backup init --repo ~/Backups/docbank
docbank backup create --repo ~/Backups/docbank --tag first-import
docbank backup verify --repo ~/Backups/docbank
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank-test
```

The [ten-minute quickstart](docs/quickstart.md) covers versions, tags, search,
trash, maintenance, and restore.

## Who owns the vault and its storage?

- **Standalone.** One daemon owns the vault. The CLI, browser, TUI, scripts,
  and external agents all go through its authenticated API on the local
  machine.
- **Embedded.** A Go application owns each vault in its own process. Every
  vault has its own root directory, and you choose CGO or pure-Go SQLite.
- **Secondary storage.** Docbank verifies content in configured filesystem and
  S3-compatible stores but does not encrypt it. Restrict those stores to their
  owner and encrypt them at the storage layer.
- **Backup.** Copying the local database and primary blob directory while the
  daemon is stopped gives a complete backup only if primary storage has a
  verified copy of every content file the vault keeps. `docbank backup create`
  also includes content held only in secondary stores. See the
  [backup guide](docs/usage/backup.md) for the full rules.

## Documentation

- [Documentation homepage](https://docbank.ai) and [visual tour](docs/tour.md)
- [Setup](docs/setup.md) and [quickstart](docs/quickstart.md)
- [Capabilities](docs/capabilities.md) and [vault lifecycle](docs/usage/lifecycle.md)
- [Web application](docs/usage/web.md) and [terminal browser](docs/usage/tui.md)
- [Multi-store storage](docs/usage/storage.md) and [backup & restore](docs/usage/backup.md)
- [Docbank for agents](docs/agents.md) and [integration guide](docs/agents/integration.md)
- [Embed in Go](docs/embedding.md) and [document processing packages](docs/document-understanding.md)
- [CLI reference](docs/cli-reference.md) and [architecture overview](docs/architecture/overview.md)

Docbank has a sibling, [msgvault](https://msgvault.io), which archives
communications. Msgvault preserves an immutable record of messages. Docbank
manages working documents that people and agents still organize, retrieve,
version, and use.

## Contributing

Start with the [development guide](docs/internal/development.md) for the
repository layout, package ownership, generated clients, and checks. Follow
[AGENTS.md](AGENTS.md) and the [documentation guide](docs/README.md) when
updating code or documentation.

## License

Copyright 2026 Kenn Software LLC.

Docbank is licensed under the [Apache License, Version 2.0](LICENSE). See
[NOTICE](NOTICE) for attribution information.
