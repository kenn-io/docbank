---
title: Configuration
description: Vault location, data layout, config.toml, and environment variables.
---

# Configuration

The only required knob is where the vault lives. `config.toml` is optional and
controls the daemon's listen address, auth, idle behavior, default backup
repository, and optional watched inboxes. The vault works without the file;
backup commands require either a configured repository or their explicit
`--repo` flag.

## Vault location

All data defaults to `~/.docbank/`. Override with the `DOCBANK_HOME`
environment variable:

```bash
export DOCBANK_HOME=/Volumes/Archive/docbank
```

Run `docbank info` after selecting a vault to see its canonical path and stable
vault ID. This is especially useful when one machine has several independent
Docbank archives:

```bash
DOCBANK_HOME=/Volumes/Archive/docbank docbank info
```

The directory layout is created on first use:

```
~/.docbank/
├── docbank.db           # SQLite: virtual tree, metadata, FTS index
├── blobs/
│   ├── <aa>/<sha256>    # raw content-addressed document bytes
│   ├── <aa>/<sha256>.zst # managed compressed loose representation
│   └── tmp/             # staging for in-flight writes
├── logs/                # JSON logs from background daemons
├── config.toml          # optional; see below
├── vault.lock           # advisory lock, held by a daemon or target restore
└── daemon.<pid>.json    # runtime record of a live daemon
```

