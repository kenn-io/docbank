---
title: Setup
description: Build docbank from source and initialize the vault.
---

# Setup

docbank is pre-release: there are no binary releases yet, so you build
from source.

## Requirements

- Go (latest stable) with CGO enabled — the store uses
  [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
- A C compiler (Xcode command-line tools on macOS, `gcc`/`clang` on Linux)
- macOS or Linux. docbank requires a Unix-like OS: vault locking uses
  `flock(2)`, and there is no Windows port.

## Build and install

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
