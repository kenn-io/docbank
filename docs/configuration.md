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
`config.toml` is configuration, not archive data — optional, but back it
up if you've customized it (it can hold an `api_key`). `vault.lock`,
`launch.lock`, and `daemon.<pid>.json` are daemon runtime state, safe to
ignore in backups and safe to delete when no daemon is running
(`docbank daemon stop` removes its own record cleanly on graceful
shutdown). The database
references blobs by hash, so restoring a copied `docbank.db` + `blobs/`
pair onto any machine yields a working vault — `docbank verify` proves
the pair is consistent. Stop the daemon before taking a manual filesystem
snapshot; see [Vault Lifecycle](usage/lifecycle.md#take-a-coherent-manual-snapshot).

!!! warning
    Don't edit or prune `blobs/` by hand. Blob files are referenced by
    the database (including as prior document versions); use
    `docbank trash empty --run` and `docbank gc --run` to reclaim space, and
    `docbank verify` to check integrity.

## config.toml

`$DOCBANK_HOME/config.toml` is read once, at daemon startup (`docbank
daemon run` / `daemon start`). It's optional — every value has a default —
and there are no per-field environment variable or flag overrides; the
only environment knob remains `DOCBANK_HOME`. An unrecognized key is
treated as a typo and rejected at startup rather than silently ignored.

```toml
# ~/.docbank/config.toml — optional, defaults shown
[server]
bind_addr = "127.0.0.1"
api_port = 0          # 0 = ephemeral; clients discover the real port
                      # from the runtime record
api_key = ""          # empty = ephemeral per-run key (loopback only)
idle_timeout = "30m"  # background daemons only; "0" = never

[web]
enabled = true
```

- **`bind_addr`** — the interface the API listens on. Loopback only
  (`127.0.0.1`, `::1`, `localhost`): the API is plain HTTP, so a
  non-loopback bind would put the key and vault contents on the wire in
  cleartext. Reach a remote docbank through an SSH tunnel or VPN.
- **`api_port`** — `0` picks an ephemeral port; the CLI never needs to
  know it in advance because it discovers the actual bound address from
  the daemon's runtime record.
- **`api_key`** — checked against `X-Api-Key` or `Authorization: Bearer`
  on every authenticated request; the daemon always enforces one. Empty
  means "generate an ephemeral key at startup" rather than "no auth
  required" — the generated key is published to same-user clients via
  the runtime record, the same mechanism the shutdown token already
  uses. Set it only when a client can't read the runtime record (an SSH
  tunnel from another machine).
- **`idle_timeout`** — how long a background daemon waits without
  requests before exiting on its own. `"0"` disables idle shutdown.
  Foreground `docbank daemon run` ignores this and never idles out.
- **`[web] enabled`** — serves the placeholder web page at `/`. Disabling
  it 404s `/`; the API and `/docs` are unaffected.

### Bind validation

Validated once, at daemon startup — a misconfiguration fails `docbank
daemon run` immediately rather than silently serving insecurely:

- A **loopback** `bind_addr` (`127.0.0.1`, `::1`, `localhost`) is the
  only accepted value. An empty `api_key` is fine there: the daemon
  generates one at startup instead.
- Every non-loopback address — wildcard, private-network, or public,
  keyed or not — is rejected. The API is plain HTTP; a key sent in
  cleartext is not protection. Remote access goes through an SSH tunnel
  or VPN to the loopback listener until the daemon grows TLS.

## Environment variables

| Variable | Effect |
|----------|--------|
| `DOCBANK_HOME` | Vault location; see [Vault location](#vault-location) above. |
| `DOCBANK_LOG_LEVEL` | Log level (`debug`, `info`, `warn`, `error`) for `docbank daemon run`, foreground or background. Invalid values are ignored and fall back to `info`. |
