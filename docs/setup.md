---
title: Setup
description: Install Docbank on Linux, macOS, or Windows and create the vault.
---

# Setup

You can install a prebuilt release archive without any build tools, or compile
Docbank from source. Docbank is pre-1.0 and runs on Linux, macOS, and 64-bit
Windows on amd64 and arm64 processors.

## Requirements

- Linux, macOS, or 64-bit Windows on amd64 or arm64.

If you install a release archive, you do not need a build toolchain. To build
from source, you also need:

- Go 1.27.4 or newer with CGO enabled, since the store uses
  [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
- A C compiler, such as Xcode command-line tools on macOS, `gcc` or `clang` on
  Linux, or a MinGW-compatible compiler on Windows
- Node.js 24 or newer and npm, because source builds compile the embedded web
  application before building the Go binary

## Install a release

Each release includes Linux, macOS, and Windows archives for amd64 and arm64,
along with a `SHA256SUMS` file. The shell and PowerShell installers pick the
native archive for your machine and verify its SHA-256 checksum before
installing. If the archive is incompatible or fails verification, the installer
stops with an error.

On Linux or macOS, run the shell installer. It puts `docbank` in `~/.local/bin`
by default:

```bash
curl -fsSL https://docbank.ai/install.sh | sh
```

To choose another destination, set `DOCBANK_INSTALL_DIR`. To install a specific
release instead of the latest one, set `DOCBANK_VERSION=vX.Y.Z`.

On Windows, run the PowerShell installer:

```powershell
irm https://docbank.ai/install.ps1 | iex
```

It installs to `%LOCALAPPDATA%\Programs\docbank\bin` and adds that directory to
your user `PATH`. `DOCBANK_INSTALL_DIR` and `DOCBANK_VERSION` work the same way
as on Unix. To leave `PATH` unchanged, set `DOCBANK_NO_MODIFY_PATH=1`.

Both installers live in the repository as `scripts/install.sh` and
`scripts/install.ps1`, and docbank.ai serves those same files. To fetch the
shell installer directly from GitHub instead, run:

```bash
curl -fsSL https://raw.githubusercontent.com/kenn-io/docbank/main/scripts/install.sh | sh
```

Neither installer extracts or installs an archive unless `SHA256SUMS` is
available, has exactly one matching entry, and matches the archive's checksum. Each
installer also rejects any archive that contains anything other than the
expected top-level executable.

### Manual installation

1. Download your platform's archive and `SHA256SUMS` from
   [GitHub Releases](https://github.com/kenn-io/docbank/releases).
2. Verify the archive against its entry in `SHA256SUMS`.
3. Extract the archive.
4. Put `docbank` on your `PATH`, for example in `~/.local/bin`.

Release archives use these names:

```
docbank_<version>_<goos>_<goarch>.tar.gz  # Linux and macOS
docbank_<version>_windows_<goarch>.zip    # Windows
```

Every release publishes all six archives. The installers fail rather than
substituting another platform, and you should never install an archive built
for a different OS or architecture.

## Build and install from source

On Linux or macOS, clone the repository and build with `make`:

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make build      # builds ./bin/docbank
make install    # installs to ~/.local/bin
```

On Windows, open a PowerShell prompt with Go and your C compiler available,
then run:

```powershell
Push-Location frontend
npm ci
npm run build
Pop-Location
Get-ChildItem internal/web/dist -Force |
  Where-Object Name -ne '.keep' |
  Remove-Item -Recurse -Force
Copy-Item -Recurse -Force frontend/dist/* internal/web/dist/
go build -tags fts5 -o bin/docbank.exe ./cmd/docbank
go test -tags fts5 ./...
```

The SQLite full-text index requires the `fts5` build tag. The Makefile targets
build the frontend first and pass that tag for you. If you run `go` directly on
Unix, build and copy the embedded frontend first, then pass `-tags fts5`:

```bash
(cd frontend && npm ci && npm run build)
find internal/web/dist -mindepth 1 ! -name .keep -exec rm -rf {} +
cp -R frontend/dist/. internal/web/dist/
go build -tags fts5 -o bin/ ./cmd/docbank
go test -tags fts5 ./...
```

## First run

Docbank has no separate init step. The first command you run creates the vault
layout under `~/.docbank/` with its database, blob directory, and lock file:

```bash
docbank document add ~/Desktop/some-document.pdf
docbank document list /inbox
```

To keep the vault somewhere else, set `DOCBANK_HOME`. See
[Configuration](configuration.md).

Before you import anything irreplaceable, pick a backup location and plan a
restore test. [Vault lifecycle](usage/lifecycle.md) covers the regular
maintenance routine, and [Backup and restore](usage/backup.md) has the exact
commands.

## Verify the toolchain

Run the test suite and linter to check your build environment:

```bash
make test    # full test suite
make lint    # golangci-lint
```

Both checks must pass on a supported platform. If `go test` fails with
`undefined: ...fts5...` errors, you are missing the `fts5` build tag.

## Enable photo meaning search

To search photos by meaning, configure an image/text provider through the
standard embedding bridge and grant processing consent for its image binding
and text queries. See
[Embed photos for meaning search](usage/document-processing.md#embed-photos-for-meaning-search)
for the setup steps. Eligible photo previews then queue automatically. In the
Photos workspace, the **Find by** control always offers **Meaning** and
**Hybrid** search. Without setup, or if the provider, consent, or index is
unavailable, search falls back to metadata and explains why.
