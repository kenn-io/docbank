---
title: Configuration
description: Vault location, data layout, and environment variables.
---

# Configuration

Phase 1 needs no configuration file. The only knob is where the vault
lives.

## Vault location

All data defaults to `~/.docbank/`. Override with the `DOCBANK_HOME`
environment variable:

```bash
export DOCBANK_HOME=/Volumes/Archive/docbank
```

The directory layout is created on first use:

```
~/.docbank/
├── docbank.db           # SQLite: virtual tree, metadata, FTS index
├── blobs/
│   ├── <aa>/<sha256>    # content-addressed document bytes
│   └── tmp/             # staging for in-flight writes
├── logs/                # reserved for the daemon (Phase 2)
└── vault.lock           # advisory inter-process lock
```

`docbank.db` and `blobs/` together are the archive; back up both. The
database references blobs by hash, so restoring a copied
`docbank.db` + `blobs/` pair onto any machine yields a working vault —
`docbank verify` proves the pair is consistent.

!!! warning
    Don't edit or prune `blobs/` by hand. Blob files are referenced by
    the database (including as prior document versions); use
    `docbank trash empty` and `docbank gc --run` to reclaim space, and
    `docbank verify` to check integrity.

## Planned configuration

!!! info "Planned — Phase 2"
    The daemon introduces `~/.docbank/config.toml` for watched-inbox
    directories, the API listen address and keys, and extraction worker
    settings. The file is optional; the CLI keeps working without it.