`docbank.db` and `blobs/` together are the archive; back up both. The optional
`.zst` suffix is only a physical encoding: hashes and reported document sizes
always describe the decoded content. Docbank chooses it for worthwhile new
writes and continues to read existing raw files without converting them.
`config.toml` is configuration, not archive data — optional, but back it
up if you've customized it (it can hold an `api_key`). `vault.lock` and
`daemon.<pid>.json` are coordination/runtime state, safe to
ignore in backups and safe to delete when no daemon or restore is running
(`docbank daemon stop` removes its own record cleanly on graceful
shutdown). The database
references blobs by hash, so restoring a copied `docbank.db` + `blobs/`
pair onto any machine yields a working vault — `docbank verify` proves
the pair is consistent. Stop the daemon before taking a manual filesystem
snapshot; see [Vault Lifecycle](usage/lifecycle.md#take-a-coherent-manual-snapshot).

Docbank also keeps persistent per-user coordination files under
`~/.local/state/docbank/target-locks`, using the home directory from the
operating-system account record. They contain no document data, but must not be
deleted: daemons and restores use their stable identities to exclude overlapping
vault trees, including simultaneous restores whose target trees overlap, and
to serialize daemon launch before the launcher owns or creates the vault root.

!!! warning
    Don't edit or prune `blobs/` by hand. Blob files are referenced by
    the database (including as prior document versions); use
    `docbank trash empty --run`, `docbank gc --run`, and (for dead packed
    payload) `docbank storage repack` to reclaim space; use `docbank verify` to
    check integrity.

## config.toml

`$DOCBANK_HOME/config.toml` is read once, at daemon startup (`docbank
daemon run` / `daemon start`). It's optional. There are no general per-field
environment overrides; the only environment knob remains `DOCBANK_HOME`.
Backup commands can override their configured repository with `--repo`. An
unrecognized key is treated as a typo and rejected at startup rather than
silently ignored.

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

[backup]
repo = ""           # no implicit repository; set a path or pass --repo
zstd_level = 0      # 0 = Kit default; otherwise 1-19

[storage]
pack_interval = "0"        # disabled; for example, "1h"
pack_max_bytes = 268435456  # 256 MiB soft raw-byte budget per run

[[watch]]
name = "agent-sessions"
source = "~/agent-sessions"
destination = "/archives/agents"
settle_time = "30s"
minimum_age = "168h" # optional; 7 days since source modification
scan_interval = "5s"
exclude = [".DS_Store", "cache/"]
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
- **`[backup] repo`** — default immutable snapshot repository used when a
  backup command or API request omits `repo`. `~/...` expands against the
  daemon user's home; a relative path is resolved beneath `$DOCBANK_HOME`.
  Keep the repository outside the live vault in normal deployments.
- **`[backup] zstd_level`** — repository compression level. `0` uses Kit's
  default; explicit values are limited to `1` through `19`.
- **`[storage] pack_interval`** — schedules non-destructive packing of
  authorized loose blobs. `"0"` disables the schedule. A configured schedule
  runs once when the daemon starts and then at this interval.
- **`[storage] pack_max_bytes`** — finite soft raw-byte budget for each
  scheduled run. It must be positive when `pack_interval` is enabled. Remaining
  loose content waits for a later run.

Scheduled packing is visible as the `storage:pack` job and keeps an auto-started
daemon alive so the schedule is meaningful. It uses the same maintenance gate
as `docbank storage pack`: ordinary mutations may briefly receive
`maintenance_busy` and can retry. Automatic packing does not delete logical
content and does not run GC or repack; those reclamation operations remain
explicit operator choices.

Scheduled packing is newer than v0.10.1. Build from `main` to use it until the
next release is tagged.

### Watched inboxes

Each `[[watch]]` entry makes the daemon poll one local directory recursively.
`source` must be absolute or begin with `~/`; `destination` is an absolute path
in Docbank's virtual tree. Symlinks and other non-regular entries inside the
source are ignored. `exclude` uses the same literal name-or-relative-path rules
as repeated `docbank add --exclude` flags; it is not glob syntax.

Traversal stays on the source's filesystem mount. It does not enter symlinks,
Windows directory reparse points, or nested mounts; configure another
`[[watch]]` entry when content on a separate mounted filesystem should also be
imported. This boundary prevents an aliased vault directory from becoming its
own input.

A file must remain the same filesystem object with the same size and
modification time for the complete `settle_time` before Docbank reads its
content. Optional `minimum_age` adds a second gate based on the source's
modification time. For example, `"168h"` requires a file to be at least seven
days old *and* unchanged for the complete settle window. It defaults to `"0s"`
(disabled), and unlike the in-memory settle observation, its source timestamp
still applies after a daemon restart. This is useful for append-heavy agent
sessions and recording streams that may pause without being finished.

After the read, the daemon verifies that the confined source path still names
that object and grants no node authority if it changed. `scan_interval`
controls how often it looks for new observations. Zero values select the
defaults shown above; explicit settle and scan values must be positive.
`minimum_age` must not be negative.

Minimum age is a conservative time policy, not proof that the producing
application explicitly closed a file. Choose a window appropriate to the
producer, or watch a directory that receives only completed files when the
producer offers a close/rename handoff.

A file that disappears during observation, or is still held exclusively by a
Windows producer, is treated as unsettled and retried from a fresh window.
Other read failures remain visible job errors rather than being ignored.

The pair `(name, relative source path)` is the durable source identity. Keep a
watch name stable when moving its machine-local `source` root: a changed file
then becomes a new immutable version of the same Docbank node, even if that
node was reorganized elsewhere in the virtual tree. Renaming a relative source
path intentionally creates a new source identity. Each source identity owns
one Docbank node, and one node cannot be claimed by two watched sources.
Deleting a source file never deletes its Docbank node.

Docbank separately remembers the last bytes accepted from each source. If a
person edits or reverts the Docbank node while the source stays unchanged, a
daemon restart does not overwrite that working version. Only a later byte
change at the watched source appends another version.

Watchers run as jobs named `watch:<name>`. In source builds newer than v0.10.0,
`docbank watch list` and `GET /api/v1/watches` pair each runner's state with its
effective source, destination, settle window, minimum source age, scan interval,
and exclusion policy; use `--json` when an agent needs the complete rules.
`minimum_age` itself is newer than v0.10.1. `docbank jobs` and
`GET /api/v1/jobs` remain the all-task view.
A source, destination, or read failure leaves the named job in the failed state
and records the reason. Restart the daemon after correcting the problem.
Per-file successes are written to the daemon log. A configured watch keeps a
background daemon alive regardless of `idle_timeout`.

Inspect the durable source facts attached to an imported file with
`docbank provenance <path-or-id>` or `GET /api/v1/nodes/{id}/provenance`.
This is distinct from job status: provenance survives daemon restarts and
records successful ingest authority, while `docbank jobs` describes only the
current daemon run.

Watched inboxes never modify or delete their source files. Configuration is
machine-local and is not part of metadata-v1 backup/restore, while the stable
watch name, relative path, stable node mapping, and last accepted content
identity are preserved in portable metadata. The watcher does not pack content
itself. Configure `[storage] pack_interval` when accumulated loose content
should be packed automatically; GC and repack remain explicit.

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
