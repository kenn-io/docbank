# docbank — Phase 2 Infrastructure: Daemon, HTTP API, Self-Update

Date: 2026-07-08
Status: Approved design, pre-implementation

## Purpose

Align docbank with the daemon-centric architecture shared by msgvault
and kata: one long-lived daemon owns the vault (SQLite + blobs) and
exposes a huma/OpenAPI HTTP surface; every other consumer — the CLI,
agents, and eventually a web frontend — is an HTTP client. This spec
covers the infrastructure skeleton only:

- `docbank serve` daemon on `go.kenn.io/kit` lifecycle primitives
- Huma v2 HTTP API implementing the subset of the documented contract
  ([http-api.md](../../architecture/http-api.md)) that the Phase 1 CLI
  needs
- Daemon-first CLI: existing commands become HTTP clients with
  discover-or-autostart
- `docbank update` self-update via `kit/selfupdate`, plus the release
  pipeline that produces its artifacts
- Web frontend placeholder: embedded static page and the config/route
  ergonomics the real frontend will inherit
- Zensical documentation for all of the above

## Non-goals (this project)

Phase 2 *features* stay out and get their own spec → plan cycles on
top of this skeleton:

- Versioned editing (`PUT /nodes/{id}/content`, `edit`/`put`/`revert`/
  `versions`) — designed in
  [editing-and-versions.md](../../architecture/editing-and-versions.md)
- Tags, `POST /batch/move`, multipart upload
- Watched inbox directories, text extraction workers
- TUI (Phase 3), backup commands (Phase 4)
- A real web frontend (framework choice deliberately unmade)
- Remote-daemon mode (`[remote]` config à la msgvault) — config stays
  shaped so it can be added without breakage
- MCP server (post-v1; the OpenAPI contract makes wrapping mechanical)

`~/.docbank` + `DOCBANK_HOME` already exist (Phase 1) and are unchanged.

## Approach

Three approaches were considered:

1. **Full msgvault port** — including its `/api/v1/cli/*` subprocess
   proxying, dual write models (direct flock writes vs daemon), and
   generated OpenAPI client. Rejected: half that machinery exists
   because msgvault has ~35 legacy commands that open SQLite directly.
   docbank has no such legacy.
2. **Lean daemon-first on kit primitives** (chosen) — kata's shape:
   kit supplies all lifecycle machinery; docbank supplies only its API
   surface and commands. Resource-shaped routes only; the CLI is the
   first consumer of the same API agents use, which keeps the
   contract's design test honest ("an agent can do everything through
   the API alone").
3. **2 + generated OpenAPI client** — rejected for now: for ~15
   endpoints consumed by our own CLI, sharing request/response types
   in-module gives compile-time contract fidelity with zero codegen
   toolchain. A published generated client can come post-v1; agents
   consume the OpenAPI doc either way.

One deviation from msgvault to note: **no `/cli/*` route mirror**. If
the CLI needs something the resource API can't express, that is a gap
in the agent surface and gets fixed there (the one exception is
`POST /api/v1/ingest`, a documented contract addendum — see below).

## Package layout

```
internal/version    Version/Commit vars (ldflags target moves here from
                    cmd/docbank/cmd) — shared by CLI, runtime record,
                    OpenAPI config, update
internal/config     config.toml loading: [server] + [web] sections,
                    defaults, validation (bind/key security check)
internal/api        huma v2 + humago server: routes, middleware, auth,
                    maintenance gate, embedded web placeholder
internal/client     typed HTTP client + daemon ensure/discovery;
                    shares request/response types with internal/api
internal/update     kit/selfupdate wrapper + daemon stop/replace/
                    restart coordination
cmd/docbank/cmd     serve.go (foreground + start/stop/status),
                    update.go, openapi.go; existing commands rewritten
                    as client calls
```

`internal/store`, `internal/blob`, `internal/ingest`, and
`internal/home` are conceptually unchanged but become daemon-side-only:
after this project, no CLI command opens SQLite or the blob store
directly.

## Daemon lifecycle

All lifecycle machinery comes from `kit/daemon`; docbank writes none of
it.

**Foreground `docbank serve`:** load config → `home.Ensure` → acquire
the vault flock **exclusively** → open store → clean stale blob tmp
files (the Phase 1 `cleanTmpIfSole` logic moves here — the exclusive
lock makes "sole" trivially true) → `kit/daemon.Listen` on
`[server] bind_addr:api_port` (default `127.0.0.1:0`, ephemeral) →
write a `kit/daemon.RuntimeRecord` into `$DOCBANK_HOME` carrying
service name (`docbank`), build version, and the *actual bound* port,
with a random shutdown token and the process create-time in the
record's `Metadata` → serve until signal or shutdown request.

