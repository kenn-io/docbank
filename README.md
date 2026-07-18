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
authenticated HTTP API. Go applications can instead embed independently rooted
vaults in-process. In either mode exactly one owner holds a vault and its
metadata, content, and maintenance authority.

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
- Stable content-version UUIDs, verified replacement and reversion, bounded
  history listing, ID-addressed retrieval, and lookup by content hash
- Stable tags with define, rename, assign, unassign, delete, and bounded
  node/tag listings
- Preview-first permanent audit enrollment for one directory scope, with
  sticky retention, recorded supported mutations, status evidence, bounded
  per-node history, and backup/restore fidelity
- FTS5 name search (document-body extraction and search are planned)
- Mixed loose and packed content storage with explicit pack, GC, and repack
- Whole-vault integrity verification
- Incremental backup repositories with create, list, verify, and confined restore
- Daemon-first CLI, authenticated loopback HTTP API, and offline OpenAPI output
- An embedded Go API with independently locked vaults, bounded tree listing,
  explicit packing, and selectable CGO or pure-Go SQLite
- A release pipeline for Linux, macOS, and Windows on amd64 and arm64, with
  SHA-256 checksums and checksum-enforcing installers
- Native vault, daemon, and recovery support on Linux, macOS, and Windows

See the [roadmap](docs/roadmap.md) for high-level product direction beyond the
capabilities listed here.

## Installation

Docbank supports Linux, macOS, and 64-bit Windows on amd64 and arm64. On Linux
or macOS, install the latest release with:

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

On Windows, run this in PowerShell:

```powershell
irm https://docbank.ai/install.ps1 | iex
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
docbank versions /archive/Documents/receipt.pdf
docbank refs <receipt-sha256>
docbank tag create taxes
docbank tag assign taxes /archive/Documents/receipt.pdf
docbank put revised-receipt.pdf /archive/Documents/receipt.pdf
docbank edit /archive/Documents/notes.md
docbank revert /archive/Documents/receipt.pdf <prior-version-id>
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
- [Embedding Docbank in Go](docs/embedding.md)
- [CLI reference](docs/cli-reference.md)
- [Architecture overview](docs/architecture/overview.md)
- [Internal design map](docs/internal/README.md) for contributors and coding agents

Docbank belongs to the same family of personal data tools as
[msgvault](https://msgvault.io). Msgvault preserves communications history;
Docbank provides a durable, reorganizable home for ordinary documents and a
safe document API for external applications.
