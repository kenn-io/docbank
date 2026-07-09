---
title: Configuration
description: Vault location, data layout, config.toml, and environment variables.
---

# Configuration

The only required knob is where the vault lives. `config.toml` is
optional and controls the daemon's listen address, auth, and idle
behavior; every value has a default and the CLI works without the file.

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
├── logs/                # JSON logs from background daemons
├── config.toml          # optional; see below
├── vault.lock           # advisory inter-process lock, held by the daemon
├── launch.lock          # serializes racing daemon auto-starts
└── daemon.<pid>.json    # runtime record of a live daemon
```

`docbank.db` and `blobs/` together are the archive; back up both.
`config.toml`, `vault.lock`, `launch.lock`, and `daemon.<pid>.json` are
not part of the archive — they're daemon runtime state, safe to ignore
in backups and safe to delete when no daemon is running (`docbank serve
stop` removes its own record cleanly on graceful shutdown). The database
references blobs by hash, so restoring a copied `docbank.db` + `blobs/`
pair onto any machine yields a working vault — `docbank verify` proves
the pair is consistent.

!!! warning
    Don't edit or prune `blobs/` by hand. Blob files are referenced by
    the database (including as prior document versions); use
    `docbank trash empty` and `docbank gc --run` to reclaim space, and
    `docbank verify` to check integrity.

## config.toml

`$DOCBANK_HOME/config.toml` is read once, at daemon startup (`docbank
serve` / `serve start`). It's optional — every value has a default —
and there are no per-field environment variable or flag overrides; the
only environment knob remains `DOCBANK_HOME`. An unrecognized key is
treated as a typo and rejected at startup rather than silently ignored.

```toml
# ~/.docbank/config.toml — optional, defaults shown
[server]
bind_addr = "127.0.0.1"
api_port = 0          # 0 = ephemeral; clients discover the real port
                      # from the runtime record
api_key = ""          # empty = keyless local-only mode
idle_timeout = "30m"  # background daemons only; "0" = never

[web]
enabled = true
```

- **`bind_addr`** — the interface the API listens on. Loopback
  (`127.0.0.1`, `::1`, `localhost`) needs no key.
- **`api_port`** — `0` picks an ephemeral port; the CLI never needs to
  know it in advance because it discovers the actual bound address from
  the daemon's runtime record.
- **`api_key`** — checked against `X-Api-Key` or `Authorization: Bearer`
  on every `/api/v1` request. Empty means keyless mode, which is valid
  **only** on a loopback bind.
- **`idle_timeout`** — how long a background daemon waits without
  requests before exiting on its own. `"0"` disables idle shutdown.
  Foreground `docbank serve` ignores this and never idles out.
- **`[web] enabled`** — serves the placeholder web page at `/`. Disabling
  it 404s `/`; the API and `/docs` are unaffected.

### Bind and key validation

Validated once, at daemon startup — a misconfiguration fails `docbank
serve` immediately rather than silently serving insecurely:

- An unspecified address (`0.0.0.0`, `::`) is always rejected: it binds
  every interface, including public ones, regardless of intent.
- A **loopback** `bind_addr` accepts an empty `api_key` (keyless mode).
- Any other `bind_addr` **requires** a non-empty `api_key` — keyless
  mode is loopback-only by design.
- Non-loopback addresses are additionally checked with `kit`'s
  `RequireNonPublic`, which permits private-network addresses (RFC 1918,
  link-local, CGNAT) and rejects a genuinely public one outright,
  regardless of whether a key is set.

## Environment variables

| Variable | Effect |
|----------|--------|
| `DOCBANK_HOME` | Vault location; see [Vault location](#vault-location) above. |
| `DOCBANK_LOG_LEVEL` | Log level (`debug`, `info`, `warn`, `error`) for `docbank serve`, foreground or background. Invalid values are ignored and fall back to `info`. |
