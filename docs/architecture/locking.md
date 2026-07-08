---
title: Concurrency & Locking
description: How concurrent docbank processes coordinate — SQLite for data, an advisory flock for cross-layer operations.
---

# Concurrency & Locking

Two layers coordinate concurrent access: SQLite serializes all metadata
writes, and an advisory file lock serializes the few operations that
span the database *and* the blob directory.

## What SQLite handles

Every tree mutation is one transaction; the store opens the database in
WAL mode with `BEGIN IMMEDIATE` write transactions and a busy timeout.
Concurrent imports, moves, and trash operations from multiple processes
interleave safely — invariants are enforced by the schema, and losers of
a name race get a typed `name already exists` error, not corruption.

## What SQLite can't handle

Garbage collection reads the database ("which blobs are unreachable?"),
then deletes files from `blobs/`. Between those two steps, a concurrent
import could ingest the same content, observe the blob file still
present, deduplicate against it — and then GC deletes the file out from
under a freshly committed reference. No database transaction can close
that window, because half of it lives on the filesystem.

The same shape recurs at startup: clearing stale `blobs/tmp/` files must
not delete a temp file another process is actively writing.

## The vault lock

`~/.docbank/vault.lock` is an advisory `flock(2)`:

| Holder | Mode | Held for |
|--------|------|----------|
| Every normal command | **shared** | the life of the command |
| `docbank gc` | **exclusive** | the whole collection |
| startup tmp cleanup | momentary **exclusive** (non-blocking attempt) | the cleanup only |

Normal commands coexist; `gc` waits for them to finish and blocks new
ones while it runs, which makes its query-then-delete sequence
atomic *with respect to other docbank processes*. Startup cleanup tries
a non-blocking upgrade to exclusive: success proves this is the only
live process, so stale temp files can be removed; failure (any other
holder) skips cleanup and lets a later sole process or `gc` handle it.

Two implementation details worth recording:

- **Failed upgrades must reacquire.** On Linux, flock lock conversion is
  release-then-acquire, so a failed non-blocking upgrade can silently
  drop the shared lock. The lock wrapper reacquires shared before
  reporting failure — otherwise a command would keep running convinced
  it holds the vault while holding nothing.
- **Unix only, explicitly.** The lock is the one platform-specific piece
  of docbank. It's compiled under a `unix` build tag with a stub
  elsewhere that fails at vault open with a clear unsupported-platform
  error, rather than an undefined-symbol build break.

## First-open bootstrap

Creating a fresh vault has its own race: SQLite's WAL-mode conversion
and autocommit DDL both acquire locks in ways that can fail immediately
*without* consulting the busy handler when two processes race first
contact. The store applies the schema and creates the root inside one
`BEGIN IMMEDIATE` transaction and retries `SQLITE_BUSY` with a bounded
backoff; every statement is idempotent, so whichever process wins, both
converge on the same initialized vault.

## Guidance for other layers

The daemon (Phase 2) is just another vault-lock participant: API
mutations take the shared lock like CLI commands, and scheduled GC takes
exclusive. Nothing about the model assumes a single resident process —
which is precisely why a CLI invoked while the daemon runs stays safe.
