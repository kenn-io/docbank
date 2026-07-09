---
title: Daemon
description: docbank serve — the single process that owns the vault, and how the CLI discovers, auto-starts, and stops it.
---

# Daemon

`docbank serve` is the one process that opens the vault. Every data
command — `add`, `ls`, `tree`, `cat`, `mv`, `rm`, `restore`, `search`,
`trash list`/`empty`, `gc`, `verify` — is an HTTP client of it, over the
[HTTP API](http-api.md). `docbank openapi` is the one exception:
it renders the API contract offline, with routes registered but never
invoked, so it needs neither a daemon nor a vault.

## Why a daemon

One process owns SQLite and the blob store; the CLI, agents, and the
future web UI are all HTTP clients of the same `/api/v1` surface. There
is no separate code path that opens the store directly — the CLI's own
commands are the design test that the agent-facing API is sufficient,
because the CLI has no other way to reach the vault.

This replaces Phase 1's model, where every command opened the store and
coordinated through the vault flock directly ([Concurrency &
Locking](locking.md) covers the daemon's single-lock-holder model that
results).

## Lifecycle

`docbank serve` runs in the foreground: it resolves `$DOCBANK_HOME`,
loads and validates `config.toml`, takes the vault lock **exclusively**
for its entire run, opens the store, cleans up any stale `blobs/tmp/`
files left by a prior crash (safe unconditionally — the exclusive lock
proves this process is the only one that could be writing them), binds
the API listener, and serves until it receives `SIGINT`/`SIGTERM` or a
shutdown request.

`docbank serve start` spawns the same binary as a detached background
process; `docbank serve stop` asks it to shut down; `docbank serve
status` reports whether it's running. Shutdown is graceful: in-flight
requests drain, the store closes, the vault lock releases, and the
runtime record is removed — in that order, so a stopped daemon leaves no
trace for the next `serve start` or auto-start to trip over.

```bash
docbank serve             # foreground; logs to stderr
docbank serve start       # background; logs to $DOCBANK_HOME/logs/
docbank serve status      # pid, address, version, uptime
docbank serve status --json
docbank serve stop
```

## Discovery

A running daemon writes a runtime record — `$DOCBANK_HOME/daemon.<pid>.json`
— naming its service (`docbank`), build version, and the actual bound
address (the configured `api_port` may be `0`, in which case the OS
picks an ephemeral port and the record carries the real one). The record
also carries, in its metadata, the process's create-time and a random
shutdown token generated at startup.

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
  serve status` and `docbank serve stop` report *any* live daemon
  regardless of version (they only discover, never start); the
  auto-start path used by data commands (`client.Ensure`) requires the
  exact match and, on a mismatch, stops the old daemon and starts a
  fresh one — see [Auto-start](#auto-start-and-idle-shutdown) below.
  `docbank serve start` does **not** replace a version-mismatched
  daemon; it reports the running instance and prints a note suggesting
  `docbank serve stop && docbank serve start` to replace it manually.

A `launch.lock` file in `$DOCBANK_HOME` serializes racing starters: two
CLI invocations that both find no daemon and both try to start one
serialize on this lock, and the second re-checks discovery after
acquiring it instead of spawning a redundant daemon.

## Auto-start and idle shutdown

Every data command calls `client.Ensure`, which discovers a
version-matched daemon or starts one — the CLI never fails with "no
daemon running" for `add`, `ls`, `cat`, and the rest. `serve status` and
`serve stop` are discovery-only and never start a daemon, so checking on
or stopping the daemon can't accidentally spawn one.

A background-spawned daemon (started via auto-start or `serve start`)
exits after `[server] idle_timeout` (default 30 minutes) with no
requests, so spawned daemons don't accumulate across sessions.
`idle_timeout = "0"` disables idle shutdown. A foreground `docbank
serve` never idles out — it runs until signaled or stopped.

## Logs

A background daemon logs structured JSON to `$DOCBANK_HOME/logs/`, one
file per day (`docbank-YYYY-MM-DD.log`), rotated at 50 MiB with the 5
most recent rotated files retained. A foreground `docbank serve` logs to
stderr instead. `DOCBANK_LOG_LEVEL` controls the level for both (see
[CLI Reference](../cli-reference.md)).
