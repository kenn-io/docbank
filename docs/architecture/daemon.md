---
title: Daemon
description: docbank daemon run — the single process that owns the vault, and how the CLI discovers, auto-starts, and stops it.
---

# Daemon

`docbank daemon run` is the one process that opens the vault. Every data
command — `add`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`, `search`,
`trash list`/`empty`, `gc`, `verify` — is an HTTP client of it, over the
[HTTP API](http-api.md). `docbank openapi` is the one exception:
it renders the API contract offline, with routes registered but never
invoked, so it needs neither a daemon nor a vault.

## Why a daemon

One process owns SQLite and the blob store; the CLI and agents are HTTP clients
of the same `/api/v1` surface. There
is no separate code path that opens the store directly — the CLI's own
commands are the design test that the agent-facing API is sufficient,
because the CLI has no other way to reach the vault.

Earlier development builds let every command open the store and coordinate
through the vault lock directly. The current single-lock-holder model is
described in [Concurrency & Locking](locking.md).

## Lifecycle

`docbank daemon run` runs in the foreground: it resolves `$DOCBANK_HOME`,
creates only that root directory, takes the vault lock **exclusively**
for its entire run, initializes the remaining layout, loads and validates
`config.toml`, opens the store, cleans up any stale `blobs/tmp/`
files left by a prior crash (safe unconditionally — the exclusive lock
proves this process is the only one that could be writing them), binds
the API listener, and serves until it receives `SIGINT`/`SIGTERM` or a
shutdown request.

`docbank daemon start` spawns the same binary as a detached background
process running `daemon run`; `docbank daemon stop` asks it to shut down;
`docbank daemon restart` stops it (tolerating it not already running) and
starts it again; `docbank daemon status` reports whether it's running.
Shutdown is graceful: background tasks receive cancellation, in-flight
requests drain, tasks receive a bounded window to return, the store closes,
the vault lock releases, and the runtime record is removed — in that order,
so a stopped daemon leaves no trace for the next `daemon start` or
auto-start to trip over. HTTP draining and background-task draining each have
a ten-second ceiling. Clients preserve a 25-second graceful-exit window before
forced termination, so scheduling and cleanup do not consume either drain
budget.

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
interval ping-based discovery cannot identify the daemon, but its runtime
record and process remain live while it still owns the vault lock. Public ping
fields cannot prove that a listener appearing on the same loopback port still
belongs to that PID. Discovery therefore follows ping with a fresh nonce
challenge whose HMAC requires the per-run shutdown secret in the private
runtime record; neither that secret nor the API key crosses the socket during
the proof. Credential-bearing requests remain pinned to that proven TCP
connection and fail rather than redirecting or reconnecting. A forged or
pingless endpoint is never sent secrets. The starter instead requests graceful
process termination only for the create-time-verified PID, waits for exit, and
then starts replacement. Runtime records without create-time proof are never
trusted for this path. This handles shutdown and the rarer uncoordinated
slow-start transition without spawning into an owned vault or trusting a
rebound listener.

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

The supervisor stops accepting work as soon as shutdown begins, cancels every
runner, and waits before SQLite and blob storage close. Runners must honor
their context; the wait is bounded so a defective runner cannot prevent daemon
exit forever. The background daemon's idle-timeout loop is supervised through
this same path. Watched inboxes and scheduled maintenance are still planned,
but must use this lifecycle rather than creating unmanaged goroutines.

## Logs

A background daemon creates its logs only after acquiring vault ownership, then
logs structured JSON to `$DOCBANK_HOME/logs/`, one
file per day (`docbank-YYYY-MM-DD.log`), rotated at 50 MiB with the 5
most recent rotated files retained. A foreground `docbank daemon run`
logs to stderr instead. `DOCBANK_LOG_LEVEL` controls the level for both
(see [CLI Reference](../cli-reference.md)).
