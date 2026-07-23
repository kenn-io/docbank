---
title: Setup
description: Install docbank on Linux, macOS, or Windows and create the vault.
---

# Setup

docbank is pre-1.0. Linux, macOS, and 64-bit Windows are supported on amd64 and
arm64.

## Requirements

- Linux, macOS, or 64-bit Windows on amd64 or arm64.

Installing a release archive needs no build toolchain. Building from source
additionally requires:

- Go 1.26 or newer with CGO enabled — the store uses
  [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
- A C compiler (Xcode command-line tools on macOS, `gcc`/`clang` on Linux,
  or a MinGW-compatible compiler on Windows)
- Node.js 24 or newer and npm — source builds compile the embedded web
  application before the Go binary

## Install a release

Published releases include Linux, macOS, and Windows archives for amd64 and
arm64 with SHA-256 checksums. The shell and PowerShell installers select the
native archive and verify it against `SHA256SUMS` before installing, failing
rather than substituting an incompatible or unverified archive.

On Linux or macOS, the installer selects the native archive and installs
`docbank` to `~/.local/bin` by default:

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

Set `DOCBANK_INSTALL_DIR` to choose another destination. Set
`DOCBANK_VERSION=vX.Y.Z` to install a specific release rather than the latest.

On Windows, run the PowerShell installer:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

It installs to `%LOCALAPPDATA%\Programs\docbank\bin` and adds that directory
to the user `PATH`. `DOCBANK_INSTALL_DIR` and `DOCBANK_VERSION` provide the
same overrides as on Unix; set `DOCBANK_NO_MODIFY_PATH=1` to leave `PATH`
unchanged.

Both installers are maintained in the repository as `scripts/install.sh`
and `scripts/install.ps1`; docbank.ai serves those files verbatim. Every
downloaded archive is verified against the `SHA256SUMS` file published
with its GitHub release before anything is installed, and the installers
fail rather than substitute an unverified archive. If you prefer to
bootstrap from a trust origin independent of the docbank.ai hosting, run
the repository copy directly:

```bash
curl -fsSL https://raw.githubusercontent.com/kenn-io/docbank/main/scripts/install.sh | sh
```

The installers fail closed: the archive is not extracted or installed unless
`SHA256SUMS` is available, contains exactly one matching entry, and verifies.
They also reject an archive that contains anything other than the expected
top-level executable.

### Manual installation

For a published release, download the archive for your platform and
`SHA256SUMS` from the
[GitHub Releases](https://github.com/kenn-io/docbank/releases) page. Verify
the archive against `SHA256SUMS`, extract it, and place `docbank` somewhere
on your `PATH` (for example, `~/.local/bin`). Release archives are named:

```
docbank_<version>_<goos>_<goarch>.tar.gz  # Linux and macOS
docbank_<version>_windows_<goarch>.zip    # Windows
```

Releases produced by the current distribution workflow publish all six
archives. Earlier tags may contain fewer targets; an installer fails instead
of substituting another platform. Never install an archive for a different OS
or architecture.

## Build and install from source

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make build      # builds ./docbank
make install    # installs to ~/.local/bin
```

On Windows, use a PowerShell prompt with Go and the C compiler available:

```powershell
go build -tags fts5 -o docbank.exe ./cmd/docbank
go test -tags fts5 ./...
```

The SQLite full-text index requires the `fts5` build tag; the Makefile
targets set it for you. If you invoke `go` directly, pass it yourself:

```bash
go build -tags fts5 ./cmd/docbank
go test -tags fts5 ./...
```

## First run

There is no init step. The first command you run creates the vault layout
under `~/.docbank/` (database, blob directory, lock file):

```bash
docbank add ~/Desktop/some-document.pdf
docbank ls /inbox
```

Set `DOCBANK_HOME` to keep the vault somewhere else — see
[Configuration](configuration.md).

Before importing irreplaceable material, read [Vault Lifecycle](usage/lifecycle.md)
and decide how the vault will be snapshotted.

## Verifying the toolchain

```bash
make test    # full test suite
make lint    # golangci-lint
```

Both must pass cleanly on a supported platform. If `go test` fails with
`undefined: ...fts5...` errors, the `fts5` tag is missing.
