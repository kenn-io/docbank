# docbank

[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/kenn-io/docbank/actions/workflows/ci.yml/badge.svg)](https://github.com/kenn-io/docbank/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kenn-io/docbank?include_prereleases)](https://github.com/kenn-io/docbank/releases)

> **Alpha software.** Docbank is pre-1.0. Keep independent copies of
> irreplaceable material and verify backups before relying on them.

Keep the documents. Change the filing system.

Docbank is a local-first document archive for PDFs, scans, notes,
spreadsheets, images, and other files. It stores immutable, deduplicated bytes
by SHA-256 and gives them a virtual directory tree that can be reorganized
without moving the underlying content.

It is designed to be useful both directly and as document infrastructure for
other software. Humans use the CLI; agents and external applications use the
same authenticated HTTP API. The daemon is the only process that opens the
vault, so every client observes one authority for metadata, content, and
maintenance.

## Why docbank?

- **Organization is not storage.** Rename and move documents through metadata
  transactions while their content identity remains stable.
- **Import is safe to repeat.** Recursive ingestion copies rather than modifies
  sources, records provenance, resumes cleanly, and deduplicates by content.
- **Deletion is deliberate.** `rm` is recoverable. Emptying trash, collecting
  unreachable content, and compacting dead packed space are separate steps.
- **Integrity is visible.** Verify live content by hash, stream bytes with
  transfer evidence, and create incremental snapshots that can be verified and
  restored into a separate vault.
- **Agents get a real contract.** Stable node IDs, revisions, bounded lists,
  structured errors, dry-run maintenance, OpenAPI, and byte-identity proofs
  support safe automation without direct database access.

## Implemented today

- Virtual folders with listing, tree browsing, rename, move, trash, and restore
- Resumable bulk import and verified multipart upload
- FTS5 name search (document-body extraction and search are planned)
- Mixed loose and packed content storage with explicit pack, GC, and repack
- Whole-vault integrity verification
- Incremental backup repositories with create, list, verify, and confined restore
- Daemon-first CLI, authenticated loopback HTTP API, and offline OpenAPI output
- A release pipeline for Linux, macOS, and Windows on amd64 and arm64, with
  SHA-256 checksums and checksum-enforcing installers
- Native vault, daemon, and recovery support on Linux, macOS, and Windows

Versioned content editing, indelible audited history, tags, watched inboxes,
text extraction, the kit-ui web portal, and the focused TUI are planned rather
than implemented. See the [roadmap](docs/roadmap.md) for the current boundary.

## Installation

Docbank supports Linux, macOS, and 64-bit Windows on amd64 and arm64. On Linux
or macOS, install the latest release with:

> **Current release:** v0.1.0 predates the complete distribution pipeline. Its
> archives cover Linux amd64/arm64 and macOS arm64 only. Windows and macOS
> amd64 users must build from source until the next release is published; the
> installers fail rather than substitute an incompatible archive.

```bash
curl -fsSL https://raw.githubusercontent.com/kenn-io/docbank/main/scripts/install.sh | sh
```

On Windows, run this in PowerShell:

```powershell
irm https://raw.githubusercontent.com/kenn-io/docbank/main/scripts/install.ps1 | iex
```

Both installers select the native archive and refuse to install it unless its
SHA-256 digest matches the release's `SHA256SUMS`. You can instead download and
verify an archive manually from
[GitHub Releases](https://github.com/kenn-io/docbank/releases).

To build from source, install Go 1.26+, CGO, and a C compiler:

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make install
```

On Windows, build `docbank.exe` with
`go build -tags fts5 -o docbank.exe ./cmd/docbank`; the
[setup guide](docs/setup.md) covers the platform toolchain.
The [setup guide](docs/setup.md) is the installation authority.

## Quick start

There is no initialization command. The first data command creates the local
vault and starts its daemon:

```bash
docbank add ~/Documents --dest /archive
docbank tree /archive
docbank search "tax return"
docbank mv /archive/Documents/receipt.pdf /archive/Documents/receipt-2026.pdf
docbank verify
```

Create and prove an incremental backup repository:

```bash
docbank backup init --repo ~/Backups/docbank
docbank backup create --repo ~/Backups/docbank --tag first-import
docbank backup verify --repo ~/Backups/docbank
docbank backup restore --repo ~/Backups/docbank --target ~/Restores/docbank-test
```

## Documentation

- [Setup](docs/setup.md) and [ten-minute quickstart](docs/quickstart.md)
- [Using docbank](docs/usage/lifecycle.md), including backup and recovery
- [Docbank for agents](docs/agents.md) and the detailed
  [integration guide](docs/agents/integration.md)
- [CLI reference](docs/cli-reference.md)
- [Architecture overview](docs/architecture/overview.md)
- [Internal design map](docs/internal/README.md) for contributors and coding agents

Docbank belongs to the same family of personal data tools as
[msgvault](https://msgvault.io). Msgvault preserves communications history;
Docbank provides a durable, reorganizable home for ordinary documents and a
safe document API for external applications.
