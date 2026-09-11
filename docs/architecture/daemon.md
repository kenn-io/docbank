---
title: Daemon & Process Model
description: docbank daemon run — the single process that owns the vault, and how the CLI discovers, auto-starts, and stops it.
---

# Daemon and process model

The daemon owns a standalone vault and serves every CLI data command through
the [HTTP API](http-api.md). This includes `add`, `ls`, `tree`, `cat`, `mv`,
`rm`, `restore`, `search`, `trash list`/`empty`, `gc`, and `verify`.

`docbank openapi` renders the API contract offline. It registers routes without
invoking them, so it needs neither a daemon nor a vault. Applications that own
an embedded vault use the separate [Go integration](../embedding.md).

## Why a daemon

One owner coordinates SQLite writes and content storage. The CLI and agents
use the same `/api/v1` contract. Because CLI commands cannot open the store
directly, each command also exercises the API an agent would use.

[Ownership & Concurrency](locking.md) owns the locking contract. Earlier
development builds opened the store once per command; that historical design
no longer describes standalone operation.

## Lifecycle

`docbank daemon run` runs in the foreground. At startup, the daemon:

1. Resolves `$DOCBANK_HOME` and creates only that root directory.
2. Takes the vault lock **exclusively** for its entire run.
3. Initializes the remaining layout, then loads and validates `config.toml`.
4. Opens the store and removes stale files from `blobs/tmp/`. The exclusive
   lock excludes another Docbank writer during cleanup.
5. Binds the API listener and serves requests until `SIGINT`, `SIGTERM`, or a
   shutdown request arrives.

`docbank daemon start` spawns the same binary as a detached background
process running `daemon run`; `docbank daemon stop` asks it to shut down;
`docbank daemon restart` stops it (tolerating it not already running) and
starts it again; `docbank daemon status` reports whether it's running.
During graceful shutdown, the daemon:

1. Cancels background tasks.
2. Drains in-flight HTTP requests.
3. Waits for background tasks to return.
4. Closes the store.
5. Releases the vault lock.
6. Removes the runtime record used for discovery.

HTTP draining and background-task draining each have a ten-second ceiling.
Clients allow 25 seconds for graceful exit before forced termination. The extra
time covers scheduling and cleanup without consuming either drain budget.

```bash
docbank daemon run          # foreground; logs to stderr
docbank daemon start        # background; logs to $DOCBANK_HOME/logs/
docbank daemon status       # pid, address, version, uptime
docbank daemon status --json
docbank daemon restart      # stop (if running) then start
docbank daemon stop
```

## Discovery

A running daemon writes a runtime record — `$DOCBANK_HOME/daemon.<pid>.json`
— naming its service (`docbank`), build version, and the actual bound
address (the configured `api_port` may be `0`, in which case the OS
picks an ephemeral port and the record carries the real one). The record
also carries, in its metadata, the process's create-time, a random
shutdown token generated at startup, and the daemon's effective API key
(the configured `[server] api_key`, or a freshly generated one when it's
unset). The record lives inside the 0700 `$DOCBANK_HOME`, so publishing
the key there — rather than requiring every loopback caller to already
know it — is safe: only the vault's owner can read it. Same-user CLI
commands pick the key up from the record automatically; there is no
keyless request path even when `api_key` is left unset.

Discovery lists runtime records, drops any whose PID isn't alive, and
probes `/api/ping` on the survivors. Two guards keep this safe against
stale state:

- **PID-reuse guard.** A dead daemon's PID can be reused by an unrelated
  process before its runtime record is cleaned up. Every record also
  carries the process's create-time; discovery compares it against the
  live process at that PID and treats a mismatch as "this record is
  stale," never signaling or trusting a process it didn't start.
