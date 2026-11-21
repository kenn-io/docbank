---
title: Daemon and process model
description: The single process that owns the vault, docbank daemon run, and how the CLI discovers, auto-starts, and stops it.
---

# Daemon and process model

The daemon owns a standalone vault and serves every CLI data command through
the [HTTP API](http-api.md): `document add`, `document list`, `document tree`,
`document cat`, `document move`, `document remove`, `document restore`,
`search`, `trash list`/`empty`, `vault gc`, and `vault verify`.

`docbank openapi` renders the API contract offline. It registers routes without
invoking them, so it needs neither a daemon nor a vault. If you are building an
application that owns an embedded vault, use the separate
[Go integration](../embedding.md).

## Why a daemon

Having one owner lets Docbank coordinate SQLite writes and content storage in a
single process. Both the CLI and agents talk to the same `/api/v1` contract.
Because CLI commands cannot open the store directly, every command exercises
the same API an agent uses.

[Ownership and concurrency](locking.md) covers the locking contract in detail.
Earlier development builds opened the store once per command, but that design
no longer applies to standalone operation.

## Lifecycle

`docbank daemon run` runs in the foreground. At startup, the daemon:

1. Resolves `$DOCBANK_HOME` and creates only that root directory.
2. Takes the vault lock **exclusively** for its entire run.
3. Initializes the remaining layout, then loads and validates `config.toml`.
4. Opens the store and removes stale files from `blobs/tmp/`. The exclusive
   lock keeps any other Docbank writer out during cleanup.
5. Binds the API listener and serves requests until `SIGINT`, `SIGTERM`, or a
   shutdown request arrives.

The other lifecycle commands control that process:

- `docbank daemon start` spawns the same binary as a detached background
  process running `daemon run`.
- `docbank daemon stop` asks the running daemon to shut down.
- `docbank daemon restart` stops the daemon, tolerating it not already running,
  and starts it again.
- `docbank daemon status` reports whether the daemon is running.

During graceful shutdown, the daemon:

1. Cancels background tasks.
2. Drains in-flight HTTP requests.
3. Waits for background tasks to return.
4. Closes the store.
5. Releases the vault lock.
6. Removes the runtime record used for discovery.

HTTP draining and background-task draining each have a ten-second ceiling.
If a daemon hosts MCP HTTP, it drains MCP first for at most half of the HTTP
budget so in-flight MCP reads can finish against its API. Any read still
running after that is canceled, leaving the rest of the budget for the API.
Clients allow 25 seconds for graceful exit before forcing termination, which
gives scheduling and cleanup extra time without cutting into either drain
budget.

During API shutdown, the daemon invalidates cached query snapshots, rejects new
snapshot builds, and signals active builders before waiting for cleanup. It
does this even when the shutdown context is already canceled or expired; only
the wait for builders is bounded by that context.

```bash
docbank daemon run          # foreground; logs to stderr
docbank daemon start        # background; logs to $DOCBANK_HOME/logs/
docbank daemon status       # pid, address, version, uptime
docbank daemon status --json
docbank daemon restart      # stop (if running) then start
docbank daemon stop
```

## Discovery

A running daemon writes a runtime record at `$DOCBANK_HOME/daemon.<pid>.json`.
That record names the daemon's service (`docbank`), its build version, and the
loopback endpoint for local CLI and MCP discovery. A wildcard listener uses
loopback on the same port. A concrete non-loopback bind adds a separate
ephemeral loopback listener so local server-path imports remain available, and
its `network_address` metadata carries the concrete network listener's actual
endpoint, including the assigned port when `api_port` is `0`. Remote clients
that cannot read the record need a fixed port. The record's metadata also
holds a random shutdown token generated at startup and the daemon's effective
API key, which is either the configured `[server] api_key` or a freshly
generated key when unset.

Because the record lives inside the 0700 `$DOCBANK_HOME`, only the vault's
owner can read it. That makes publishing the key there safe, and loopback
callers do not need to know the key in advance. Same-user CLI commands pick up
the key from the record automatically. There is no keyless request path, even
when `api_key` is left unset.

