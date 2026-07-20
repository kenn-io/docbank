# docbank

[![Go 1.26+](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/kenn-io/docbank/actions/workflows/ci.yml/badge.svg)](https://github.com/kenn-io/docbank/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/kenn-io/docbank?include_prereleases)](https://github.com/kenn-io/docbank/releases)

> **Alpha software.** Docbank is pre-1.0. Keep independent copies of
> irreplaceable material and verify backups before relying on them.

**Your documents. Your agents. One system.**

Docbank gives you and your agents one authoritative place to file, find,
organize, version, and verify the PDFs, scans, notes, spreadsheets, and records
you depend on. You keep the authority: the vault and its history live on your
own machine rather than inside a provider account. Content is immutable and
deduplicated under one logical SHA-256 identity, while stable document IDs and
a virtual tree let people and agents reorganize the archive without losing
track of anything. Documentation lives at [docbank.ai](https://docbank.ai).

## Why docbank?

Ordinary folders make a document's location part of its identity, and cloud
drives couple an archive's integrity, history, and continued existence to an
account in good standing. Neither makes durable content identity,
independent verification, and provider-independent retention part of its
native contract. Docbank does: everything lives on your machine in an
inspectable layout, imports copy and never touch sources, and integrity is
provable on demand. One honest boundary — docbank is an archive and system
of record, not a sync-and-share tool. It makes the copy that must survive
trustworthy.

Two ways to run it, one authority per vault:

- **Standalone.** A personal archive: the CLI and a daemon on your machine.
  Agents and scripts use the same authenticated HTTP contract the CLI uses,
  with IDs that survive renames and revision preconditions for safe
  read-modify-write.
- **Embedded.** A Go module: independently rooted vaults in-process, no
  daemon, the same storage model and lifecycle guarantees.

Four commitments:

- **Immutable content.** Blob writes are durable before the database ever
  references them, and every content version keeps a verified SHA-256
  identity you can re-check at any time with `verify`.
- **Deliberate lifecycle.** `rm` is always recoverable trash. Emptying
  trash, collecting unreachable content, and compacting packed space are
  separate explicit operations — never side effects.
- **Verified backup & restore.** Incremental snapshot repositories support
  create, list, verify, and confined restore into a separate vault, so a
  backup is proven before it is trusted.
- **Audited history.** Opt a directory into permanent, tamper-evident
  history with `docbank audit enable`; every supported change is recorded
  and `verify` independently replays it.

## Implemented today

- Virtual folders with listing, tree browsing, rename, move, trash, and restore
- Resumable bulk import and verified multipart upload
- Stable content-version UUIDs, verified replacement and reversion, bounded
  history listing, ID-addressed retrieval, and lookup by content hash
- Stable tags with define, rename, assign, unassign, delete, and bounded
  node/tag listings
- Preview-first permanent audit enrollment for one directory scope, with
  sticky retention, recorded supported mutations, status evidence, bounded
  per-node history, independent chain/protected-byte verification, exact-prefix
  checks against externally recorded evidence, and backup/restore fidelity
- FTS5 search over names and verified UTF-8 text/Markdown/JSON contents
  (body search is available from source until the next tagged release)
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
`go build -tags fts5 -o docbank.exe ./cmd/docbank`. The
[setup guide](docs/setup.md) is the installation authority and covers each
platform's toolchain.

## Quick start

There is no initialization command. The first data command creates the local
vault and starts its daemon:

> **Release availability:** `docbank version` and the explicit
> `docbank versions list|show|cat` commands are newer than v0.7.0. Build from
> source to use them until the next release is published.

```bash
docbank add ~/Documents --dest /archive
docbank tree /archive
docbank search "tax return"
docbank versions list /archive/Documents/receipt.pdf
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
- [Embed in Go](docs/embedding.md)
- [CLI reference](docs/cli-reference.md)
- [Architecture overview](docs/architecture/overview.md)
- [Internal design map](docs/internal/README.md) for contributors and coding agents

Docbank belongs to a family of personal data tools alongside
[msgvault](https://msgvault.io), the communications archive. Where msgvault
preserves an immutable record of your messages, docbank manages working
documents: files you still organize, retrieve, and build workflows around —
and a safe document API for external applications.