Create-time is not a kit field: kit's record carries PID/endpoint/
service/version/StartedAt plus a free-form `Metadata` map, and kit's
discovery checks only `ProcessAlive` and ping-PID equality. docbank
follows msgvault's pattern exactly — write
`Metadata["create_time"]` (epoch millis via
`gopsutil/v4/process.CreateTime`) at startup, and treat a record whose
recorded create-time mismatches the live PID's as dead during
discovery and before any signal fallback. The dependency is justified:
it is the only portable guard against SIGTERM-ing an unrelated process
that reused the recorded PID. Hoisting this into `kit/daemon` is a
candidate follow-up, not part of this project.

The exclusive lock replaces Phase 1's shared/exclusive split: with all
access funneled through one process, the daemon is the single lock
holder, and `gc` no longer needs its own exclusive flock (see
maintenance gate). Two daemons on one vault are impossible by
construction. The Unix-only lock policy is unchanged; the vault remains
Unix-only.

**Discovery and auto-start (CLI side):** `kit/daemon.Manager.Ensure` —
list runtime records, drop dead PIDs (with the docbank-side
create-time check above guarding PID reuse), probe `/api/ping`
(`ExpectedService: "docbank"`), require **exact version match**
(pre-1.0: no compatibility matrix, no auto-restart policy knob). On version mismatch the CLI gracefully stops
the old daemon (shutdown endpoint with token, SIGTERM fallback) and
starts fresh. Auto-start is `kit/daemon.StartDetached` re-exec of
`os.Executable()` with `serve` and `DOCBANK_BACKGROUND_DAEMON=1`; a
launch flock serializes racing starters.

**Idle shutdown:** background-spawned daemons exit after
`[server] idle_timeout` (default 30m, `0` = never) without requests,
tracked by middleware. Foreground `serve` never idles out.

**Stop:** `docbank serve stop` sends `POST /api/daemon/shutdown` with
the shutdown token from the runtime record (hidden route, loopback
semantics); falls back to SIGTERM at the recorded PID only when the
create-time check confirms the PID still belongs to the recorded
daemon. Graceful shutdown finishes in-flight requests, closes the
store, releases the lock, removes the runtime record.

**Status:** `docbank serve status` reports pid, address, version,
uptime; `--json` for agents. `serve status` and `serve stop` are
**discovery-only** (`Manager.Find`, never `Ensure`): reporting on or
stopping a daemon must not spawn one.

**Logs:** background daemons log JSON to `$DOCBANK_HOME/logs/` via
`kit/logging` (daily files, size rotation, retained count); foreground
logs to stderr.

## HTTP API surface (this project)