- **Exact version match.** Pre-1.0, there is no compatibility matrix:
  the CLI requires the daemon's version to match its own exactly. `docbank
  daemon status` and `docbank daemon stop` report *any* live daemon
  regardless of version (they only discover, never start). Everything
  that starts a daemon — `daemon start`, `daemon restart`, and the data
  commands' auto-start — goes through one path (`client.EnsureDaemon`)
  that requires the exact match and, on a mismatch, stops the old daemon
  and starts a fresh one under the launch lock. There is deliberately no
  way to start a daemon that leaves a stale-version daemon running: the
  exclusive vault lock already guarantees at most one daemon per vault,
  and the single convergence path guarantees that the one daemon is
  current after any successful start.

An external launch lock under the canonical per-user target-lock registry
serializes racing starters: two CLI invocations that both find no daemon and
both try to start one serialize there, and the second re-checks discovery after
acquiring it instead of spawning a redundant daemon. Keeping launch
coordination outside `$DOCBANK_HOME` is essential: discovery and launch do not
create the target, its database, logs, or runtime records before the child
daemon acquires the vault-tree lock. `daemon restart` reuses the same lock for
the start half of the restart. Bootstrap stderr is captured in a private
transient file beside that external lock, included in a startup failure, and
removed when the start attempt finishes.

The listener closes before background tasks finish draining. During that
interval, the process and runtime record remain live and the daemon still owns
the vault lock, but ping cannot identify it.

A public ping response also cannot prove that a listener on the recorded port
belongs to the recorded PID. Discovery therefore checks the connection:

1. Send a fresh random challenge after ping.
2. Verify the reply's HMAC using the per-run shutdown secret from the private
   runtime record. Neither that secret nor the API key crosses the socket
   during this proof.
3. Keep credential-bearing requests on that proven TCP connection. Requests
   fail instead of redirecting or reconnecting.

The starter sends no secrets to a forged or pingless endpoint. It requests
graceful process termination only after verifying the PID's create-time, waits
for exit, then starts a replacement. A runtime record without create-time proof
cannot authorize this path. The same process handles shutdown and an
uncoordinated slow start without launching into an owned vault or trusting a
new listener that reused the port.

## Auto-start and idle shutdown

Every data command calls `client.Ensure`, which discovers a version- and
protocol-matched daemon or starts one — the CLI never fails with "no daemon
running" for `add`, `ls`, `cat`, and the rest. The protocol revision in the
runtime record distinguishes incompatible development builds that share the
same version string; a missing or mismatched revision forces replacement
before a CLI data request is sent. `daemon status` and `daemon stop` are
discovery-only and never start a daemon, so checking on or stopping the daemon
can't accidentally spawn one.

A background-spawned daemon (started via auto-start, `daemon start`, or
`daemon restart`) exits after `[server] idle_timeout` (default 30
minutes) with no requests, so spawned daemons don't accumulate across
sessions. `idle_timeout = "0"` disables idle shutdown. A foreground
`docbank daemon run` never idles out — it runs until signaled or
stopped.

## Background tasks

Every daemon-owned task runs under one supervisor rooted in the daemon's
shutdown context. Names are unique and stable for the lifetime of that daemon;
a task panic is recovered and recorded as a failure rather than crashing the
process. `docbank jobs` and authenticated `GET /api/v1/jobs` expose running and
terminal state in deterministic order. Terminal records remain until restart,
which makes a failed task visible instead of silently disappearing.

Every daemon runs three jobs that derive information from retained content:

- `extract:plain-text` verifies and indexes supported current text content with
  a bounded worker.
- `extract:source-metadata` reads retained originals that the current extractor
  has not processed. It publishes typed metadata and keeps watching for new
  content.
- `maintenance:auxiliary-checksums` computes MD5 for retained blobs that
  predate auxiliary checksums, then completes. New writes record MD5 at ingest.
  On an existing vault, the first daemon start after this upgrade therefore
  reads every stored blob once.

`process:renditions` runs only when a rendition provider is bound to the daemon.
Its presence in `docbank jobs` means the daemon can process renditions.
Configured watched inboxes add one `watch:<name>` runner each.

All derived-data writes take the ordinary mutation side of the daemon operation
gate, one target at a time. This prevents GC from retiring a blob while a worker
reads it and publishes a result.

`docbank watch list` and authenticated `GET /api/v1/watches` join those runner
records to the daemon's effective watched-inbox configuration. This gives
people and agents the source, destination, settling policy, and exclusions they
need to diagnose an archive without turning the API into a second configuration
authority.

The supervisor stops accepting work as soon as shutdown begins, cancels every
runner, and waits before SQLite and blob storage close. Runners must honor
their context; the wait is bounded so a defective runner cannot prevent daemon
exit forever. The background daemon's idle-timeout loop is supervised through
this same path. Text extraction, watched inboxes, and configured automatic
packing use this lifecycle. Scheduled packing appears as the `storage:pack`
job, waits the configured interval after each completed run, and uses the same
maintenance gate as explicit packing rather than creating an unmanaged
goroutine. Garbage collection and repacking remain explicit operator actions.

## Logs

A background daemon creates its logs only after acquiring vault ownership, then
logs structured JSON to `$DOCBANK_HOME/logs/`, one
file per day (`docbank-YYYY-MM-DD.log`), rotated at 50 MiB with the 5
most recent rotated files retained. A foreground `docbank daemon run`
logs to stderr instead. `DOCBANK_LOG_LEVEL` controls the level for both
(see [CLI Reference](../cli-reference.md)).
