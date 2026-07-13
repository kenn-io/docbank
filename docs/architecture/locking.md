---
title: Concurrency & Locking
description: How the daemon coordinates concurrent access — SQLite for data, a portable exclusive vault lock, and an in-daemon gate for maintenance.
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

## The vault lock: one exclusive holder per vault tree

`~/.docbank/vault.lock` is an advisory byte-range file lock that `docbank
daemon run` takes exclusively (`TryLockExclusive`) at startup and releases
only on shutdown. Unix uses `flock(2)` and Windows uses `LockFileEx`. Because
it's a single long-lived process rather than one lock
acquisition per command, the shared/exclusive split from Phase 1 is
gone: with all access funneled through one process, the daemon *is* the
serialization point, and a second daemon on the same vault is impossible
by construction.

Restore uses the same lock for its separate target tree. Kit first opens the
target without following a final symlink and keeps that directory descriptor
through publication. Docbank validates and locks that exact held directory,
then Kit performs every cleanup and write relative to it. Renaming or replacing
the target pathname cannot redirect a restore into the live vault, repository,
or another tree.

A `vault.lock` alone cannot coordinate overlapping roots: `/restore` and
`/restore/nested` contain different lock files. Docbank therefore also keeps a
canonical per-user target-lock registry at
`~/.local/state/docbank/target-locks`, resolved from the operating-system user
record rather than `HOME` or XDG environment variables. Each daemon or restore
takes shared locks for the filesystem identities of all ancestors and an
exclusive lock for its root identity. Unix keys these identities by device and
inode; Windows uses volume serial and file ID. Parent and descendant trees
consequently conflict in either acquisition order, while disjoint sibling vault
daemons remain independent.

The persistent registry files contain no vault data and must not be removed;
their stable names are coordination state keyed by the platform filesystem
identity above. These
locks coordinate Docbank daemons and restores that retain the paths they were
given. They do not attempt to make arbitrary same-user filesystem reparenting a
safe operation: a process able to move a restore root into another live vault
already has the authority to modify that vault directly. The held `os.Root`
still prevents such a rename from redirecting restore writes through the old
pathname.

Restore acquires this hierarchy before writing even when the target is fresh,
while the serving daemon continues to hold the live vault's hierarchy. This
prevents a second restore, a daemon pointed at the target, or a restore nested
inside an active vault from racing publication. Daemon startup creates only the
root needed for locking before it attempts the lock; it does not initialize the
database, blob tree, logs, or configuration first.

`TryLockExclusive` is **non-blocking**: a second `docbank daemon run` or restore
against an overlapping vault tree fails immediately rather than hanging.
This matches the daemon's role — waiting to acquire a lock another daemon holds
for its entire lifetime would mean waiting indefinitely. Restore reports its
conflict as `backup_restore_target_active`.

The `vault.lock` pathname is stable coordination state, not a success marker.
It remains after both successful and failed restores. Removing a held lock file
would allow a contender to create and lock a different inode at the same path,
breaking mutual exclusion; restore retries therefore ignore the retained file
when applying the empty-target rule.

Startup blob-tmp cleanup (`blob.CleanTmp`, the same stale-temp-file
problem described above) needs no locking scheme of its own anymore: the
daemon holding the vault lock exclusively at that point in startup
*proves* it's the sole process that could have left those files
mid-write, so cleanup is unconditional rather than a best-effort
non-blocking attempt.

The lock implementation is platform-specific without changing the contract.
Unix retries interrupted `flock` calls; Windows uses non-blocking shared or
exclusive `LockFileEx` ranges. Windows directory identities come from opened
handles, and final reparse points are rejected in the same places Unix rejects
symlinks. The target-lock registry and vault root are private to the current
user: POSIX modes enforce this on Unix and restricted DACLs enforce it on
Windows.

## The maintenance gate: serializing inside the daemon

With one process holding the vault lock for its whole run, `gc --run`,
`trash empty`, and `verify` can no longer take the *vault* lock
exclusively the way Phase 1's CLI did — the daemon already holds it.
Instead, an in-process `sync.RWMutex`-shaped gate serializes maintenance
against regular mutations: ordinary mutating API handlers take the read
side (concurrent with each other), and `gc --run`/`trash empty`/`verify`
take the write side, giving them the same "observe a quiescent vault"
guarantee the exclusive vault lock gave Phase 1's `gc`. Requests queue rather
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
reach `store.Open` at once — the loser fails at the vault lock instead. The
retry logic stays in `internal/store` regardless: it's exercised
directly by the store package's own tests, and it's the correct
behavior for any caller that opens the store without first taking the
vault lock.
