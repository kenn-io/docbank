---
last_edited: 2026-09-12
title: Configuration
description: Vault location, data layout, config.toml, and environment variables.
---

# Configuration

For optional pinned local email rendering, see
[Email PDF configuration](usage/email-pdf.md#configure-the-renderer).

Docbank uses `~/.docbank/` with default settings unless you choose another
vault. Use `DOCBANK_HOME` to select its location. Add `config.toml` when you need
to change how the daemon runs.

The file controls the listening address, authentication, idle timeout, backup
repository, watched inboxes, and secondary-store connections. A vault with only
a primary store needs no configuration file. Each registered secondary store
needs a matching connection profile after restart. Backup commands need either
a configured repository or an explicit `--repo` flag.

For optional bounded PDF and density-qualified PNG rendering, see
[Verified page images](architecture/page-images.md#configure-the-optional-runtime).
The `[page_runtime]` section is independent of text and embedding providers.

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
├── web-launch/          # owner-private browser authentication handoff
├── web-downloads/       # private, temporary verified browser downloads
├── config.toml          # optional; see below
├── vault.lock           # advisory lock, held by a daemon or target restore
└── daemon.<pid>.json    # runtime record of a live daemon
```

`docbank.db` holds the local catalog; `blobs/` holds the built-in primary
store. A `.zst` file contains compressed content. Hashes and document sizes
always describe the decoded bytes. Docbank may compress new writes when the
savings justify it, but it reads existing raw files without converting them.

Back up `config.toml` separately if you customize it. It can contain an
`api_key`, filesystem paths, S3 coordinates, and credential-profile names.

`vault.lock`, `daemon.<pid>.json`, `web-launch/`, and `web-downloads/` coordinate
running processes. You can omit them from backups. Delete them only when no
daemon or restore is running. `docbank daemon stop` removes its own runtime
record on graceful shutdown.

Use `docbank backup create` to capture every retained blob from a verified
location, including content held only in a secondary store. The resulting
backup can restore without recreating the original store layout.

A stopped copy of `docbank.db` and `blobs/` is complete only when the primary
is an authorized location for every retained blob. Copying `config.toml` saves
secondary-store coordinates, but does not copy their content. Stop the daemon
before taking a filesystem snapshot. See
[Vault Lifecycle](usage/lifecycle.md#take-a-coherent-backup).

Docbank also keeps persistent per-user coordination files under
`~/.local/state/docbank/target-locks`, using the home directory from the
operating-system account record. They contain no document data, but must not be
deleted: daemons and restores use their stable identities to exclude overlapping
vault trees, including simultaneous restores whose target trees overlap, and
to serialize daemon launch before the launcher owns or creates the vault root.

!!! warning
    Don't edit or prune `blobs/` or a secondary namespace by hand. Physical
    files are authorized by the database (including for prior document
    versions); use
    `docbank trash empty --run`, `docbank gc --run`, and (for dead packed
    payload) `docbank storage repack` to reclaim space; use `docbank verify` to
    check integrity.

## config.toml

`$DOCBANK_HOME/config.toml` is read once, at daemon startup (`docbank
daemon run` / `daemon start`). It's optional. There are no general per-field
environment overrides. `DOCBANK_HOME` selects the vault, and named credential
bindings can read explicitly configured environment variables.
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
- **`api_key`** — the daemon checks `X-Api-Key` or `Authorization: Bearer`
  on every authenticated request. An empty setting makes the daemon generate
  a key at startup and publish it to same-user clients in the runtime record.
  Set a fixed key when a client cannot read that record, such as a client
  using an SSH tunnel from another machine.
- **`idle_timeout`** — how long a background daemon waits without
  requests before exiting on its own. `"0"` disables idle shutdown.
  Foreground `docbank daemon run` ignores this and never idles out.
- **`[web] enabled`** — serves the embedded web application at `/`.
  `docbank web` starts or reconnects to the compatible daemon and opens an
  authenticated browser session on a fresh per-daemon loopback origin,
  independent of a configured `api_port`. Disabling it 404s `/` and `/assets/`;
  the API and `/docs` are unaffected. See [Web application](usage/web.md).
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

### Watched inboxes

Each `[[watch]]` entry makes the daemon poll one local directory recursively.
`source` must be absolute or begin with `~/`; `destination` is an absolute path
in Docbank's virtual tree. Symlinks and other non-regular entries inside the
source are ignored. `exclude` remains a literal name-or-relative-path rule. It
is independent of the glob include and exclude patterns accepted by
`docbank add`.

Traversal stays on the source's filesystem mount. It does not enter symlinks,
Windows directory reparse points, or nested mounts; configure another
`[[watch]]` entry when content on a separate mounted filesystem should also be
imported. This boundary prevents an aliased vault directory from becoming its
own input.

The daemon checks each file in this order:

1. Observe the same filesystem object, size, and modification time for the
   complete `settle_time`.
2. Require the source modification time to satisfy `minimum_age`, if enabled.
3. Read the file, then check that the source path still names the same object.
   If it changed, do not accept the read as a Docbank node.

For example, `minimum_age = "168h"` requires a file to be at least seven days
old and unchanged for the complete settle window. The default `"0s"` disables
this age check. The source timestamp still applies after a daemon restart;
the in-memory settle observation starts again. This helps with sessions or
recordings that pause before they finish.

`scan_interval` controls observation frequency. Zero settle and scan values
select the defaults shown above; explicit values must be positive.
`minimum_age` must not be negative.

Minimum age is a conservative time policy, not proof that the producing
application explicitly closed a file. Choose a window appropriate to the
producer, or watch a directory that receives only completed files when the
producer offers a close/rename handoff.

A file that disappears during observation, or is still held exclusively by a
Windows producer, is treated as unsettled and retried from a fresh window.
Other read failures remain visible job errors rather than being ignored.

Docbank identifies each watched source by `(name, relative source path)`.
Keep the watch name when moving its local `source` root. Later content changes
then add versions to the same Docbank node, even if someone moved that node in
the virtual tree.

Renaming the relative source path creates a new source identity. Each identity
owns one Docbank node; two watched sources cannot claim the same node.
Deleting a source file does not delete its Docbank node.

Docbank separately remembers the last bytes accepted from each source. If a
person edits or reverts the Docbank node while the source stays unchanged, a
daemon restart does not overwrite that working version. Only a later byte
change at the watched source appends another version.

Watchers run as jobs named `watch:<name>`. `docbank watch list` and
`GET /api/v1/watches` pair each runner's state with its effective source,
destination, settle window, minimum source age, scan interval, and exclusion
policy; use `--json` when an agent needs the complete rules. `docbank jobs`
and `GET /api/v1/jobs` remain the all-task view.
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

### Embedding workers and credentials

The daemon runs retained embedding jobs for its configured OpenAI-compatible
and Voyage runtimes. Other [provider packages](document-understanding.md) are
available to Go applications; they are not additional daemon runtime choices.

An embedding is a numeric representation used to compare document meaning. Each provider
binding publishes its own results; one provider's failure does not remove
another binding's completed vectors.

Configuring a provider does not prepare inputs or apply a processing profile
to new imports. A job requires all of the following:

- An existing input generation: the retained set of prepared provider inputs.
- A matching processing profile.
- A matching provider descriptor.
- Current consent to disclose the inputs to that provider.

After restore, the daemon recreates jobs from retained inputs once these
requirements are met again.

`[processing_profiles.<name>]` selects embedding bindings by name in its
`embeddings` array. `[embedding_profiles.<name>]` defines each binding's model,
input kind, dimensions, formatters, compatibility identity, descriptor and
disclosure fingerprints, byte limits, and `optional` or `required` activation.
Chunk bindings also pin their tokenizer and chunk policy in `.chunk`. Use the
fingerprints from the exact provisioned profile and deployment; arbitrary
fingerprints or an endpoint's model alias do not establish compatibility.

Credentials are referenced by name, never stored as values in a processing
profile. This fragment connects an existing `semantic` binding to a secret in
the daemon's environment:

```toml
[credential_bindings.embedding-primary]
environment_variable = "DOCBANK_EMBEDDING_PRIMARY_KEY"

[embedding_profiles.semantic]
credential_binding = "credential:embedding-primary"
# The remaining pinned profile fields are also required.
```

A missing or empty secret leaves ordinary document operations available.
Affected embedding jobs record an authorization failure and can recover later.
The daemon reads secrets from its own environment for each request. To change
a secret, set the variable for the daemon and restart it; exporting a variable
in another shell does not update a running daemon.

Startup still fails for an undefined credential binding, invalid runtime
configuration, or mismatched descriptor.

#### Runtime settings

An optional `[embedding_profiles.<name>.runtime]` section makes a binding
executable on this machine. Without it, the daemon does not claim that binding's
work. Runtime configuration and environment-variable mappings are machine-local
and must be supplied separately after restoring a vault.

| Fields | Meaning |
| --- | --- |
| `adapter_contract` | `docbank-openai-compatible-embeddings/v1` for rendition chunks, or `docbank-voyage-embeddings/v1` for original files. |
| `endpoint`, `model_revision` | Exact provider endpoint and pinned revision. OpenAI-compatible endpoints are origins without a path; Voyage uses `https://api.voyageai.com/v1`. |
| `deployment_epoch`, `provider_revision_header` | OpenAI-compatible runtimes require exactly one. The epoch must equal `model_revision`; a revision header must echo the pinned revision in every response. |
| `capability_manifest` | Voyage requires an absolute path to a capability manifest matching its model, media policy, and descriptor. |
| `request_timeout`, `max_request_bytes` | Bound each provider request. The profile's `max_batch_items`, `max_input_bytes`, and `max_response_bytes` supply the other request/response limits. |
| `allowed_cidrs`, `spki_sha256`, `proxy_mode` | Explicit destination CIDRs, optional TLS public-key pins, and `proxy_mode = "disabled"`. The provider connection enforces this policy. |
| `connect_timeout`, `keep_alive`, `tls_handshake_timeout` | Required positive transport durations, each at most five minutes. |

Request timeouts must also be positive and at most five minutes. Retry policy
belongs to the worker, so runtime configuration has no retry-count or retry-delay
fields. The worker handles transient failures and capacity-driven batch splits;
malformed responses are recorded separately from rejected document input.

#### Model input

`[embedding_profiles.<name>.model_input]` pins how document and query inputs are
formatted. Its `profile` selects a named contract such as `nomic/v1`, `bge-m3/v1`,
`e5/v1`, or `gte/v1`; it is not an arbitrary provider model name. The resulting
contract must match the binding's `compatibility_id` and provider descriptor.
For `custom/v1`, supply `compatibility_id` and explicit `.document` and `.query`
encoders, each with `mode` and `template`. Templates use `{{content}}`; contract
validation checks the supported roles and formatting rules. `query_instruction`
is available only to contracts that support it.

Discovery uses bounded pages and waits one minute between complete passes.
Existing queued work is still checked on the normal one-second idle cadence.
Missing or invalid generation bytes are skipped during discovery so later
candidates can proceed; a later pass retries discovery. Database failures retain
their separate bounded storage-retry behavior. Terminal jobs do not retain
superseded generations after their last embedding set is collected, while
queued/running jobs and explicit retention roots keep their inputs.

### Store bindings

`[store_bindings.<name>]` profiles describe machine-local filesystem or
S3-compatible secondary storage. Filesystem profiles use `kind = "filesystem"`
and an absolute `path`. S3 profiles use `kind = "s3"`, `endpoint`, `region`,
`bucket`, optional `prefix`, `credential_profile`, and `force_path_style`.
`priority` controls read preference after current health; lower values are
preferred. The complete workflow and examples are in
[Multi-store Storage](usage/storage.md).

Bindings are loaded once when the daemon starts. They are deliberately absent
from logical metadata, audit evidence, and backups. Restart after editing a
profile; Docbank reports a typed stale-configuration error rather than
hot-reloading credentials or paths underneath active jobs.

S3 endpoints must use authenticated HTTPS, including loopback services.
Docbank rejects plain HTTP: a loopback port does not identify which process
receives an ownership marker or document bytes.

Active stores must use separate locations:

- A filesystem root must not overlap the vault, a watched inbox, or another
  filesystem store.
- S3 prefixes must not be equal or nested within the same canonical endpoint
  and bucket.

Docbank also checks each store's ownership marker and epoch, a value that
identifies the current ownership claim. These checks catch aliases that path
comparisons cannot recognize.

Secondary objects are verified but not encrypted by Docbank. Raw readers of a
filesystem root or S3 prefix can decode document content without the daemon API
key. Store profiles therefore belong only on owner-controlled storage, or on
storage protected by independently controlled filesystem, bucket, or KMS
encryption and access policy.

### Bind validation

The daemon validates its listening address at startup. An invalid setting makes
`docbank daemon run` fail immediately:

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
