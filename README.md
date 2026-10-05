# docbank

[![Go 1.27+](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/kenn-io/docbank/actions/workflows/ci.yml/badge.svg)](https://github.com/kenn-io/docbank/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kenn-io/docbank?include_prereleases)](https://github.com/kenn-io/docbank/releases)

> **Alpha software.** Keep your own copies of anything irreplaceable, and
> verify a backup before you rely on it.

**Agent-native document system of record**

Docbank is an open-source document vault for people, applications, and agents.
It runs on your own machine. It imports files, email, photos, and recordings,
makes their text searchable, and keeps every version you save. When you need to
hand records to someone else, it exports the versions you checked.

![The Docbank web application browsing a synthetic vault.](https://docbank.ai/assets/generated/web-vault-browser.png)

Use it from the command line, a local web app, a terminal browser, or an
authenticated HTTP API, or connect an agent with `docbank mcp`. They all talk
to the daemon, a local background process that owns the vault. A Go application
can instead [embed a vault of its own](docs/embedding.md).

## What can I use it for?

| Task | Guide |
| --- | --- |
| Import and organize records | [Importing](docs/usage/importing.md) and [tagging](docs/usage/organizing.md) |
| Find a document | [Searching](docs/usage/searching.md) and [search by meaning](docs/usage/search.md) |
| Extract a document's text and read it | [Document processing](docs/usage/document-processing.md) |
| Read email and follow attachments | [Web email reader](docs/usage/web.md#read-archived-email) |
| Group RAW, JPEG, and XMP files | [Photo assets](docs/usage/photos.md) |
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

To build from source, install Go 1.27+, CGO, a C compiler, Node 24+, and npm:

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make install
```

The [setup guide](docs/setup.md) lists the build requirements for each platform.

## Start a vault

The first data command creates the vault and starts its daemon:

```bash
docbank add ~/Documents --dest /archive
docbank tree /archive
docbank search "tax return"
docbank web
```

Retrieve a file, list its versions, replace its content, and rename it. Docbank
verifies a file before handing it back. Replacing content adds a version and
leaves the earlier ones in place, and a rename keeps the document's ID:

```bash
docbank get /archive/Documents/receipt.pdf ./receipt.pdf
docbank versions list /archive/Documents/receipt.pdf
docbank put revised-receipt.pdf /archive/Documents/receipt.pdf
docbank mv /archive/Documents/receipt.pdf /archive/Documents/receipt-2026.pdf
docbank verify
```

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

## Telemetry

Docbank sends anonymous usage events by default:

- `daemon_started` and `daemon_active` each time the daemon starts.
- `daemon_active` once a day while the daemon runs.
- `app_opened` when the web app opens, about once a day per browser and daemon
  run.

Each event carries a random per-vault install ID, the version and commit, and
the operating system and architecture. Events never include document content,
names, paths, or queries. Set `DOCBANK_TELEMETRY_ENABLED=0` to turn
telemetry off. See
[anonymous usage telemetry](docs/configuration.md#anonymous-usage-telemetry).

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

## Contributors

Thanks to everyone whose work went into v0.15.0:

- [Joi Ito (@Joi)](https://github.com/Joi): macOS cloud-placeholder preflight and download guidance.
- [Marius van Niekerk (@mariusvniekerk)](https://github.com/mariusvniekerk): API clients, MCP integration, CI, and shared search and embedding components.
- [Rod Boev (@rodboev)](https://github.com/rodboev): import and format support, recordings, photos, metadata, and performance.
- [Rusty Shackleford (@salmonumbrella)](https://github.com/salmonumbrella): document processing, search and review, email, reports, and exports.
- [Wes McKinney (@wesm)](https://github.com/wesm): storage, backups, the web workspace, performance, and release integration.

See the [release contribution history](https://github.com/kenn-io/docbank/compare/v0.14.0...v0.15.0)
and [all contributors](https://github.com/kenn-io/docbank/graphs/contributors).

## License

Copyright 2026 Kenn Software LLC.

Docbank is licensed under the [Apache License, Version 2.0](LICENSE). See
[NOTICE](NOTICE) for attribution information.