To find a live daemon, discovery lists runtime records, drops any whose PID is
no longer alive, and probes `/api/ping` on the survivors. Two guards keep this
safe against stale state:

- **PID-reuse guard.** Before a dead daemon's runtime record is cleaned up, an
  unrelated process can reuse its PID. Every record carries the Kit process
  identity. On Linux, this combines the boot ID, PID namespace, and process
  start ticks, so wall-clock changes and rounded uptime estimates cannot change
  a live daemon's identity. Discovery compares that identity against the live
  process at the recorded PID and treats any mismatch as a stale record, never
  signaling or trusting a process it did not start. Because a sandbox can share
  `$DOCBANK_HOME` while seeing different PIDs, a starter that finds no daemon
  checks the vault lock first and refuses to proceed while another process
  holds it. A dead daemon holds no lock, so its leftover record never blocks a
  start.
- **Exact version match.** Pre-1.0, there is no compatibility matrix, so local
  daemon discovery requires an exact version match with the CLI. `docbank
  daemon status` and `docbank daemon stop` only discover and never start a
  daemon, so they report any live daemon regardless of version. Everything
  that starts a daemon goes through one path, `daemonconn.EnsureDaemon`:
  `daemon start`, `daemon restart`, and auto-start from data commands. That
  path requires the exact match. On a mismatch, it stops the old daemon and
  starts a fresh one under the launch lock, so no start path leaves a
  stale-version daemon running. The exclusive vault lock guarantees at most
  one daemon per vault, and this single start path guarantees that the daemon
  is current after any successful start.

A launch lock serializes racing starters. It lives outside `$DOCBANK_HOME`,
under the canonical per-user target-lock registry. If two CLI invocations both
find no daemon and try to start one, the second waits for the lock and then
re-checks discovery instead of spawning a redundant daemon. Launch
coordination stays outside `$DOCBANK_HOME` so discovery and launch do not
create the target, its database, logs, or runtime records before the child
daemon acquires the vault-tree lock. `daemon restart` reuses the same lock for
the start half of a restart. During startup, Docbank captures bootstrap stderr
in a private transient file beside that external lock, includes it in any
startup failure, and removes it when the start attempt finishes.

The listener closes before background tasks finish draining. During that
interval, the process and runtime record remain live and the daemon still owns
the vault lock, but ping cannot identify it.

A public ping response also cannot prove that a listener on the recorded port
belongs to the recorded PID, so discovery checks the connection before sending
credentials:

1. Send a fresh random challenge after ping.
2. Verify the reply's HMAC using the per-run shutdown secret from the private
   runtime record. Neither that secret nor the API key crosses the socket
   during this proof.
3. Keep credential-bearing requests on that proven TCP connection. Requests
   fail instead of redirecting or reconnecting.

Acquisition finishes this proof on the socket within a two-second budget before
handing it to the authenticated client. After handoff, any connection loss
fails the request.

The starter never sends secrets to a forged or pingless endpoint. It requests
graceful process termination only after verifying the PID's process identity,
waits for exit, and then starts a replacement. A runtime record without
matching identity proof cannot authorize this path. The same flow handles
shutdown and an uncoordinated slow start without launching into an owned vault
or trusting a new listener that reused the port.

## Auto-start and idle shutdown

Data commands for the local vault call `daemonconn.Ensure`, which discovers a
version- and protocol-matched daemon or starts one for you. You never get a
"no daemon running" error for `document add`, `document list`, `document cat`,
and the rest. The protocol revision in the runtime record distinguishes
incompatible development builds that share the same version string. A missing
or mismatched revision forces a replacement before a CLI data request or MCP
tool request is sent. `daemon status` and `daemon stop` are discovery-only and
never start a daemon, so checking on or stopping the daemon cannot accidentally
spawn one.

[`docbank push`](../usage/pushing.md) works differently: it connects to its
explicit `--to` origin with an operator-supplied API key and does not discover,
start, or replace a local daemon. Every new connection must first answer a key
challenge, proving it holds the API key without either side sending it. Only
then does the key or any document byte cross that connection. The target must
support the push routes, or an older daemon returns an error before any upload.

