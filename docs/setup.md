---
title: Setup
description: Build docbank from source and initialize the vault.
---

# Setup

docbank is pre-1.0. Published releases provide archives for Linux amd64/arm64
and macOS arm64; building from source remains supported on Unix-like systems.

## Requirements

- macOS or Linux. docbank requires a Unix-like OS: vault locking uses
  `flock(2)`, and there is no Windows port.

Installing a release archive needs no build toolchain. Building from source
additionally requires:

- Go 1.26 or newer with CGO enabled — the store uses
  [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
- A C compiler (Xcode command-line tools on macOS, `gcc`/`clang` on Linux)

## Install a release

For a published release, download the archive for your platform and
`SHA256SUMS` from the
[GitHub Releases](https://github.com/kenn-io/docbank/releases) page. Verify
the archive against `SHA256SUMS`, extract it, and place `docbank` somewhere
on your `PATH` (for example, `~/.local/bin`). Release archives are named:

```
docbank_<version>_<goos>_<goarch>.tar.gz
```

Windows is not published because the vault is Unix-only.

## Build and install from source

```bash
git clone https://github.com/kenn-io/docbank.git
cd docbank
make build      # builds ./docbank
make install    # installs to ~/.local/bin (or GOPATH/bin)
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

## Verifying the toolchain

```bash
make test    # full test suite
make lint    # golangci-lint
```

Both must pass cleanly on a supported platform. If `go test` fails with
`undefined: ...fts5...` errors, the `fts5` tag is missing.
