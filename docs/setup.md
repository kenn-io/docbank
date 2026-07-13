---
title: Setup
description: Build docbank from source and initialize the vault.
---

# Setup

docbank is pre-1.0. Linux, macOS, and 64-bit Windows are supported. The current
tagged release provides Linux amd64/arm64 and macOS arm64 archives; the next
distribution release adds the other supported targets and installers.

## Requirements

- Linux, macOS, or 64-bit Windows on amd64 or arm64.

Installing a release archive needs no build toolchain. Building from source
additionally requires:

- Go 1.26 or newer with CGO enabled — the store uses
  [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
- A C compiler (Xcode command-line tools on macOS, `gcc`/`clang` on Linux,
  or a MinGW-compatible compiler on Windows)

## Install a release

For a published release, download the archive for your platform and
`SHA256SUMS` from the
[GitHub Releases](https://github.com/kenn-io/docbank/releases) page. Verify
the archive against `SHA256SUMS`, extract it, and place `docbank` somewhere
on your `PATH` (for example, `~/.local/bin`). Release archives are named:

```
docbank_<version>_<goos>_<goarch>.tar.gz  # Linux and macOS
docbank_<version>_windows_<goarch>.zip    # Windows
```

!!! info "Current release assets"
    The first release predates the Windows port and macOS amd64 build. Until
    the next release is published, those users must build this branch from
    source. Never install an archive for a different OS or architecture.

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
