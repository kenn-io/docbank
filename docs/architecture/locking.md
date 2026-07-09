---
title: Concurrency & Locking
description: How the daemon coordinates concurrent access — SQLite for data, a single exclusive flock for the vault, an in-daemon gate for maintenance.
---

# Concurrency & Locking

The daemon is the single process that opens the vault: it holds the
vault lock **exclusively** for its entire lifetime, and every other
consumer — CLI, agents — reaches the vault only through its HTTP API
([Daemon](daemon.md)). Two layers below that still do the real
coordination work: SQLite serializes metadata writes, and an in-process
gate serializes the daemon's own maintenance operations against its
regular ones.

## What SQLite handles

Every tree mutation is one transaction; the store opens the database in
WAL mode with `BEGIN IMMEDIATE` write transactions and a busy timeout.
Concurrent API requests interleave safely — invariants are enforced by
the schema, and losers of a name race get a typed `name already exists`
error, not corruption.

## What SQLite can't handle

Garbage collection reads the database ("which blobs are unreachable?"),
then deletes files from `blobs/`. Between those two steps, a concurrent
import could ingest the same content, observe the blob file still
present, deduplicate against it — and then GC deletes the file out from
under a freshly committed reference. No database transaction can close
that window, because half of it lives on the filesystem.

The same shape recurs at startup: clearing stale `blobs/tmp/` files must
not delete a temp file another process is actively writing.

## The vault lock: one exclusive holder

`~/.docbank/vault.lock` is an advisory `flock(2)` that `docbank daemon
run` takes exclusively (`TryLockExclusive`) at startup and releases only on
shutdown. Because it's a single long-lived process rather than one lock
acquisition per command, the shared/exclusive split from Phase 1 is
gone: with all access funneled through one process, the daemon *is* the
serialization point, and a second daemon on the same vault is impossible
by construction.

`TryLockExclusive` is **non-blocking**: a second `docbank daemon run`
against a vault that's already locked fails immediately with a clear "is a
docbank daemon already running?" error rather than hanging. This matches
the daemon's role — waiting to acquire a lock another daemon holds for
its entire lifetime would mean waiting indefinitely for no reason, since
that daemon isn't going to release it and retry.

Startup blob-tmp cleanup (`blob.CleanTmp`, the same stale-temp-file
problem described above) needs no locking scheme of its own anymore: the
daemon holding the vault lock exclusively at that point in startup
*proves* it's the sole process that could have left those files
mid-write, so cleanup is unconditional rather than a best-effort
non-blocking attempt.

**Unix only, explicitly.** The lock is the one platform-specific piece
of docbank. It's compiled under a `unix` build tag with a stub elsewhere
that fails at vault open with a clear unsupported-platform error, rather
than an undefined-symbol build break. This is unchanged from Phase 1;
the vault remains Unix-only.

## The maintenance gate: serializing inside the daemon

With one process holding the vault lock for its whole run, `gc --run`,
`trash empty`, and `verify` can no longer take the *vault* lock
exclusively the way Phase 1's CLI did — the daemon already holds it.
Instead, an in-process `sync.RWMutex`-shaped gate serializes maintenance
against regular mutations: ordinary mutating API handlers take the read
side (concurrent with each other), and `gc --run`/`trash empty`/`verify`
take the write side, giving them the same "observe a quiescent vault"
guarantee the exclusive flock gave Phase 1's `gc`. Requests queue rather
than fail. See [HTTP API: maintenance gate](http-api.md#maintenance-gate)
for the request-handling detail.

## First-open bootstrap

Creating a fresh vault has its own race: SQLite's WAL-mode conversion
and autocommit DDL both acquire locks in ways that can fail immediately
*without* consulting the busy handler when two processes race first
contact. The store applies the schema and creates the root inside one
`BEGIN IMMEDIATE` transaction and retries `SQLITE_BUSY` with a bounded
backoff; every statement is idempotent, so whichever process wins, both
converge on the same initialized vault.

`docbank daemon run` takes the vault lock before opening the store, so two
daemons racing to bootstrap the same fresh vault can no longer both
reach `store.Open` at once — the loser fails at the flock instead. The
retry logic stays in `internal/store` regardless: it's exercised
directly by the store package's own tests, and it's the correct
behavior for any caller that opens the store without first taking the
vault lock.
