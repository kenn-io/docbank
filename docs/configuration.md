---
last_edited: 2026-11-07
title: Configuration
description: Vault location, data layout, config.toml, and environment variables.
---

# Configuration

By default, Docbank stores its vault in `~/.docbank/` and runs with default
settings. To pick another directory, set `DOCBANK_HOME`. To change how the
daemon runs, add a `config.toml` file inside the vault directory.

In `config.toml`, you can configure the listening address, authentication, idle
timeout, backup repository, watched inboxes, the optional daemon MCP HTTP
listener and credential binding, and secondary-store connections. If your vault
uses only a primary store, you do not need a configuration file. Each registered
secondary store needs a matching connection profile after a restart, and backup
commands need either a configured repository or a `--repo` flag.

For optional pinned local email rendering, see
[Email PDF configuration](usage/email-pdf.md#configure-the-renderer).

For optional bounded PDF and density-qualified PNG rendering, see
[Verified page images](architecture/page-images.md#configure-the-optional-runtime).
The `[page_runtime]` section is independent of text and embedding providers.

## Vault location

All data defaults to `~/.docbank/`. To use another location, set the
`DOCBANK_HOME` environment variable:

```bash
export DOCBANK_HOME=/Volumes/Archive/docbank
```

After you select a vault, run `docbank vault info` to inspect its canonical path
and vault ID. This is useful when one machine holds several independent Docbank
archives:

```bash
DOCBANK_HOME=/Volumes/Archive/docbank docbank vault info
```

Docbank creates this directory layout on first use:

```
~/.docbank/
├── docbank.db           # SQLite: virtual tree, metadata, FTS index
├── blobs/
│   ├── <aa>/<sha256>    # raw content-addressed document bytes
│   ├── <aa>/<sha256>.zst # managed compressed loose representation
│   └── tmp/             # staging for in-flight writes
├── email-pdf-spool/     # isolated email PDF input/output staging
├── logs/                # JSON logs from background daemons
├── web-launch/          # owner-private browser authentication handoff
├── web-downloads/       # private, temporary verified browser downloads
├── config.toml          # optional; see below
├── lane-controls.json   # local job pause and concurrency settings
├── vault.lock           # advisory lock, held by a daemon or target restore
└── daemon.<pid>.json    # runtime record of a live daemon
```

`docbank.db` stores the local catalog, and `blobs/` holds the built-in primary
store. Files ending in `.zst` contain compressed content, while hashes and
document sizes always describe the decoded bytes. Docbank may compress new
writes when the savings justify it, and it reads existing raw files without
converting them.

If you customize `config.toml`, back it up separately. It can contain an
`api_key`, filesystem paths, S3 coordinates, and credential-profile names.

`vault.lock`, `daemon.<pid>.json`, `web-launch/`, and `web-downloads/`
coordinate running processes. You can omit them and `lane-controls.json` from
backups, and delete them only when no daemon or restore is running. On graceful
shutdown, `docbank daemon stop` removes its own runtime record.

To capture every retained blob from a verified location, including content held
only in a secondary store, run `docbank backup create`. You can restore the
resulting backup without recreating the original store layout.

A stopped copy of `docbank.db` and `blobs/` is complete only when the primary
store is an authorized location for every retained blob. Copying `config.toml`
saves secondary-store coordinates, but it does not copy their content. Stop the
daemon before taking a filesystem snapshot. See
[Vault lifecycle](usage/lifecycle.md#take-a-coherent-backup).

Docbank also keeps persistent per-user coordination files under
`~/.local/state/docbank/target-locks`, using the home directory from your
operating-system account record. They contain no document data, but do not
delete them. Daemons and restores use their stable identities to exclude
overlapping vault trees, including simultaneous restores whose target trees
overlap, and to serialize daemon launch before the launcher owns or creates the
vault root.

!!! warning
    Do not edit or prune `blobs/` or a secondary namespace by hand. The
    database authorizes physical files, including files for prior document
    versions. To reclaim space, run `docbank trash empty --run`,
    `docbank vault gc --run`, and `docbank storage repack` for dead packed
    payload. To check integrity, run `docbank vault verify`.

## config.toml

The `config.toml` file is optional. When the daemon starts, including through an
explicit `docbank daemon run` or `docbank daemon start`, it validates the entire
`$DOCBANK_HOME/config.toml` file and inherits your shell's environment. The
daemon treats any unrecognized key as a typo and rejects it at startup instead
of ignoring it. Invalid policy reports `daemon config invalid:` followed by the
section and reason.

`docbank mcp --transport http` and `docbank web` skip unrelated policy and
daemon-only environment overrides, whether a daemon is running or not.
MCP reads the whole `[mcp]` table and its
named credential binding, and web reads the whole `[web]` table. Both launchers
check the types of every setting in those tables. For example, an invalid
`session_lifetime` or a `public_origin` with the wrong type stops the web
launcher even with a healthy daemon running. Both launchers reject TOML syntax
errors anywhere in the file, and reject unknown keys and bad values, such as
`enabled = 'yes'`, only in the sections they read. If no compatible daemon is
running, they start one. Data commands never read `config.toml`; they use the
daemon's startup settings and start a daemon if needed.

Nonempty environment variables override the supported startup fields after
Docbank loads `config.toml`. Empty values preserve the TOML settings, and
overrides are never written back to disk. `DOCBANK_HOME` selects the vault.
Named credential bindings read their configured environment variables, and MCP
bindings also accept mounted files through corresponding `_FILE` variables.
Backup commands can override their configured repository with `--repo`.

```toml
# ~/.docbank/config.toml — optional, defaults shown
[server]
bind_addr = "127.0.0.1"
api_port = 0          # 0 = ephemeral; clients discover the real port
                      # from the runtime record
api_key = ""          # empty = ephemeral per-run key (loopback only)
allowed_hosts = []   # additional Host names, optionally with a port
idle_timeout = "30m"  # automatically started daemons only; "0" = never

[web]
enabled = true
base_path = ""                 # empty = root; e.g. "/archive" (no trailing slash)
public_origin = ""             # empty = loopback web origin; set = 32+ char api_key
allowed_hosts = []             # extra exact host[:port] authorities; needs public_origin
trust_private_network = false  # explicit trust for non-loopback HTTP
session_lifetime = "24h"       # key-login lifetime, 1m..2160h; 0 = 24h

[mcp.http]
# enabled = true        # unset = hosted for a loopback API once the bearer resolves
port = 0                # 0 = serve /mcp on the API port; otherwise a loopback port
credential_binding = "" # empty = HTTP MCP cannot start without an env binding
allowed_hosts = []      # additional MCP HTTP Host names, optionally with ports
public_origin = ""      # e.g. https://archive.example.test behind an HTTPS proxy

[backup]
repo = ""           # no implicit repository; set a path or pass --repo
zstd_level = 0      # 0 = Kit default; otherwise 1-19
interval = "0"      # disabled; for example, "24h"
keep_last = 0       # choose a positive retained count before enabling

[storage]
pack_interval = "0"        # disabled; for example, "1h"
pack_max_bytes = 268435456  # 256 MiB soft raw-byte budget per run

[[watch]]
name = "agent-sessions"
source = "~/agent-sessions"
destination = "/archives/agents"
tags = ["receipts"] # optional; first run: docbank tag create receipts
settle_time = "30s"
minimum_age = "168h" # optional; 7 days since source modification
scan_interval = "5s"
capture_on_change = false # opt in to prompt reads for working files
exclude = [".DS_Store", "cache/"]
default_excludes = true # skip known partial files and sync conflicts
exclude_patterns = ["*.swp"]
```

- **`bind_addr`.** The network interface the API listens on. Defaults to
  loopback. An explicit non-loopback IP, including a wildcard, requires a
  configured `api_key`. Setting a non-loopback address opts into plain HTTP on
  that network. Use a trusted network, an encrypted tunnel, or an HTTPS proxy.
  See [Bind validation](#bind-validation).
- **`allowed_hosts`.** Additional accepted HTTP `Host` values. Provide an IP or
  ASCII DNS name, optionally with a port, such as `docbank:8485`. An entry
  without a port permits that name on any port. The daemon also accepts loopback
  hosts, the concrete bind IP, and its dedicated browser origin. Wildcard bind
  addresses do not permit arbitrary `Host` names. The daemon rejects URLs,
  wildcard names, zero ports, and IPv6 zone identifiers.
- **`api_port`.** The TCP port for the API. Setting `0` picks an ephemeral port.
  The CLI does not need to know the port in advance, because it discovers a
  loopback endpoint from the daemon's runtime record. A concrete non-loopback
  bind adds a separate ephemeral loopback listener for local CLI and MCP
  clients, and both listeners enforce the same authentication and server-path
  import rules. The runtime record's `network_address` metadata carries the
  concrete network endpoint and its assigned port. For remote clients that
  cannot read the record, set a fixed port.
- **`api_key`.** The API authentication key. The daemon checks `X-Api-Key` or
  `Authorization: Bearer` on every authenticated request. If you leave this
  empty, the daemon generates a key at startup and publishes it to same-user
  clients in the runtime record. Set a fixed key when a client cannot read that
  record, such as a client connecting over an SSH tunnel from another machine.
- **`idle_timeout`.** How long an automatically started daemon waits without
  requests before exiting on its own. Setting `"0"` disables idle shutdown. A
  fresh `daemon start`, `daemon restart`, or foreground `daemon run` ignores
  this setting and never idles out. Because `daemon start` reuses a compatible
  automatic daemon with its existing idle policy, run `daemon restart` to keep
  it running. A daemon hosting MCP HTTP also never idles out.
- **`[web] enabled`.** Controls whether the daemon serves the embedded web
  application. Running `docbank web` starts or reconnects to the compatible
  daemon and opens an authenticated browser session on a fresh per-daemon
  loopback origin, independent of a configured `api_port`. If you disable it,
  the daemon returns 404 for `/`, `/searches`, `/productions`, `/imports`,
  `/trash`, `/photos`, `/photos/hidden`, `/photos/people`, `/photos/map`,
  `/photos/map/{set_id}`, `/photos/albums`, canonical `/photos/albums/{id}`, and
  `/assets/`. The API and `/docs` remain unaffected. See
  [Web application](usage/web.md).
- **`[web] base_path`.** Mounts browser pages, assets, and API requests under a
  path such as `/archive`. An empty value mounts them at `/`. Use ASCII letters,
  digits, `-`, `.`, `_`, and `~` in nonempty segments, and omit the trailing
  slash. The daemon rejects dot segments and reserved API prefixes such as
  `/api`, `/health`, and `/docs`. The CLI keeps using its root API endpoints.
  See [proxy subpaths](usage/web.md#serve-docbank-under-a-path).
- **`[mcp.http] enabled`.** Hosts read-only MCP HTTP in the daemon. When unset,
  the daemon hosts MCP HTTP if the API binds loopback, `port` is unset, and the
  MCP bearer resolves in the daemon's environment; otherwise it logs the reason
  and starts without MCP. Setting this to `true` requires the bearer on every
  daemon start, including automatic startup from a CLI command whose environment
  lacks it. Setting it to `false` never reads the bearer.
  `DOCBANK_MCP_HTTP_ENABLED` overrides TOML.
- **`[mcp.http] port`.** The port for daemon-hosted MCP HTTP. Setting `0`, or
  matching the API port, serves MCP at `/mcp` on the API listener, including a
  network API listener. Any other port binds a separate listener on loopback.
  `DOCBANK_MCP_HTTP_PORT` overrides TOML. Standalone
  `docbank mcp --transport http` uses only its own `--listen` flag.
- **`[mcp.http] credential_binding`.** Names the separate inbound credential
  used by both daemon-hosted MCP HTTP and `docbank mcp --transport http`. An
  empty value leaves stdio available, while HTTP startup fails unless an
  environment binding is selected. See
  [MCP HTTP credential](#mcp-http-credential).
- **`[mcp.http] allowed_hosts`.** Additional accepted MCP HTTP authorities. See
  [MCP network access](usage/mcp.md#connect-over-a-trusted-network).
- **`[mcp.http] public_origin`.** The client-facing HTTP or HTTPS origin for
  MCP behind a proxy. The proxy must preserve the `Host` header. Docbank accepts
  the origin's authority without adding it to `allowed_hosts`; use
  `allowed_hosts` for any additional client authorities. If the proxy rewrites
  `Host`, the returned URL uses the validated backend origin instead.
- **`[backup] repo`.** The default snapshot repository used when a backup
  command or API request omits `repo`. A `~/...` path expands against the daemon
  user's home directory, and a relative path resolves beneath `$DOCBANK_HOME`.
  In normal deployments, keep the repository outside the live vault.
- **`[backup] zstd_level`.** The repository compression level. Setting `0` uses
  Kit's default; other values must be between `1` and `19`.
- **`[backup] interval`.** Schedules captures at this interval. Setting `"0"`
  disables the schedule. An enabled schedule requires a configured `repo` and a
  positive `keep_last`. Values must be nonnegative, and enabled intervals must
  be at least one minute. A `docbank backup schedule set interval` override
  takes precedence over this value, and `docbank backup schedule reset interval`
  returns control to `config.toml`.
- **`[backup] keep_last`.** The number of scheduled snapshots to retain after
  each capture. Values must be nonnegative. Manual snapshots remain outside this
  policy. You can override this at runtime with
  `docbank backup schedule set keep_last`.
- **`[storage] pack_interval`.** Schedules non-destructive packing of
  authorized loose blobs. Setting `"0"` disables the schedule. When enabled, a
  pass runs once at daemon startup and then at this interval.
- **`[storage] pack_max_bytes`.** The soft raw-byte budget for each scheduled
  run. It must be positive when `pack_interval` is enabled. Any remaining loose
  content waits for a later run.

After changing backup scheduling configuration, restart the daemon. See
[Schedule backups](usage/backup.md#schedule-backups) for initialization,
retention, scheduled tag rules, retries, and stale warnings.

Scheduled packing appears as the `storage:pack` job and keeps an auto-started
daemon alive so the schedule can run. It uses the same maintenance gate as
`docbank storage pack`, which means ordinary mutations may briefly receive
`maintenance_busy` and can retry. Each scheduled pass requests cancellation at
`pack_interval` and releases the gate when the pass returns, leaving any
remaining indexed loose content for the next pass. Work that ignores context can
delay gate release. Set `pack_interval` comfortably above the time needed to
build and seal one pack so each pass can make progress. Repeated
`automatic packing canceled at interval; retrying` warnings can indicate that
the interval is too short. Automatic packing does not delete logical content and
does not run GC or repack; you still need to request those reclamation
operations explicitly.

### Kata connection

The optional `[integrations.kata]` section connects document citations to a
Kata project. It is disabled by default and validates only when a Kata
operation uses it. See
[Cite documents in Kata](usage/kata-evidence.md#connect-kata) for fields,
credential sources, and endpoint restrictions.

### MCP HTTP credential

The MCP HTTP listener requires a named credential binding. Configuration stores
only the environment-variable name, and you supply the bearer to the hosting
process through that variable or its `_FILE` counterpart:

```toml
[mcp.http]
credential_binding = "credential:mcp-http"

[credential_bindings.mcp-http]
environment_variable = "DOCBANK_MCP_HTTP_TOKEN"
```

Binding names must start with a lowercase letter, contain only lowercase
letters, digits, `_`, or `-`, and be at most 63 characters long. The configured
environment-variable name must use ordinary shell-variable syntax. The bearer
must be non-empty, contain no spaces or control bytes, and be at most 4,096
bytes.

To start without TOML, set `DOCBANK_MCP_HTTP_TOKEN` or
`DOCBANK_MCP_HTTP_TOKEN_FILE`. This selects a process-local named binding and
overrides `[mcp.http] credential_binding`. The daemon resolves its value only
when MCP hosting is enabled explicitly or by default. It does not replace
existing entries in `[credential_bindings]`; if the preferred internal name is
already taken, Docbank picks an unused name. See
[MCP network access](usage/mcp.md#connect-over-a-trusted-network).

Docbank resolves the bearer once when the MCP HTTP listener starts. To rotate
it, restart the hosting daemon or standalone MCP process. The bearer must
differ from `[server] api_key` and from any ephemeral daemon key published in
the runtime record. HTTP startup checks the effective daemon key and refuses a
reused value, and standalone MCP keeps that exclusion active when it reacquires
a restarted daemon. Daemon-hosted MCP always uses its own daemon API.

Docbank does not support a raw bearer field in `config.toml`, a command-line
token flag, a URL credential, or runtime-record publication. Supply the
environment variable to the hosting process through an owner-controlled secret
or process manager. This is a fixed bearer, not OAuth. See
[Model Context Protocol](usage/mcp.md) for the complete transport boundary.

### Watched inboxes

Each `[[watch]]` entry tells the daemon to scan one local directory recursively.
By default, settled mode polls at `scan_interval`. To read files promptly after
filesystem notifications with periodic reconciliation at the same interval, set
`capture_on_change = true`.

`source` must be an absolute path or begin with `~/`, and `destination` is an
absolute path in Docbank's virtual tree. The scanner ignores symlinks and other
non-regular entries inside `source`. `exclude` matches literal names or relative
paths, including names that contain `*`, `?`, or `[`. For glob rules, add
`exclude_patterns` using the same
[syntax as `docbank document add`](usage/importing.md#choose-files-with-include-and-exclude-rules).
A pattern without `/` matches basenames at any depth, while a pattern with `/`
matches paths relative to the source root. Use `/` separators on every platform.
Matching is case-sensitive, `*` and `?` do not cross `/`, and `**` is not
recursive. A match in either list skips the entry, and the scanner prunes
matching directories during both startup checks and scanning.

`default_excludes` defaults to `true`. With it enabled, every watch skips
Syncthing partial files, `.syncthing.*.tmp` and `~syncthing~*.tmp`, Syncthing
conflict copies, `*.sync-conflict-*`, and incomplete Chrome downloads,
`*.crdownload`. These patterns add to your configured rules and use the same
exclusion matcher and directory pruning as `exclude_patterns`. Conflict copies
matching `*.sync-conflict-*` are finished files that Docbank does not archive
unless you set `default_excludes = false`. Set `default_excludes = false`
to use only your configured exclusions when those filenames are intentional.
Exclusions never remove documents that are already archived.

For example, here is a Syncthing inbox configuration:

```toml
[[watch]]
name = "synced-notes"
source = "~/synced-notes"
destination = "/notes"
exclude = [".stversions", ".git"]
```

To assign existing tags when a document first arrives, first run
`docbank tag create receipts` and `docbank tag create 2026`, then add
`tags = ["receipts", "2026"]`. Docbank commits the document and its tags
together. If a configured tag is missing, Docbank holds new imports while edits
to existing documents keep recording and preserve manual tag changes. Watch
status shows the tag error separately from the runner's error. Create the
missing tag to resume imports on the next scan. If you rename a configured tag,
update the watch configuration to match.

Empty, absolute, parent-traversal, malformed, and backslash-containing glob
rules fail watcher or folder-scanner construction and report the watch name.
After changing watch rules, restart the daemon.

Traversal stays on the source's filesystem mount and does not enter symlinks,
Windows directory reparse points, or nested mounts. If you also want to import
content from a separate mounted filesystem, configure another `[[watch]]` entry.
This boundary prevents an aliased vault directory from becoming its own input.

The daemon checks each file in this order:

1. In settled mode, observe the same filesystem identity, size, modification
   time, and native change timestamp when available for the complete
   `settle_time`; fall back to size and modification time when that timestamp is
   absent. Prompt mode skips only this delay.
2. Require the source modification time to satisfy `minimum_age`, if enabled.
3. Read the file, then check that the source path still names the same object.
   If it changed, the daemon does not accept that read as a Docbank node.

For example, `minimum_age = "168h"` requires a file to be at least seven days
old and, in settled mode, unchanged for the complete settle window. The default
`"0s"` disables this age check. The source timestamp still applies after a
daemon restart, though only settled mode starts a new in-memory settle window.
This helps with sessions or recordings that pause before they finish.

`scan_interval` controls how often the daemon runs periodic scans. Zero settle
and scan values select the defaults shown above. `capture_on_change` defaults to
`false`. Prompt mode still uses the configured positive `settle_time` value for
configuration compatibility, without delaying admission. Any other `settle_time`
or `scan_interval` value must be positive, and `minimum_age` must not be
negative.

Minimum age is a conservative time policy, not proof that the producing
application closed a file. Pick a window that fits the producer, or watch a
directory that receives only completed files when the producer offers a
close-and-rename handoff.

If a file disappears during observation or is still held exclusively by a
Windows producer, the daemon treats it as unsettled and retries it from a fresh
window. If a retryable observation failure, such as a file another program holds
open, prevents a complete source inventory, the daemon still imports readable
settled files. It defers only work that needs the whole tree: rename continuity,
new paths that share native identity with an accepted source the scan did not
see, and forgetting sources it did not see. The watch job keeps running with a
visible error, and the daemon logs the incomplete-inventory block once. A
complete scan resumes the deferred work and clears that signal, while any active
notification failure remains visible.

If a symlink replaces a known source file, the daemon skips it without following
its target. If a directory replaces that file, the daemon uses normal confined
traversal and exact-destination rules. The archived file and durable cursor
remain, and an occupied old path never authorizes a rename join. If a symlink
hides previously observed descendants, a known path becomes another nonregular
file, or a known path crosses a filesystem boundary, the daemon skips that path
and restarts the settle window for files below it. Reconciliation is deferred
the same way, and the path appears in the watch job error until it recovers.
Other permission or native observation failures stop the watch with a job error.

Docbank identifies each watched source by `(name, relative source path)`. If you
move the local `source` root, keep the watch name. Later content changes then
continue adding versions to the same Docbank node, even if someone moved that
node in the virtual tree.

A live, unambiguous source rename can move that watch cursor to the current
relative path while keeping its original provenance and archived virtual
location unchanged. The daemon needs unique native object and birth evidence
from a complete scan; see
[watched inboxes](usage/importing.md#continuously-ingest-a-local-inbox) for the
limits. Each current cursor owns one Docbank node, and deleting a source file
does not delete its Docbank node.

After each complete scan, Docbank records when a cursor's source was first
observed gone. The deepest directory listed on its path must lack it, so a
deleted or renamed folder marks every file below it, and a directory or symlink
at the old filename counts as gone. A folder that still exists but was not
listed, such as a symlink, skipped subtree, excluded path, or filesystem
boundary, leaves the files below it unchanged, as do uncertain rename candidates
and incomplete scans. Returning a regular file or completing a live rename
clears the mark. Docbank observes trashed documents too, without restoring them.

These observations stay local to storage and are not exported in metadata.
Startup clears marks for watches that are no longer configured. You can omit
marked documents from search and the document catalog with `source_present`.
Defaults include them, and documents owned by push are always included by this
filter.

Docbank separately remembers the last bytes accepted from each source. If
someone edits or reverts the Docbank node while the source stays unchanged, a
daemon restart does not overwrite that working version. Only a later byte change
at the watched source appends another version.

To queue a processing profile's OCR or text-extraction stage after each added or
updated version, set `profile = "mistral-ocr"` in a `[[watch]]` entry. The
profile must include a rendition that accepts original documents; photo vision
and embedding-only profiles are not supported here. Omit both routing forms to
use only baseline plain-text extraction.

For a mixed folder, use
`profiles = { document = "docling", presentation = "docling", spreadsheet = "docling", image = "docling", audio_video = "asr" }`
instead of `profile`. You cannot set both forms in the same watch. The
`document` family covers PDF, EPUB ebooks, and the word-processing formats
DOCX, DOC, ODT, and RTF. Keys use search `media_family` values: `email`,
`document`, `spreadsheet`, `presentation`, `image`, `audio_video`, `text`,
`source_code`, `web`, `calendar`, `archive`, `cad`, `unknown`. Each value names
a profile with an original-file rendition. Only the matching family's rendition
is queued, and unmapped families get baseline extraction only. The Docbank
Docling document adapter accepts PDF, PPTX, XLSX, plain text, Markdown, PNG, and
JPEG; other files in those families are ineligible. Docbank's Docling ASR
adapter accepts only MP3 and WAV; M4A, MP4, and MOV files in `audio_video`
are skipped as ineligible. No current automatic transcription adapter extracts
MP4 audio. Photo descriptions and image embeddings continue to use vault-wide
consent, independently of watches.

The rendition stage requires current consent for that profile configuration.
Without it, files still import with baseline plain-text extraction while the
configured rendition stays unprocessed.
[Grant consent with `docbank processing build --consent`](usage/document-processing.md)
for the configured profile to process subsequent imports. `docbank watch list`
and the web app's **All workers** table show each family's profile and whether
consent is required, expired, or revoked, while scanner errors remain separate.
Unsupported files are skipped, and an unexpected admission failure is logged
once. The document stays imported and available for manual processing with
`docbank processing build`. A newer save can supersede older queued work.
Consent skips and missing or stale document authority end admission quietly, and
imports stay accepted. A daemon stop between import and queue admission can
leave a version unprocessed; run explicit processing for that version.

Scheduling happens only at import. Granting consent later or adding a profile
does not process older imports; run explicit processing for those documents.
Unchanged rescans add no work. The existing durable workers execute the queued
rendition, and embedding stages require explicit profile execution.

Watchers run as jobs named `watch:<name>`. `docbank watch list` and
`GET /api/v1/watches` pair each runner's state with its effective source,
destination, tags, capture mode (`settled` or `prompt`), settle window, minimum
source age, scan interval, and exclusion policy. JSON output reports
`capture_on_change` as a boolean. `docbank watch list` shows a waiting watcher
as `idle` and an ingesting watcher as `running`. `EXCLUDES` counts both literal
and glob rules, including the default patterns unless `default_excludes = false`.
When an agent needs the separate `exclude` and `exclude_patterns` lists, pass
`--json`. `docbank jobs` and `GET /api/v1/jobs` remain the all-task view. A
permanent source, destination, or read failure puts the named job in the failed
state and records the reason; restart the daemon after correcting the problem.
Per-file successes are written to the daemon log.

Prompt notifications are best effort: coalesced events and slow ingestion can
miss intervening writes, and the watcher may capture readable intermediate
content. The scanner still enforces the same root, exclusion, symlink, mount,
stable-read, and mutation boundaries. Notification hints hold at most 1,024
pending paths, and overflow triggers full reconciliation. Native subscriptions
use at most 32,768 entry units, counted as one plus the current immediate entry
count for each admitted directory. That count covers handles a macOS backend may
open for children, though the OS can fail earlier. A backend or budget failure
closes all subscriptions and visibly degrades to periodic scans until restart.
If a directory vanishes during subscription, the watcher retries it once at the
next periodic scan. The watch job and log report the failure and any
restoration.

A configured watch keeps a background daemon alive regardless of `idle_timeout`.

To inspect the source facts recorded for an imported file, run
`docbank document provenance <path-or-id>` or call
`GET /api/v1/nodes/{id}/provenance`. Provenance is distinct from job status: it
survives daemon restarts and records where successfully ingested content came
from, whereas `docbank jobs` describes only the current daemon run.

Watched inboxes never modify or delete their source files. Watch configuration
is machine-local and is not part of metadata-v1 backup and restore, though
portable metadata preserves the watch name, relative path, node mapping, and
last accepted content identity. The watcher does not pack content itself. To
pack accumulated loose content automatically, configure
`[storage] pack_interval`. GC and repack still run only on request.

### Photo descriptions

Photo descriptions send the cached, stripped 512px grid JPEG to an
OpenAI-compatible `POST /v1/chat/completions` endpoint that supports image input
and `max_completion_tokens`. The preview has normalized orientation and color
and fits the 512px input profile. `max_document_bytes` caps the preview upload,
though the provider rejects any preview over 4 MiB regardless of how high you
set it, and `max_response_bytes` is at most 1 MiB. The original photo can be
larger. Vision profiles require every `retain_*` flag to be `false`.

The example below uses a named environment credential and the
[shared transport settings](#supplied-audio-transcription). Use a root origin, a
positive request timeout of at most ten minutes, and HTTPS for
`hosted_provider`. Profiles that share a descriptor must share endpoint,
credential, and runtime settings.

Compute both fingerprints with the release binary using the exact model, trust
boundary, endpoint, and deployment fingerprint for your configuration. This
command runs locally without a daemon or provider credential:

```sh
docbank processing vision-fingerprints --model vision-model --trust-boundary operator_network --endpoint http://127.0.0.1:8080 --deployment-fingerprint aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

Copy the printed `descriptor_fingerprint` and `disclosure_fingerprint`
assignments into the tables below, add them to `config.toml`, and set
`DOCBANK_VISION_KEY` in the daemon's environment. When the vision prompt
changes, including after an upgrade that adds orientation support, recompute
these pinned fingerprints, review the changed processing plan, and grant fresh
consent. The other fingerprints identify synthetic policies; use stable
identities for your own policies and update them when those policies change.

```toml
[credential_bindings.vision]
environment_variable = "DOCBANK_VISION_KEY"

[rendition_profiles.vision]
adapter_contract = "docbank-compatible-vision/v1"
model = "vision-model"
descriptor_id = "compatible-vision.describe-v1"
descriptor_fingerprint = "<descriptor_fingerprint>"
disclosure_fingerprint = "<disclosure_fingerprint>"
deployment_fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
authorization_fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
upload_options_fingerprint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
credential_binding = "credential:vision"
trust_boundary = "operator_network"
disclose_filename = false
requested_artifacts = ["structured_evidence"]
max_document_bytes = 4194304
max_response_bytes = 1048576
max_units = 1

[rendition_profiles.vision.runtime]
endpoint = "http://127.0.0.1:8080"
request_timeout = "2m"
allowed_cidrs = ["127.0.0.0/8"]
proxy_mode = "disabled"
connect_timeout = "10s"
keep_alive = "30s"
tls_handshake_timeout = "10s"

[retrieval_profiles.photos]
lexical_limit = 40
vector_limit = 60

[processing_profiles.photos]
rendition = "vision"
retrieval = "photos"
trust_boundary = "operator_network"
attachment_policy_fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
completeness_fingerprint = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
consent_fingerprint = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
lexical_segmenter_fingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
normalizer_fingerprint = "2222222222222222222222222222222222222222222222222222222222222222"
sanitizer_fingerprint = "3333333333333333333333333333333333333333333333333333333333333333"
max_document_chars = 100000
max_segment_runes = 2000
max_unit_runes = 100000
retain_provider_markdown = false
retain_sanitized_markdown = false
retain_typed_artifacts = false
```

Automatic description picks the profile with the lexicographically smallest name
among those with active [consent](usage/document-processing.md#describe-photos)
whose provider passes its probe. If the probe for the smallest name fails, the
next consented profile receives the photos. Docbank checks readiness on demand
when a description starts, and status output does not list it. Automatic work
leaves settled descriptions alone.

A consented explicit build under another provider fingerprint can refresh
rotation even when the photo already has a description. Pending and accepted
caption and tag decisions stay settled. After a dismissal, an explicit build
with a different provider fingerprint can offer replacement text along with the
new rotation answer.

### Image and meme understanding

To search JPEG, PNG, and WebP files by generated descriptions and possible meme
meaning, configure an image-understanding profile. It keeps four labeled
sections: visible text as a best-effort transcription, literal description,
possible meaning as an interpretation, and uncertainty or missing context. The
output is machine-generated, with degraded provenance and no verified text
coordinates. A meme does not establish the sender's or recipient's beliefs,
endorsement, or intent. Models can still misread text or context; inspect the
original when relying on a result.

This task runs through the ordinary rendition pipeline and sends the selected
original image, including any embedded metadata, to your configured endpoint
under that profile's consent. It sends no surrounding messages. Inputs can be at
most 16 MiB and responses at most 1 MiB. It does not use the stripped 512px
photo-suggestion preview, does not create authored captions, tags, or person
identities, and does not run automatic photo-description backfill.

Compute the task-specific fingerprints:

```sh
docbank processing vision-fingerprints --task compatible-vision.understand-v1 --model vision-model --trust-boundary operator_network --endpoint http://127.0.0.1:8080 --deployment-fingerprint aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

The four labeled output sections require `max_units` to be at least four. Paste
the printed assignments into this configuration. The endpoint and credential
settings follow the photo-description transport above.

Sections without supported content are omitted and recorded as omissions. If the
model returns no supported content in any section, the job fails without
retrying that response.

```toml
[credential_bindings.image_understanding]
environment_variable = "DOCBANK_VISION_KEY"

[rendition_profiles.image_understanding]
adapter_contract = "docbank-compatible-image-understanding/v1"
model = "vision-model"
descriptor_id = "compatible-vision.understand-v1"
descriptor_fingerprint = "<descriptor_fingerprint>"
disclosure_fingerprint = "<disclosure_fingerprint>"
deployment_fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
authorization_fingerprint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
upload_options_fingerprint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
credential_binding = "credential:image_understanding"
trust_boundary = "operator_network"
disclose_filename = false
requested_artifacts = ["structured_evidence"]
max_document_bytes = 16777216
max_response_bytes = 1048576
max_units = 4

[rendition_profiles.image_understanding.runtime]
endpoint = "http://127.0.0.1:8080"
request_timeout = "2m"
allowed_cidrs = ["127.0.0.0/8"]
proxy_mode = "disabled"
connect_timeout = "10s"
keep_alive = "30s"
tls_handshake_timeout = "10s"

[retrieval_profiles.image_understanding]
lexical_limit = 40
vector_limit = 60

[processing_profiles.image_understanding]
rendition = "image_understanding"
retrieval = "image_understanding"
trust_boundary = "operator_network"
attachment_policy_fingerprint = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
completeness_fingerprint = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
consent_fingerprint = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
lexical_segmenter_fingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
normalizer_fingerprint = "2222222222222222222222222222222222222222222222222222222222222222"
sanitizer_fingerprint = "3333333333333333333333333333333333333333333333333333333333333333"
max_document_chars = 100000
max_segment_runes = 2000
max_unit_runes = 100000
retain_provider_markdown = false
retain_sanitized_markdown = true
retain_typed_artifacts = false
```

After adding an image, build its rendition:

```sh
docbank processing plan /images/cartoon.png --profile image_understanding
docbank processing build /images/cartoon.png --profile image_understanding --plan-fingerprint <fingerprint> --consent
```

For semantic and hybrid search, attach an existing text embedding profile with
`input_kind = "rendition_chunk"` and chunk
`formatter = "rendition-chunk-headings/v1"` to this processing profile,
following the [text embedding configuration](#embedding-workers-and-credentials).
Each embedding input and search excerpt carries the caveat and section, for
example
`Machine-generated image understanding; unverified; sender intent unknown / Possible meaning (interpretation)`.
Keyword search works on the generated rendition without an embedding provider.

### Automatic face detection

Face detection stays off until you add a separate vision rendition profile with
`descriptor_id = "compatible-vision.detect-faces-v1"` and a processing profile
that selects it. Reuse the photo-description example above with separate profile
names, and compute its fingerprints for the detection task:

```sh
docbank processing vision-fingerprints --task compatible-vision.detect-faces-v1 --model vision-model --trust-boundary operator_network --endpoint http://127.0.0.1:8080 --deployment-fingerprint aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

Pin the printed fingerprints in that rendition profile, then grant consent for
its processing profile by reviewing
`docbank processing plan <photo-path> --profile <detection-profile>` and running
`docbank processing build <photo-path> --profile <detection-profile> --plan-fingerprint <reviewed-sha256> --consent`.
Consent for descriptions alone does not enable detection. Both tasks send the
same stripped grid preview through the vision adapter, with one request per
task, and each task selects the first ready, consented processing profile by
name.

Detection scans visible, included display photos with ready previews. Once a
scan completes, including a scan with zero faces, automatic and explicit
requests do not rescan that version under the same request fingerprint.
Changing the model, prompt, endpoint, deployment, or another fingerprinted
request policy requires fresh consent and permits a new scan.
Backups preserve faces
and their measurements while leaving scan completion behind; restored photos
can reconcile after fresh consent with one new provider call per photo.

### Document conversion with Docling

For a separately run gateway with durable input and repeatable result storage,
see [Run a bounded conversion worker](usage/conversion-worker.md). Its stable-key
protocol is separate from a native Docling endpoint.

The daemon can send an original PDF, PPTX, XLSX, plain text, Markdown, PNG, or
JPEG to your Docling Serve deployment and retain searchable Markdown. Add a
rendition profile with `adapter_contract = "docbank-docling-document/v1"`,
`descriptor_id = "docling.serve-v1"`, and
`requested_artifacts = ["provider_markdown", "structured_evidence"]`, then
select it from a processing profile with `retain_sanitized_markdown = true`. No
conversion runs on import: review `processing plan`, then grant consent with
`processing build`.

Use the same named `credential_binding = "credential:<name>"` and
[runtime fields as Docling ASR](#supplied-audio-transcription). You must define
the named binding even for a deployment that normally accepts anonymous
requests. The daemon reads its environment variable only when sending a request,
and a missing secret fails that attempt without sending the document. The
endpoint must be an absolute root origin, and the client refuses redirects and
ambient proxies.

HTTP requires `trust_boundary = "operator_network"`. At that boundary, every
`allowed_cidrs` prefix must fit entirely inside a loopback, private, or shared
network: `127.0.0.0/8`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`,
`100.64.0.0/10`, `::1/128`, or `fc00::/7`. The daemon rejects any public or
broader prefix. Hosted deployments use `hosted_provider`, require HTTPS, and
may allow explicit public networks. Optional `spki_sha256` pins require HTTPS
and keep normal certificate verification.

`max_document_bytes` must be positive and at most 1 GiB, and
`max_response_bytes` must be positive and at most 512 MiB. Set `max_units` to
cap the PDF pages, presentation slides, or workbook sheets found during local
inspection; the daemon rejects any source above that limit before upload. The
`provider_markdown` artifact role is required for bounded provider text, and
`structured_evidence` is optional for retained PDF page JSON. Document
conversion has no transcript limit; omit `max_transcript_chars`. Request,
total, poll, and transport timeouts use the ASR bounds below.

The document adapter and trust boundary fix the descriptor. In Go, you can
obtain it with `docling.DocumentDescriptor(boundary)`. The document policy
fingerprint is the SHA-256 of the UTF-8 contract string
`docbank-docling-document/v1`. The descriptor declares Markdown and structured
results, the structured evidence artifact role, and original-file capabilities
for PDF, DOCX, PPTX, XLSX, plain text, Markdown, PNG, and JPEG, and its
fingerprint includes those capabilities and the trust boundary. DOCX remains
ineligible because the current local inspector cannot bound it. HTML, TIFF,
legacy Office formats, audio, and video are outside this daemon document
profile. Docling's own format support cannot override local inspection.

Set `descriptor_fingerprint` to the value for your trust boundary:

| Trust boundary | Descriptor fingerprint |
| --- | --- |
| `operator_network` | `21bcb9a189ebd0c285637a3d0b7bb1f29310080b997a0c043ae5dce05c908b4f` |
| `hosted_provider` | `f5603a0b0cf6ea91a65a141323a025eb205bfb3278f2baffa46e893ebdff0357` |

`disclosure_fingerprint` uses the same NUL-separated formula and Python command
as ASR below, substituting the document adapter contract and descriptor. In Go,
call `docling.DocumentDisclosureFingerprint(descriptor, endpoint, deployment)`.
Before you grant consent, the plan shows the document adapter, Docling provider,
endpoint, deployment, filename disclosure, and retained artifact roles. An
endpoint, deployment, or descriptor mismatch prevents startup. Profiles that
share a descriptor must agree on all effective runtime and credential settings,
and a runtime with no selecting processing profile stays staged.

PDF results can preserve exact page evidence, including blank pages. Each page
unit holds that page's text items and tables, and each table becomes a Markdown
table on the one page named by its provenance. Pages with mapped tables follow
Docling's reading order through the body and then the furniture, while other
pages keep text array order. Docbank escapes cell text so every character is
kept, except that line breaks and repeated spaces in a cell collapse to one
space. Text inside a picture, such as a letterhead or a scanned page, and
captions are ordinary text items, so they stay on their pages too. Docbank never
assigns content to a guessed page. The page evidence is partial, with a field
omission for each unmapped kind, when:

- a table spans several pages, has no provenance, names a page outside the
  result, has a missing or ragged grid, or has a cell that holds other items;
- a table or picture carries a Docling description or classification;
- the result contains key-value, form, or field items.

A described or classified table keeps its cells on the page when its grid and
provenance can be mapped. Its description or classification stays an omission.

Other formats use bounded Markdown with explicitly degraded provenance. The
existing upload authorization, sanitization, publication, and lexical indexing
checks still apply, and original bytes remain unchanged. After you change the
descriptor, queued work fails with `stale_authority`; plan and grant consent
for the changed profile before retrying.

### Document OCR with Mistral

The daemon sends PDFs to Mistral OCR and retains searchable Markdown through its
processing plan and consent flow. Start from the portable rendition and
selecting processing-profile fields in
[Docling document conversion](#document-conversion-with-docling), and set
`adapter_contract = "docbank-mistral-ocr/v1"`,
`descriptor_id = "mistral.ocr-v1"`, `trust_boundary = "hosted_provider"`, and
`requested_artifacts = ["provider_markdown"]`. Images and other formats are
outside this daemon profile.

| Runtime field | Meaning |
| --- | --- |
| `endpoint` | Exactly `https://api.eu.mistral.ai`. |
| `capability_manifest` | Complete authenticated manifest with PDF upload authority. Relative paths resolve against the config root. |
| `request_timeout` | Positive per-attempt limit, at most ten minutes. |
| `allowed_cidrs`, `proxy_mode` | Non-empty provider IP allowlist and `proxy_mode = "disabled"`. |
| `spki_sha256` | Optional lowercase SHA-256 SPKI pins for the provider certificate. |
| `connect_timeout`, `keep_alive`, `tls_handshake_timeout` | Positive transport limits, each at most five minutes. |

Leave `total_timeout`, `poll_interval`, `max_poll_attempts`, and
`max_transcript_chars` at zero. Bind the credential through
`credential_bindings`; a missing secret causes an `authentication` provider
error and a `terminal` job failure. The client makes at most four attempts,
with up to thirty seconds between attempts, all within the daemon's ten-minute
authorization window after inspection and spooling.

Create a directory with five synthetic native seed files named `doc`, `ppt`,
`xls`, `numbers`, and `msg`, using valid files in those formats with no private
content. Each seed's extractable text must include its exact sentinel phrase
from `mistral.ProbeFixtureSentinel`, or the probe records `sentinel_missing`:

<!-- release-commands: expected-failure -->

| Seed | Sentinel phrase |
| --- | --- |
| `doc` | `docbank probe doc cedar 7319` |
| `ppt` | `docbank probe ppt cedar 7319` |
| `xls` | `docbank probe xls cedar 7319` |
| `numbers` | `docbank probe numbers cedar 7319` |
| `msg` | `docbank probe msg cedar 7319` |

Run the probe with the selecting processing profile. It can take several
minutes:

```bash
docbank processing probe --profile ocr --seeds ./mistral-seeds
```

This command runs locally without starting the daemon. It uses the profile's
policy, credential binding, and egress settings, sends the generated fixture
matrix to Mistral, and saves the manifest at the configured
`capability_manifest` path only if it grants the PDF authority the daemon
requires. Even when that check fails, it prints each format's result. To
replace an existing manifest after a successful probe, pass `--overwrite`.
After saving, it prints the `descriptor_fingerprint` and
`disclosure_fingerprint` for the configured deployment and tells you whether
the rendition profile already uses them. Update any values that differ, then
restart the daemon. If you interrupt the probe, it removes its temporary files
and leaves any existing manifest unchanged.

Header and footer extraction stay off, so their text stays in Markdown. The
daemon pins `standard` retention, `default-opt-out` training, the EU region,
`mistral.DefaultModel`, and a normalization identity of 25,000,000 characters.
`max_units` accepts 3 to 5,000, subject to manifest authority, while document
and response limits are 500 MiB and 512 MiB.

The probe prints both fingerprints, and startup reports the expected
`descriptor_fingerprint` if it drifts. You can also compute
`disclosure_fingerprint` with the
[Python disclosure command](#supplied-audio-transcription) or Go's
`mistral.DisclosureFingerprint`. After a model re-pin, re-probe and recompute
both fingerprints. Before consent, the plan shows the adapter, endpoint, model,
and deployment. Profiles that share a descriptor must agree on runtime and
credential settings, and a runtime with no selecting processing profile stays
staged.

Start with this complete configuration. The repeated-digit fingerprints are
placeholders for your policy identities. Set the deployment identity, run the
probe, and replace `descriptor_fingerprint` and `disclosure_fingerprint` with
the values it prints.

For `allowed_cidrs`, use the provider's published address ranges if available,
or resolve `api.eu.mistral.ai` from the daemon host and allow each approved
IPv4 address as `/32` and IPv6 address as `/128`. Every resolved address must
fit the allowlist, and you should review and update it whenever the provider
changes addresses. The example below uses a documentation-only network; replace
it before running the daemon.

```toml
[credential_bindings.mistral]
environment_variable = "MISTRAL_API_KEY"

[rendition_profiles.ocr]
adapter_contract = "docbank-mistral-ocr/v1"
descriptor_id = "mistral.ocr-v1"
descriptor_fingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
authorization_fingerprint = "1111111111111111111111111111111111111111111111111111111111111111"
credential_binding = "credential:mistral"
deployment_fingerprint = "2222222222222222222222222222222222222222222222222222222222222222"
disclosure_fingerprint = "3333333333333333333333333333333333333333333333333333333333333333"
upload_options_fingerprint = "4444444444444444444444444444444444444444444444444444444444444444"
trust_boundary = "hosted_provider"
requested_artifacts = ["provider_markdown"]
max_document_bytes = 1048576
max_response_bytes = 1048576
max_units = 100

[rendition_profiles.ocr.runtime]
endpoint = "https://api.eu.mistral.ai"
capability_manifest = "mistral-capabilities.json"
request_timeout = "2m"
allowed_cidrs = ["192.0.2.0/24"]
proxy_mode = "disabled"
connect_timeout = "10s"
keep_alive = "30s"
tls_handshake_timeout = "10s"

[retrieval_profiles.ocr]
lexical_limit = 20
vector_limit = 20

[processing_profiles.ocr]
rendition = "ocr"
retrieval = "ocr"
attachment_policy_fingerprint = "5555555555555555555555555555555555555555555555555555555555555555"
completeness_fingerprint = "6666666666666666666666666666666666666666666666666666666666666666"
consent_fingerprint = "7777777777777777777777777777777777777777777777777777777777777777"
lexical_segmenter_fingerprint = "8888888888888888888888888888888888888888888888888888888888888888"
normalizer_fingerprint = "9999999999999999999999999999999999999999999999999999999999999999"
sanitizer_fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
max_document_chars = 100000
max_segment_runes = 2000
max_unit_runes = 100000
retain_provider_markdown = true
retain_sanitized_markdown = true
trust_boundary = "vault-primary"
```

Set `MISTRAL_API_KEY` in the daemon's environment, then run
`docbank processing plan /documents/report.pdf --profile ocr` to review the
upload before granting consent with `docbank processing build`.

### Supplied audio transcription

The daemon can transcribe supplied WAV and MP3 files through a configured
Docling Serve deployment. The profile's descriptor, artifact policy, filename
disclosure, and trust boundary are portable, while the endpoint, transport
policy, and secret binding stay local to the daemon.

Set `adapter_contract = "docbank-docling-asr/v1"` and
`credential_binding = "credential:<name>"` on the rendition profile. Its
`runtime` section requires `endpoint`, `request_timeout`, `total_timeout`,
`poll_interval`, `max_poll_attempts`, `allowed_cidrs`, `proxy_mode`, and the
transport timeout fields. You can use `spki_sha256` to pin the deployment
certificate. The endpoint must be a root origin, `allowed_cidrs` must contain
at least one network, and `proxy_mode` must be `"disabled"`. HTTP endpoints
require the `operator_network` trust boundary, and certificate pins require
HTTPS. At the `operator_network` boundary, every `allowed_cidrs` prefix must be
loopback, private, or shared, as in
[document conversion](#document-conversion-with-docling). Any explicit port
must be between 1 and 65535, and redirects are not followed.

Set a positive `max_transcript_chars` on the rendition profile. This limit caps
the generated transcript and is part of the descriptor's policy fingerprint,
while each processing profile keeps its own `max_document_chars` limit for
subsequent processing. A runtime with no selecting profile remains staged and
cannot execute.

After a restart, queued work whose descriptor is no longer configured fails
with `stale_authority`. Plan the work and grant consent for the changed profile
before retrying it.

For this adapter, `disclosure_fingerprint` binds the descriptor, endpoint, and
deployment fingerprint. Recompute it whenever the endpoint or deployment
changes; the daemon rejects a mismatched binding before starting provider work.
For audio, Go applications can call `docling.ASRDisclosureFingerprint` from
`go.kenn.io/docbank/document/docling`. You can also compute the same value from
your config with Python 3.11 or later. Replace the path and profile name in
this command, then copy the output into that profile's `disclosure_fingerprint`:

```sh
python3 - /path/to/config.toml asr <<'PY'
import hashlib
import sys
import tomllib

with open(sys.argv[1], "rb") as source:
    profile = tomllib.load(source)["rendition_profiles"][sys.argv[2]]
values = [profile["adapter_contract"], profile["descriptor_id"],
          profile["descriptor_fingerprint"], profile["runtime"]["endpoint"],
          profile["deployment_fingerprint"]]
print(hashlib.sha256("\0".join(values).encode()).hexdigest())
PY
```

The daemon registers one provider per descriptor fingerprint. Profiles with the
same descriptor must share identical endpoint, credentials, and runtime
settings. Two Docling deployments with the same descriptor cannot run together
in one daemon, and conflicting settings prevent startup.

Set the source ceiling for retained audio under `[media]`:

```toml
[media]
max_source_bytes = 4294967296 # 4 GiB; the default and maximum
pdf_passwords_file = "pdf-passwords.txt" # optional; relative to the config root
```

`pdf_passwords_file` supplies passwords for locked PDFs sent to Docling or
Mistral. The file must be a regular file owned by the current user with Unix
mode `0400` or `0600`, or a private protected Windows ACL, and it can hold at
most 16 KiB and 128 passwords. Put one password per line; Docbank ignores blank
lines and preserves spaces. When you first set or change this path, restart the
daemon. Edits to the file itself take effect on the next build. See
[Password-protected PDFs](usage/document-processing.md#password-protected-pdfs).

`max_source_bytes` accepts 1 through 4,294,967,296 bytes, and `0` selects the
default. The daemon applies this ceiling to media HTTP uploads, shared staging
reservations, original publication, and audio processing admission and worker
preparation, while any smaller processing-profile source limit still applies.
After changing this setting, restart the daemon. Lowering it fails queued
processing for larger sources; to recover, raise it again and retry that work.
Embedded Go applications set `ProcessingOptions.MediaMaxBytes`, where `0` uses
the 4 GiB default.

This setting does not raise document inspection, image or video, transcript,
caption, or hosted-provider bounds. Audio keeps its 24-hour duration and codec
qualification bounds, remote MP4 video originals keep their 20 MiB limit, and
transcript uploads, caption uploads, and the built-in profiles' responses keep
their 16 MiB limits. If a staging disk or quota failure happens, the daemon
releases the reservation, removes the failed file, and returns HTTP 503 with
`media_staging_unavailable`; free disk space and retry. Running out of space
while publishing the durable copy returns the same error. Embedded callers can
match `docbank.ErrMediaStagingUnavailable` with `errors.Is`. Because the
staging reservation limits aggregate bytes, one maximum-size source occupies
the whole reservation until its stage closes. Processing plans for retained
WAV, MP3, or M4A audio above 1 GiB may run past the ordinary 60-second request
deadline while Docbank verifies the full source, though client cancellation and
caller deadlines still apply.

The built-in `supplied-transcript` and `supplied-captions` profiles accept
4 GiB sources, which changes their profile fingerprints. After upgrading a
vault that used either profile:

- Consent granted to the earlier profile no longer applies. Plan and grant
  consent again before submitting or retrying supplied inputs.
- Work queued before the upgrade finishes, but its media operation is marked
  `failed`. Retry it after granting consent.
- Coverage reports and profile-name rendition lookups treat earlier supplied
  transcripts as uncovered until they are processed again, while transcript
  reads and lexical search still return the earlier evidence.

The daemon also registers two local media profiles. `supplied-transcript`
processes supplied WAV, MP3, and M4A transcript text as untimed evidence; M4A
requires audio-only AAC or ALAC, as described in the
[media input contract](cli-reference.md#docbank-media). `supplied-captions`
processes supplied SubRip captions for WAV, MP3, M4A, and MP4 originals as
timed evidence. Both names are reserved for these built-in profiles, and
configuring a profile with either name prevents startup.

The daemon reads the credential from the named environment binding when
the adapter sends a provider request. A missing secret fails that processing
attempt while leaving supplied-media retention, plaintext processing, and the
built-in supplied-transcript path available. After changing the endpoint,
transport policy, or environment binding, restart the daemon.

| Runtime field | Meaning |
| --- | --- |
| `endpoint` | Absolute root Docling Serve origin. |
| `request_timeout`, `total_timeout` | Per-request and complete operation limits, each positive and at most 24 hours. |
| `poll_interval`, `max_poll_attempts` | Delay and count bounds for polling. The interval cannot exceed `total_timeout`, and attempts range from 1 to 10,000. |
| `allowed_cidrs`, `proxy_mode` | Non-empty IP network allowlist and `proxy_mode = "disabled"`. At `operator_network`, only loopback, private, or shared prefixes are allowed. |
| `spki_sha256` | Optional lowercase SHA-256 SPKI pins for the provider certificate; requires HTTPS. |
| `connect_timeout`, `keep_alive`, `tls_handshake_timeout` | Positive transport limits, each at most five minutes. |

### Embedding workers and credentials

If you move an embedding endpoint or change its timeout, you still need the
updated descriptor fingerprint and consent for the current profile. After you
grant consent, Docbank reuses compatible saved vectors, and an unchanged
configuration keeps its existing vector space across upgrades. Legacy BGE-M3
and Qwen3 profiles may require recomputation on their first policy change
because their old records omit deployment revisions.

The daemon runs retained embedding jobs for its configured OpenAI-compatible,
Voyage, and `docbank-standard-embedding-bridge/v2` runtimes. Other
[provider packages](document-understanding.md) are available to Go
applications, and the daemon cannot run them.

An embedding is a numeric representation used to compare document meaning. Each
provider binding publishes its own results, and one provider's failure does not
remove another binding's completed vectors.

Configuring a provider does not prepare inputs or apply a processing profile to
new imports. A job requires all of the following:

- An existing input generation: the retained set of prepared provider inputs.
- A matching processing profile.
- A matching provider descriptor.
- Current consent to disclose the inputs to that provider.

After a restore, the daemon recreates jobs from retained inputs once those
requirements are met again.

`[processing_profiles.<name>]` selects embedding bindings by name in its
`embeddings` array. `[embedding_profiles.<name>]` defines each binding's model,
input kind, dimensions, formatters, compatibility identity, descriptor and
disclosure fingerprints, byte limits, and `optional` or `required` activation.
Chunk bindings also pin their tokenizer and chunk policy in `.chunk`. Use the
fingerprints from the provisioned profile and deployment; arbitrary fingerprints
or an endpoint's model alias do not establish compatibility.

#### Text-service configuration

For OpenAI-compatible text services, prefer Kit's
`[embedding_profiles.<name>.embedder]` schema. Keep Docbank's pinned descriptor,
consent, input kind, byte limits, normalization, model-input contract, and
chunk policy in the existing profile tables.

```toml
[embedding_profiles.semantic]
credential_binding = "credential:embedding-primary"
# The remaining pinned profile fields are also required.

[embedding_profiles.semantic.embedder]
base_url = "https://embedding.example.invalid/v1"
model = "provisioned-model"
dims = 768
fingerprint_salt = "deployment-v1"
batch_size = 8
timeout_seconds = 30
api_key = { env = "DOCBANK_EMBEDDING_PRIMARY_KEY" }

[embedding_profiles.semantic.runtime]
# Retain the provisioned egress policy and max_request_bytes.
# adapter_contract defaults to docbank-openai-compatible-embeddings/v1
# when embedder is present.
```

`base_url` must end in `/v1` or `/v1/embeddings`. Docbank rejects public-IP
HTTP endpoints, and plaintext private-network endpoints require
`trust_private_network = true`. The existing CIDR and proxy controls still
apply. `fingerprint_salt` records the configured model revision and, unless a
`provider_revision_header` is configured, also supplies the deployment epoch.

The API key can be a literal string, `{ env = "NAME" }`, or
`{ file = "/absolute/path/to/private.key" }`. Prefer environment or private-file
references. Kit requires credential files to be private, regular files owned by
the current user. File paths follow Kit's rules: `~/` expands to the daemon
user's home directory, and relative paths use its working directory. Docbank
resolves credential sources for each request rather than at startup, and a
missing secret turns into an authorization failure when that provider is used.
File edits take effect on the next request, while environment changes require
restarting the daemon.

Keep `credential_binding` as the portable name; the immutable profile records
that name, not the secret or its source. If `api_key` is unset, an existing
`credential_bindings` entry still supplies the secret. If neither source is
present, startup reports that the credential source is not configured. Never
put secret values in processing profiles, fingerprints, receipts, backups, or
source-controlled configuration.

Kit's default batch size is 32 and its default timeout is 30 seconds. When you
convert a legacy configuration, set both values to your existing limits instead
of relying on those defaults. Matching effective settings produce the same
canonical identities and reuse existing generations. Changing the model,
revision, or input recipe still requires a matching pinned descriptor and
profile.

This adapter keeps Docbank's document and query formatting and chunk
preparation. Set `input_type_mode` to `"none"` and configure roles in
`model_input`. Leave `model_context_tokens` and `max_batch_tokens` at zero; the
adapter rejects nonzero values because it does not use Kit token packing. The
profile's normalization setting still governs validation, and adopting Kit's
schema does not normalize returned vectors. With `unit_length`, the server must
return unit vectors, and Docbank rejects squared norms that differ from 1 by
more than `1e-4`.

#### Optional EmbeddingGemma 2 text recipe

For optional EmbeddingGemma 2 text retrieval, use the existing
OpenAI-compatible adapter with a `custom/v1` model-input contract. The
[Google model card](https://ai.google.dev/gemma/docs/embeddinggemma/model_card_2)
specifies native 768-dimensional output along with separate search-query and
document prefixes. The recipe below targets Google's
[pinned checkpoint](https://huggingface.co/google/embeddinggemma-2/tree/914f7f89142e33e77833254d9c9b90c3cef7303b),
`google/embeddinggemma-2@914f7f89142e33e77833254d9c9b90c3cef7303b`.

This fragment belongs inside a separately provisioned embedding profile. Keep
its credential binding, egress policy, input and response bounds, chunk policy,
descriptor ID, and formatter IDs. If processing may continue without these
embeddings, set activation to `optional`. Recompute the immutable descriptor,
authorization, and disclosure fingerprints for the complete new profile;
fingerprints copied from another model fail daemon startup. Select the profile
in a processing profile and review its processing plan before granting consent.
See [document processing configuration](usage/configuration.md) and the
[embedding runtime settings](#runtime-settings).

```toml
[embedding_profiles.semantic]
normalization = "unit_length"
compatibility_id = "embeddinggemma2/search/native768/v1"

[embedding_profiles.semantic.embedder]
base_url = "http://127.0.0.1:11434/v1"
model = "embeddinggemma2-text-f32-768-v1"
dims = 768
fingerprint_salt = "914f7f89142e33e77833254d9c9b90c3cef7303b-text-f32-mean-native768-v1"
input_type_mode = "none"
batch_size = 8
timeout_seconds = 30

[embedding_profiles.semantic.model_input]
profile = "custom/v1"
compatibility_id = "embeddinggemma2/search/native768/v1"

[embedding_profiles.semantic.model_input.document]
mode = "text"
template = "title: none | text: {{content}}"

[embedding_profiles.semantic.model_input.query]
mode = "text"
template = "task: search result | query: {{content}}"
```

The loopback endpoint and serving alias are examples; provision the service
separately. The service operator must bind the alias and deployment epoch to
the exact weights, tokenizer, pooling, precision or quantization, and
output-width recipe. `fingerprint_salt` records that assertion in the immutable
revision and policy, without verifying which weights the server actually
loaded. When that recipe changes, replace both the alias and epoch, then create
a matching descriptor and embedding generation. Keep the epoch equal to the
resolved model revision, or use the existing revision-header contract instead.

Docbank applies each template once and preserves the space before `{{content}}`,
and the server must accept these already formatted strings without adding
prompts again. Require mean pooling including the prompts, L2-normalized
output, and `bfloat16` or `float32` activations, never `float16`. The server
must enforce the shared 8,192-token input window, including formatting, along
with the agreed truncation policy. Reserve prompt overhead in your chunk
budget. A character limit, batch size, or byte limit does not enforce
tokenizer admission.

`dims` declares the expected response width. This adapter sends neither
`dimensions` nor `input_type`, and it never slices or normalizes the result.
Docbank rejects wrong-width, nonfinite, zero-norm cosine, and non-unit vectors.
Reduced 512-, 256-, or 128-dimensional output requires an explicit custom
serving contract that truncates and then L2-renormalizes both documents and
queries, plus a new matching descriptor, compatibility identity, and
generation. The current adapter has no `request_dimensions` option. Put literal
formatting in `model_input`; Docbank rejects unsupported `embedder` affix keys
at load time.

This native model recipe is verified against primary sources, and synthetic
HTTP tests cover configuration, literal formatting, identity, and vector
validation. Actual model inference, checkpoint loading, tokenizer admission,
pooling, precision, server prompt behavior, and retrieval quality remain
untested. The recipe transports rendition text and text queries only and does
not advertise original-file image, audio, or video support. Staging this
fragment does not change any default model or existing generation.

#### Legacy text-service configuration

Existing `model`, `dimensions`, `max_batch_items`, runtime `endpoint`,
`model_revision`, `request_timeout`, and named environment credentials remain
supported. For new text-service configurations, prefer the `embedder` table.
Native and multimodal providers keep their existing configuration.

If both forms set the same field, their effective values must agree; for
example, `embedder.dims = 768` conflicts with `dimensions = 1024`. If `api_key`
is set and a named credential entry also exists, both must point to the same
environment variable. Otherwise, remove the legacy entry when switching
sources. Profiles that share a credential name must agree on its source.

```toml
[credential_bindings.embedding-primary]
environment_variable = "DOCBANK_EMBEDDING_PRIMARY_KEY"

[embedding_profiles.semantic]
credential_binding = "credential:embedding-primary"
# The remaining pinned profile fields are also required.
```

A missing or empty secret leaves ordinary document operations available, while
affected embedding jobs record an authorization failure and can recover later.
Startup still fails for an undefined legacy credential binding, invalid runtime
configuration, or mismatched descriptor.

#### Local text indexing

To approve Nomic indexing for the vault and future imports, run
`docbank processing local --server local`. Setup previews the profile and all
provider disclosures, downloads any missing server and model files, and needs
no confirmation. It starts the shared server, grants consent, saves the
approved profile and server in version 1 of `local-text.json` beside the
vault's `config.toml`, and restarts the daemon, leaving your TOML intact.

If you already have an indexed compatible Nomic profile, setup moves it to
Kit's shared per-user server with its extractor, chunks, and vectors; otherwise
it builds a separate `local-text` index with Nomic v1.5 and the plaintext
extractor. Both `auto` and `local` select Kit's shared per-user server, while
other processing profiles keep their configured providers.

The managed provider uses anonymous loopback requests. Consent identifies the
shared service and selection mode, so a new port after a restart keeps the
grant. Revoking consent stops indexing and query provider calls, and a server
startup failure leaves the saved selection unchanged. See
[document processing](usage/document-processing.md#set-up-local-text-search).

The built-in `local-text` profile sends up to 8 inputs per request, waits up to
5 minutes per request, and sends one request at a time. When setup moves an
existing compatible profile, its configured batch size and timeout remain in
effect. Configured remote endpoints keep their own settings.

#### Runtime settings

An optional `[embedding_profiles.<name>.runtime]` section makes a binding
executable on this machine; without it, the daemon does not claim that
binding's work. Runtime configuration and credential sources are machine-local
and must be supplied separately after you restore a vault.

| Fields | Meaning |
| --- | --- |
| `adapter_contract` | `docbank-openai-compatible-embeddings/v1` for rendition chunks, `docbank-voyage-embeddings/v1` for rendition chunks and original files, or `docbank-standard-embedding-bridge/v2` for image inputs and text queries. |
| `endpoint`, `model_revision` | Exact provider endpoint and pinned revision. OpenAI-compatible endpoints are origins without a path; Voyage uses `https://api.voyageai.com/v1`. |
| `deployment_epoch`, `provider_revision_header` | OpenAI-compatible runtimes require exactly one. Voyage text runtimes require an epoch and forbid a revision header. The epoch must equal `model_revision`; an OpenAI-compatible revision header must echo the pinned revision in every response. |
| `capability_manifest` | Voyage original-file runtimes require an absolute path to a capability manifest matching their model, media policy, and descriptor. Voyage text runtimes forbid this field. |
| `request_timeout`, `max_request_bytes` | Bound each provider request. The profile's `max_batch_items`, `max_input_bytes`, and `max_response_bytes` supply the other request/response limits. |
| `allowed_cidrs`, `spki_sha256`, `proxy_mode` | Explicit destination CIDRs, optional TLS public-key pins, and `proxy_mode = "disabled"`. The provider connection enforces this policy. |
| `connect_timeout`, `keep_alive`, `tls_handshake_timeout` | Required positive transport durations, each at most five minutes. |

Request timeouts must also be positive and at most five minutes. Because retry
policy belongs to the worker, runtime configuration has no retry-count or
retry-delay fields. The worker handles transient failures and capacity-driven
batch splits, and records malformed responses separately from rejected document
input.

#### Image/text retrieval

For Photos Meaning and Hybrid search, use `docbank-standard-embedding-bridge/v2`
with `input_kind = "image"`. The bridge receives verified, stripped JPEG grid
previews and text queries in the same vector space while originals and filenames
stay local. Configure the bridge endpoint origin, pinned `model_revision`,
provider disclosure, and explicit transport policy as described above. Grant
image and query-text consent before searching; if consent, provider access, or
an active index is missing, search returns metadata matches with a visible
reason.

In the image binding's runtime config, set `min_query_similarity` to a
threshold calibrated for the pinned model revision. There is no default. It
accepts a finite score for cosine or dot product, and cosine values must fall
in `[-1, 1]`. Other input kinds and L2 bindings reject this setting. Without a
threshold, an image binding uses the `similarity_floor_unset` metadata fallback
for Meaning and Hybrid search.

Docbank applies the threshold to raw similarity before ranking or combining
semantic and metadata results. When every photo falls below the threshold,
Meaning returns an empty result without fallback, while Hybrid keeps metadata
matches. Changing the threshold leaves all fingerprints unchanged and requires
no reindexing.

```toml
[processing_profiles.photos]
embeddings = ["images"]

[embedding_profiles.images]
input_kind = "image"
# Supply the remaining pinned profile fields from the provisioned bridge.

[embedding_profiles.images.runtime]
adapter_contract = "docbank-standard-embedding-bridge/v2"
endpoint = "https://embedding.example.invalid"
model_revision = "provisioned-image-model-v1"
# Example only: calibrate this value for the pinned model revision.
min_query_similarity = 0.2
# Supply the disclosure, credentials, limits, and transport policy above.
```

#### Voyage text retrieval

To embed retained rendition text, including Docling Markdown, use
`docbank-voyage-embeddings/v1` with `input_kind = "rendition_chunk"`. It
supports `voyage-3.5`, `voyage-4-large`, `voyage-4`, and `voyage-4-lite` at
exactly 256, 512, 1024, or 2048 dimensions. Set `descriptor_id` to
`voyage.embeddings-v1`, `trust_boundary` to `hosted_provider`, `metric` to
`cosine`, `normalization` to `unit_length`, and `scalar_encoding` to `float32`,
and use `voyage/document/v1` and `voyage/query/v1` as the formatters.

The custom model-input contract must use native document and query modes. For
example, these sections belong in an otherwise complete embedding profile:

```toml
[embedding_profiles.voyage_text.model_input]
profile = "custom/v1"
compatibility_id = "voyage/retained-text/v1"

[embedding_profiles.voyage_text.model_input.document]
mode = "document"
template = "{{content}}"

[embedding_profiles.voyage_text.model_input.query]
mode = "query"
template = "{{content}}"

[embedding_profiles.voyage_text.runtime]
adapter_contract = "docbank-voyage-embeddings/v1"
endpoint = "https://api.voyageai.com/v1"
model_revision = "text-deployment-v1"
deployment_epoch = "text-deployment-v1"
# Supply the request bounds and sealed egress settings listed above.
```

Set the outer profile's `compatibility_id` to the same value, and supply its
credential binding, chunk policy, disclosure policy, and exact descriptor
fingerprint as you would for other retained text bindings. The fingerprint
includes the model, dimensions, document and query formatting, deployment
epoch, credential binding name, transport bounds, and egress policy, while
excluding credential values. A mismatched descriptor prevents the daemon from
starting that runtime.

You own the epoch. Because Voyage does not return an immutable revision, the
epoch attests that stored document vectors and new query vectors use the same
model deployment. When the hosted model changes, update the epoch and rebuild
vectors. Never use `mutable-alias-export-only` as an epoch. Voyage text
profiles without an epoch remain export-only, and contextual and original-file
profiles do not accept a text deployment epoch.

The adapter sends `input_type = "document"` for chunks and `"query"` for text
queries. It pins `output_dimension`, requests float vectors, disables provider
truncation, omits `encoding_format`, and checks unit length without
renormalizing the response. Voyage may echo each input as a `text` member, so
set `max_response_bytes` high enough for both the vectors and the echoed text.
Semantic and hybrid searches can select this binding, while lexical search does
not send queries to Voyage. Consent and retained input requirements still
apply.

#### Model input

`[embedding_profiles.<name>.model_input]` pins how Docbank formats document and
query inputs. Its `profile` selects a named contract such as `nomic/v1`,
`bge-m3/v1`, `e5/v1`, or `gte/v1`, not an arbitrary provider model name. The
resulting contract must match the binding's `compatibility_id` and provider
descriptor. For `custom/v1`, supply `compatibility_id` along with `.document`
and `.query` encoders, each with `mode` and `template` using `{{content}}`.
Contract validation checks the supported roles and formatting rules.
`query_instruction` is available only to contracts that support it.

Discovery reads in pages and checks once a minute whether another complete pass
is needed. Quiet vaults skip the pass; see
[idle embedding discovery](embedding.md#idle-embedding-discovery) for the wake
triggers. Existing queued work is still checked on the normal one-second idle
cadence. Discovery skips missing or invalid generation bytes so later
candidates can proceed. Later passes retry temporary input failures, while
permanent rejections let discovery settle. Database failures keep their
separate bounded storage-retry behavior. Terminal jobs do not retain superseded
generations after their last embedding set is collected, whereas queued or
running jobs and explicit retention roots keep their inputs.

### Search reranking

A processing profile can opt into one hosted reranking adapter. Because this
section is deployment configuration, it stays outside the portable document
profile and does not change rendition or embedding identities.

```toml
[credential_bindings.search-rerank]
environment_variable = "DOCBANK_SEARCH_RERANK_KEY"

[processing_profiles.private.reranking]
provider = "zeroentropy"
model = "zerank-2"
credential_binding = "credential:search-rerank"
candidate_count = 20
excerpt_bytes = 4096
deadline = "5s"
failure_policy = "degrade"
endpoint = "https://api.zeroentropy.dev"
allowed_cidrs = ["192.0.2.0/24"]
proxy_mode = "disabled"
connect_timeout = "5s"
keep_alive = "30s"
tls_handshake_timeout = "5s"
```

`provider` accepts `zeroentropy` with model `zerank-2`, or `cohere` with
`rerank-v4.0-pro` or `rerank-v4.0-fast`. `candidate_count` must be between 1
and 1,000, and `excerpt_bytes` must be between 1 and 4,096 to cap each UTF-8
excerpt sent to the provider. `deadline` must be positive and at most five
minutes, and `failure_policy` must be `degrade` or `fail_closed`.

The daemon rejects a partial section, an unknown model, an invalid endpoint or
egress policy, and an undefined credential binding. It reads the named secret
only when a reranking request reaches the provider, and the provider call needs
an active query-and-excerpt consent grant. The adapter policy fingerprint binds
the provider, model, candidate limit, excerpt limit, deadline, endpoint, and
transport policy to the reviewed plan.

Adding this section changes the plan fingerprint, and the plan reports that
consent is required until you approve reranking. Existing grants remain valid
for the operations they already cover, including searches without `--rerank`.
Granting the revised plan approves all configured profile operations together;
reranking has its own grant record, with no separate approval step. To review
and grant the revised plan, follow the
[search consent walkthrough](usage/search.md#consent-before-semantic-or-hybrid-search).

### Self-hosted Cap origins

Register a self-hosted Cap deployment under `[media_origins.<name>]`. The
endpoint is a single HTTP or HTTPS origin, and Cap share and embed paths are
recognized only on that origin. The deployment revision, credential binding,
network allowlist, TLS pins, and probe timeout are part of the daemon
configuration.

```toml
[credential_bindings.team-cap]
environment_variable = "CAP_API_KEY"

[media_origins.team-cap]
provider = "cap.self-hosted"
endpoint = "https://cap.example.test"
allowed_cidrs = ["127.0.0.0/8"]
proxy_mode = "disabled"
connect_timeout = "30s"
keep_alive = "30s"
tls_handshake_timeout = "10s"
spki_sha256 = []
credential_binding = "credential:team-cap"
deployment_revision = "v-synthetic"
probe_timeout = "30s"
```

HTTPS uses the system trust store, while a private deployment may use HTTP when
its configuration lists allowed CIDRs. The daemon resolves the named credential
only for the registered origin and runs a bounded probe at startup. Recognized
recordings remain `access_required`, and Docbank has no byte download path for
them.

Registration applies to new submissions. A generic source retained earlier
stays separate from a later registered-origin submission.

### Store bindings

`[store_bindings.<name>]` profiles configure machine-local filesystem or
S3-compatible secondary storage. Filesystem profiles use `kind = "filesystem"`
and an absolute `path`, while S3 profiles use `kind = "s3"`, `endpoint`,
`region`, `bucket`, optional `prefix`, `credential_profile`, and
`force_path_style`. `priority` controls read preference after current health,
with lower values preferred. For the full workflow and examples, see
[Multi-store storage](usage/storage.md).

The daemon loads bindings once at startup, and logical metadata, audit
evidence, and backups do not include them. After editing a profile, restart the
daemon. Docbank reports a typed stale-configuration error instead of
hot-reloading credentials or paths underneath active jobs.

S3 endpoints must use authenticated HTTPS, including loopback services. Docbank
rejects plain HTTP because a loopback port does not identify which process
receives an ownership marker or document bytes.

Active stores must use separate locations:

- A filesystem root must not overlap the vault, a watched inbox, or another
  filesystem store.
- S3 prefixes must not be equal or nested within the same canonical endpoint
  and bucket.

Watches check checkout separation when they start, and filesystem stores check
at daemon start and at registration. An overlapping store stays unattached at
startup until a background check confirms separation, and registration rejects
overlapping roots. Released checkout folders stay protected while they contain
files or while Docbank cannot inspect them. See
[Create writable copies](usage/photos.md#create-writable-copies).

Docbank also checks each store's ownership marker and epoch, a value that
identifies the current ownership claim, catching aliases that path comparisons
cannot recognize.

Docbank verifies secondary objects without encrypting them, so raw readers of a
filesystem root or S3 prefix can decode document content without the daemon API
key. Put store profiles only on owner-controlled storage, or on storage
protected by independently controlled filesystem, bucket, or KMS encryption and
access policy.

### Bind validation

The daemon validates its listening address at startup, and an invalid setting
makes `docbank daemon run` fail immediately:

- A **loopback** `bind_addr` (`127.0.0.1`, `::1`, `localhost`) accepts an empty
  `api_key`, and the daemon generates one at startup.
- A **non-loopback IP**, including `0.0.0.0` or `::`, requires an explicit
  `api_key` from TOML, `DOCBANK_API_KEY`, or `DOCBANK_API_KEY_FILE`.
  Authentication remains mandatory. Choosing a non-loopback address opts into
  sending the key and vault contents over the selected network in cleartext.
  Use a trusted network, an encrypted tunnel, or an HTTPS proxy; Docbank
  provides no TLS or remote server identity verification. See
  [Run in a container](usage/containers.md).
- `api_port` must be between `0` and `65535`. Keys must contain 1 to 4,096
  bytes with no whitespace or control bytes. Invalid `Host` allowlist entries
  fail startup.

Server-path ingest and preflight, backup restore, and explicit backup `repo`
paths remain restricted to a loopback `RemoteAddr`. Host allowlists and
forwarding headers do not change that peer check.

## Environment variables

| Variable | Effect |
|----------|--------|
| `DOCBANK_BIND_ADDR` | Override `[server] bind_addr`. |
| `DOCBANK_API_PORT` | Override `[server] api_port`; `0` selects an ephemeral port. |
| `DOCBANK_ALLOWED_HOSTS` | Comma-separated replacement for `[server] allowed_hosts`; spaces around entries are stripped. |
| `DOCBANK_API_KEY` | Override `[server] api_key` with an explicit key. |
| `DOCBANK_API_KEY_FILE` | Read the key from a mounted regular file instead of `DOCBANK_API_KEY`. |
| `DOCBANK_MCP_HTTP_ENABLED` | Override `[mcp.http] enabled` with `true` or `false`; empty preserves TOML. |
| `DOCBANK_MCP_HTTP_PORT` | Override `[mcp.http] port`; `0` serves `/mcp` on the API port. |
| `DOCBANK_MCP_HTTP_TOKEN` | Select an environment-backed MCP HTTP binding. Overrides the TOML binding; resolved only when MCP HTTP is enabled. |
| `DOCBANK_MCP_HTTP_TOKEN_FILE` | Supply that MCP bearer through a mounted regular file. |
| `DOCBANK_MCP_HTTP_ALLOWED_HOSTS` | Comma-separated replacement for `[mcp.http] allowed_hosts`. |
| `DOCBANK_MCP_HTTP_PUBLIC_ORIGIN` | Override `[mcp.http] public_origin`. |
| `DOCBANK_HOME` | Vault location; see [Vault location](#vault-location) above. |
| `DOCBANK_LOCK_DIR` | Absolute lock-registry directory for an isolated environment whose account home is unwritable. Defaults to the operating-system account home's `.local/state/docbank/target-locks`, independent of `HOME` and XDG settings. All processes sharing or restoring overlapping vault trees must use the same directory; stop them before changing this setting. Keep it outside vaults and restore targets. Never delete it while any participating process runs. |
| `DOCBANK_LOG_LEVEL` | Log level (`debug`, `info`, `warn`, `error`) for `docbank daemon run` and `docbank mcp`, foreground or background. Invalid values are ignored and fall back to `info`. |

Nonempty startup overrides take precedence over TOML, while empty variables
keep the TOML values. `DOCBANK_API_KEY` and `DOCBANK_API_KEY_FILE` cannot both
be nonempty. A selected file must be a readable, non-symlink regular file owned
by the process user and contain a valid nonempty key. On Unix, its mode must be
`0400` or `0600`. On Windows, its protected ACL must grant access only to the
current user or token owner and trusted system administrators; owner-only read
access is sufficient. Missing, unreadable, empty, or oversized files fail
startup instead of falling back. Docbank strips one trailing LF or CRLF and
bounds file keys before parsing. Secrets are resolved once per process; restart
the process to rotate them.

## Usage reporting

Docbank can send general, anonymous usage telemetry. Telemetry stays off unless
you opt in or set `TELEMETRY_ENABLED=1`.

Any of these settings turns it off, even after you opt in:

- `DOCBANK_TELEMETRY_ENABLED=0`
- `TELEMETRY_ENABLED=0`
- `DO_NOT_TRACK` set to any value except `0` or `false`
- `CI` set to any value

The daemon reads these variables at startup; run `docbank daemon restart` after
changing them. Development builds never send telemetry.

## Photo map tiles

Maps stay disabled until you configure a raster tile provider and restart the
daemon:

```toml
[web.map_tiles]
tile_url = "https://tiles.example.org/{z}/{x}/{y}.png"
attribution = "Example map provider"
max_zoom = 19
```

The URL must have a fixed HTTPS origin and include all three placeholders in
its path, with a hostname, IPv4 address, or bracketed IPv6 address as the host.
Attribution is plain text. Docbank connects directly to the configured host and
ignores proxy settings. Because the daemon fetches tiles for signed-in
browsers, the URL and any key in it stay on the server and the web page carries
only the provider's host name. The provider sees the daemon host's IP address
and the viewed map areas, with no `Referer` header.

Docbank refuses redirects, accepts only responses up to 2 MiB whose bytes
identify them as PNG, JPEG, GIF, or WebP, and times out after 10 seconds. The
daemon does not cache tiles. Tiles carry a one-hour private cache lifetime that
varies by browser session, except under key login, where responses are never
cached. On provider failures, Docbank logs only a status code or error class,
never the URL. Choose a provider that permits this setup; the
[public OpenStreetMap tile service requires a Referer](https://operations.osmfoundation.org/policies/tiles/)
and is incompatible with this policy.
