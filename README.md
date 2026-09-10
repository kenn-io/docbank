# docbank

[![Go 1.27+](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/kenn-io/docbank/actions/workflows/ci.yml/badge.svg)](https://github.com/kenn-io/docbank/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kenn-io/docbank?include_prereleases)](https://github.com/kenn-io/docbank/releases)

> **Alpha software.** Keep independent copies of irreplaceable material and
> verify backups before relying on them.

**Keep, find, and recover documents in a vault you control.**

Docbank is an open-source document vault for people, applications, and agents.
Import files, organize them in folders, and search their names and extracted
text. Move or rename a document without changing its ID. Save new versions
without overwriting earlier ones, and verify backups before you need them.

![The Docbank web application browsing a synthetic vault.](https://docbank.ai/assets/generated/web-vault-browser.png)

Use the command line, local web app, terminal browser, or authenticated HTTP
API. A local background process, the daemon, owns the vault and handles these
requests. Go applications can also [embed separately rooted vaults](docs/embedding.md).

## What can I use it for?

| Task | Guide |
| --- | --- |
| Import and organize records | [Importing](docs/usage/importing.md) and [tagging](docs/usage/organizing.md) |
| Find a document | [Searching](docs/usage/searching.md) |
| Keep earlier content versions | [Editing and versions](docs/architecture/editing-and-versions.md) |
| Automate filing and retrieval | [Agent integration](docs/agents/integration.md) |
| Recover deleted documents | [Trash and recovery](docs/usage/trash-and-gc.md) |
| Create and verify backups | [Backup and restore](docs/usage/backup.md) |
| Retain a permanent record of changes | [Audited history](docs/usage/audited-history.md) |
| Add filesystem or S3-compatible storage | [Multi-store storage](docs/usage/storage.md) |

Docbank does not synchronize a working folder across devices or create public
share links. See [capabilities](docs/capabilities.md) for the product overview
and [roadmap](docs/roadmap.md) for planned work.

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

The [setup guide](docs/setup.md) lists the build requirements for each platform.

## Start a vault

The first data command creates the vault and starts its daemon:

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

Create a backup, verify it, and restore a separate copy to inspect:

```bash
docbank backup init --repo ~/Backups/docbank
docbank backup create --repo ~/Backups/docbank --tag first-import
docbank backup verify --repo ~/Backups/docbank
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank-test
```

The [ten-minute quickstart](docs/quickstart.md) walks through versions, tags,
search, recoverable trash, maintenance, and restore.

## Who owns the vault and its storage?

- **Standalone:** one daemon owns a vault; every CLI, browser, TUI, script, and
  external agent goes through its authenticated API on the local machine.
- **Embedded:** one Go application owns each independently rooted vault
  in-process, with selectable CGO or pure-Go SQLite.
- **Secondary storage:** Docbank verifies content in configured filesystem and
  S3-compatible stores but does not encrypt it. Protect those namespaces with
  owner access controls and storage encryption appropriate to their operator.
- **Backup:** a stopped copy of the local database and primary blob directory
  is complete only when primary storage has a verified copy of every retained
  content file.
  Use `docbank backup create` to include retained content held only in secondary
  stores. See the [backup guide](docs/usage/backup.md) for the full rules.

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