An automatically started daemon exits after `[server] idle_timeout` (default 30
minutes) with no requests, so spawned daemons do not accumulate across
sessions. Set `idle_timeout = "0"` to disable idle shutdown. A foreground
`docbank daemon run` and any daemon hosting MCP HTTP never idle out; they run
until signaled or stopped. Daemons started with `daemon start` or `daemon
restart`, along with their upgrade replacements, behave the same way. `daemon
start` leaves a compatible automatic daemon unchanged, and `daemon restart`
keeps it running. An open photo event stream counts as an in-flight request, so
a visible Photos tab keeps a background daemon from reaching its idle timeout.

## Background tasks

Every daemon-owned task runs under one supervisor rooted in the daemon's
shutdown context, with unique names that stay stable for the lifetime of that
daemon. If a task panics, the supervisor recovers it and records a failure so
it does not crash the process. `docbank jobs` and authenticated
`GET /api/v1/jobs` expose worker activity and terminal state in deterministic
order. Workers waiting for work show as idle, and worker records remain until
restart. The list includes the newest 1,000 durable storage records. Finished
storage operations clear within an hour after their retention ends unless they
still own pending cleanup.

Source-metadata, email, visual-preview, photo-quality, document-events,
document-people, media-continuations, and local-text backfills stop scanning
after an empty full scan, then rescan after any committed vault write or a
daemon restart. A write wakes a settled backfill within its existing idle wait,
at most ten seconds. A failing target keeps its lane polling at the retry
cadence, and local text also polls while waiting for index publication to
finish.

The daemon's content jobs include:

- `extract:plain-text` verifies and indexes supported current text content with
  a bounded worker.
- `extract:source-metadata` reads retained originals that the current extractor
  has not processed, publishes typed metadata, and keeps watching for new
  content.
- `extract:photo-sidecars` syncs Pending photo files with their linked XMP
  sidecars, including hidden photos.
- `derive:photo-quality` evaluates the selected display file's quality signals.
- `maintenance:auxiliary-checksums` computes MD5 for retained blobs that
  predate auxiliary checksums, then completes. New writes record MD5 at ingest.
  On an existing vault, the first daemon start after this upgrade therefore
  reads every stored blob once.

`process:renditions` runs only when a rendition provider is bound to the daemon.
Its presence in `docbank jobs` means the daemon can process renditions.
Configured watched inboxes add one `watch:<name>` runner each.

All derived-data writes take the ordinary mutation side of the daemon operation
gate, one target at a time. This prevents GC from retiring a blob while a
worker reads it and publishes a result.

`docbank watch list` and authenticated `GET /api/v1/watches` join those runner
records to the daemon's effective watched-inbox configuration. That gives
people and agents the source, destination, settling policy, and exclusions they
need to diagnose an archive without turning the API into a second configuration
authority.

As soon as shutdown begins, the supervisor stops accepting work, cancels every
runner, and waits before SQLite and blob storage close. Runners must honor
their context, and the wait is bounded so a defective runner cannot prevent
daemon exit forever. The background daemon's idle-timeout loop uses this same
supervisor, along with text extraction, watched inboxes, and configured
automatic packing. Scheduled packing appears as the `storage:pack` job, waits
the configured interval after each completed run, and uses the same maintenance
gate as a requested pack without creating an unmanaged goroutine. An enabled
backup schedule from `config.toml` or a runtime override starts the
`backup:scheduled` job under the same supervisor. It waits the interval counted
from the last capture, stops when shutdown begins, and keeps the daemon awake
only while the schedule can run or a settings read is failing. Garbage
collection and repacking run only when an operator requests them.

## Logs

A background daemon creates its logs only after acquiring vault ownership. It
then writes structured JSON to `$DOCBANK_HOME/logs/`, one file per day
(`docbank-YYYY-MM-DD.log`). Files rotate at 50 MiB, keeping the 5 most recent
rotated files. A foreground `docbank daemon run` logs to stderr instead.
`DOCBANK_LOG_LEVEL` controls the level for both; see the
[CLI reference](../cli-reference.md).