Huma v2 with the `humago` (stdlib `net/http`) adapter. Routes under a
`huma.NewGroup(api, "/api/v1")` carrying auth middleware. Huma's
`DefaultConfig` serves `/openapi.json`, `/openapi.yaml`, and `/docs`
at the root for free. Root-level extras: `GET /health`,
`GET /api/ping` (kit's ping handler for discovery), hidden
`POST /api/daemon/shutdown`, and `/` (web placeholder).

Endpoints, per the http-api.md contract — IDs canonical in every
response:

| Route | Backs |
|-------|-------|
| `GET /nodes/{id}`, `GET /path?path=/a/b` | stat |
| `GET /nodes/{id}/children` (limit/offset pagination) | `ls`, `tree` (client-side walk) |
| `GET /nodes/{id}/content` (streamed) | `cat` |
| `GET /search?q=&limit=` | `search` |
| `POST /nodes` (directories only for now) | agent reorganization (no CLI `mkdir` exists; agents need it to file into new directories) |
| `POST /ingest` `{paths, dest}` | `add` |
| `PATCH /nodes/{id}` `{new_parent_id?, new_name?}` | `mv` |
| `POST /nodes/{id}/trash`, `POST /nodes/{id}/restore` | `rm`, `restore` |
| `GET /trash`, `POST /trash/empty` `{older_than?}` | `trash`, `trash empty` |
| `POST /gc` `{run}` , `POST /verify` | `gc`, `verify` (timeout-exempt) |

**Path resolution:** `GET /path` takes the virtual path as a **query
parameter** (`?path=/inbox/doc.pdf`), not a catch-all URL segment —
stdlib-mux wildcard decoding of percent-encoded slashes makes a
`/path/{path...}` route ambiguous for names containing `/`-adjacent
encodings, and a query parameter has one well-defined encoding. The
path must be absolute (leading `/`); `?path=/` stats the root. Query
encoding is ordinary URL encoding; the server applies the store's
existing NFC normalization and name validation and returns 422 for
invalid paths. http-api.md's route table is updated to this form.

**`If-Match` semantics per endpoint:** the contract's rule —
mutations require `If-Match: <revision>` — applies where a mutation
targets one existing node; bulk and maintenance operations are
explicit exceptions:

| Endpoint | Precondition |
|----------|--------------|
| `PATCH /nodes/{id}` | required — target node's revision |
| `POST /nodes/{id}/trash` | required — target node's revision |
| `POST /nodes/{id}/restore` | required — target node's revision |
| `POST /nodes` (create dir) | none — creation has no prior revision; name collision is 409 |
| `POST /ingest` | none — long-running bulk operation with per-path partial success; the destination directory may legitimately change while it runs. Collisions resolve by Phase 1's suffixing rules |
| `POST /trash/empty`, `POST /gc`, `POST /verify` | none — vault-wide maintenance, serialized by the maintenance gate |

Stale revision → 412; missing `If-Match` where required → 428
Precondition Required. Both carry problem-JSON explaining the rule.

**Contract addendum — `POST /ingest`:** takes server-side local paths
and a destination, returns the ingest report (imported / skipped /
failed, per-path errors). For a local daemon this preserves Phase 1's
provenance recording, resumability, and partial-failure reporting, and
avoids re-streaming whole trees over loopback. Because it grants
"read any daemon-readable local path" capability, it is **restricted
to loopback connections** (checked per-request against `RemoteAddr`)
regardless of bind address or API key — a private-LAN client gets 403
with problem-JSON pointing at the planned multipart upload, which is
the correct remote ingest path. Multipart upload stays planned.
http-api.md gets updated accordingly.

**Error mapping:** the store's typed errors map to statuses exactly as
the contract table specifies (`ErrNotFound` 404, `ErrExists`/`ErrCycle`
409, `ErrNotDir`/`ErrNotFile`/`ErrInvalidName` 422, stale `If-Match`
412) via huma's RFC 7807 problem responses, with a machine-readable
error code the client maps back to the typed error.

**Auth:** `X-Api-Key` or `Authorization: Bearer`, constant-time
compared against `[server] api_key`. **Keyless = local-allow**
(msgvault's model): an empty key is valid only for loopback binds;
startup refuses a non-loopback bind without a key, and non-public
binds are enforced with kit's `RequireNonPublic` endpoint policy.
`/health`, `/api/ping`, `/docs`, OpenAPI documents, and the web
placeholder are auth-exempt.

**Maintenance gate:** a `sync.RWMutex`-shaped gate. Regular mutating
handlers take the read side; `gc --run`, `trash empty`, and `verify`
take the write side. Requests queue rather than fail; maintenance
routes are exempt from the request timeout. This replaces Phase 1's
exclusive-flock model for `gc`.

**Middleware stack** (outer → inner): request ID → request logger →
panic recovery → idle tracker → per-path timeout (maintenance-exempt)
→ auth (on the v1 group).

## CLI changes

Command UX — names, flags, output — is unchanged. Internals swap
`openVault` for "ensure daemon, call typed client". The client maps
problem-JSON error codes back to the store's typed errors so command
error messages stay consistent with Phase 1.

New commands:

- `docbank serve` (foreground), `serve start|stop|status`
  (`status --json`)
- `docbank update [--check] [--yes] [--force]`
- `docbank openapi` — print the OpenAPI document offline (no daemon,
  no DB; same route registration bound to nothing), for agents and
  client generation

Removed: direct-store plumbing in `cmd/docbank/cmd/vault.go`
(`openVault`, `openVaultExclusive`, `cleanTmpIfSole` — the latter moves
into daemon startup).

## Web frontend placeholder

One handwritten `index.html`, `go:embed`-ded into `internal/api` and
served at `/` when `[web] enabled` (default true). It names the vault
and links to `/docs` (the huma-generated API reference is the real
browser surface today). No JS toolchain.

Decisions fixed now that the real frontend inherits: UI at `/`, API
under `/api/v1`, docs at `/docs`; `[web]` config section; placeholder
is auth-exempt (static, no data). Explicitly deferred: the real
frontend's auth story (API-key entry vs session) and its build
pipeline.

## Configuration

`$DOCBANK_HOME/config.toml`, optional — every value has a default and
the file may be absent. TOML via `BurntSushi/toml` (msgvault
precedent). No per-field env or flag overrides; the only env knob
remains `DOCBANK_HOME` (plus internal background-daemon markers).

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

Validation at daemon startup: non-loopback `bind_addr` without
`api_key` is a fatal misconfiguration; public addresses are rejected
outright (`RequireNonPublic`).

## Self-update and releases

`internal/update` wraps `kit/selfupdate.Client{Owner: "kenn-io",
Repo: "docbank", BinaryName: "docbank", CurrentVersion:
version.Version, CacheDir: $DOCBANK_HOME/cache/update,
AllowUnsignedChecksums: true}`. Checksum **presence** is mandatory
(msgvault's rule: refuse to install when the release has no SHA256);
dev builds are not replaced without `--force`.

`docbank update` flow: check (re-check with `Force` when the cached
info needs refetch) → show current → latest, size, SHA256 → confirm
unless `--yes` → **stop the running daemon → replace the binary →
restart the daemon with the new executable path**, with
rollback-restart on failure. `--check` stops after printing.

**Release pipeline** (`.github/workflows/release.yml`), modeled on
msgvault's because docbank needs CGO (mattn/go-sqlite3 + `fts5` tag —
kata's goreleaser/CGO_ENABLED=0 path does not apply): tag-driven,
native runner matrix — Linux amd64/arm64, macOS amd64/arm64 — each
building `CGO_ENABLED=1 go build -tags fts5 -trimpath -ldflags "<version stamp>"`,
packaging `docbank_<version>_<os>_<arch>.tar.gz`, and a publish job
assembling `SHA256SUMS` and creating the GitHub release. Asset naming
matches `kit/selfupdate.DefaultAssetName`. Actions SHA-pinned with
version comments, `persist-credentials: false`, zizmor-clean. No
Windows artifacts: the vault is Unix-only.

## Testing

- **API:** `httptest` against the real store (existing design
  commitment — no store mocking). Table-driven per endpoint: happy
  path, `If-Match` 412 on stale revision, 428 where required and
  absent, no-precondition endpoints accepting requests without one,
  error mapping (404/409/412/422), pagination bounds, auth on/off,
  the startup refusal of non-loopback-without-key, path-query
  resolution (root, invalid names, encoding), and `POST /ingest`
  rejecting non-loopback callers with 403.
- **Client:** round-trips against an `httptest` server; problem-JSON →
  typed store error mapping verified in both directions.
- **CLI e2e:** existing cmd tests keep passing by running a real
  in-process daemon on an ephemeral port that writes a runtime record
  into the test's temp `DOCBANK_HOME`; CLI discovery finds it
  naturally. No test-only hooks or transport injection.
- **Lifecycle integration (Unix):** real `StartDetached` spawn → probe
  → version-mismatch restart → graceful stop, in a temp home; plus
  `serve status`/`serve stop` against an empty home proving neither
  spawns a daemon, and a stale record with a reused-PID create-time
  mismatch being treated as dead.
- **Update:** `kit/selfupdate` accepts base-URL overrides — fake
  release server in `httptest` serving a real archive + SHA256SUMS,
  install into a temp dir; daemon stop/restart coordination tested
  against a fake lifecycle.
- **Maintenance gate:** concurrent mutation vs `gc --run` ordering
  test.

Every regression fix follows the Phase 1 discipline: prove the test
fails against the unfixed code before trusting it.

## Documentation (zensical)

- New `docs/architecture/daemon.md`: lifecycle, discovery/auto-start,
  runtime records, idle shutdown, the single-lock-holder model (updates
  [locking.md](../../architecture/locking.md) cross-references)
- `docs/configuration.md`: rewritten for config.toml (the "Planned"
  admonition becomes real documentation)
- `docs/architecture/http-api.md`: implemented subset marked as such,
  `POST /ingest` addendum documented, remainder stays "Planned"
- `docs/cli-reference.md`: `serve`, `update`, `openapi`
- `docs/quickstart.md`, `docs/roadmap.md`, `docs/changelog.md` updated
  (roadmap splits Phase 2 into infrastructure — implemented — and
  features — designed)

## Risks and accepted trade-offs

- **Per-command latency:** every CLI invocation pays a loopback HTTP
  round-trip and, on first use, a daemon spawn. Accepted: loopback is
  ~ms, and the spawn cost amortizes across the idle window.
- **Daemon mandatory for CLI:** there is no direct-store fallback. A
  wedged daemon means a wedged CLI (mitigated by exact-version restart,
  the launch lock, and `serve stop`'s SIGTERM fallback). Accepted in
  exchange for a single write path and no dual-model locking.
- **`POST /ingest` takes server-side paths:** an intentional local-
  daemon affordance, restricted to loopback callers so an API key
  never grants remote read of arbitrary daemon-readable files; a
  remote deployment needs multipart upload first. Documented in the
  contract.
- **No API compatibility window pre-1.0:** exact version match between
  CLI and daemon, restart on mismatch. Revisit at 1.0.
