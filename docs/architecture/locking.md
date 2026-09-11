---
title: Ownership & Concurrency
description: How daemon and embedded owners coordinate concurrent access with SQLite, hierarchy locks, and owner-local operation gates.
---

# Ownership and concurrency

Only one process may own a vault at a time. A daemon or embedded `Vault` holds
an exclusive lock for its lifetime. The lock also excludes another owner or
restore in any parent or descendant directory.

Standalone CLI commands and agents use the owner's [HTTP API](daemon.md).
Embedded applications own a separate vault root. Inside either owner, SQLite
serializes metadata writes. In-process locks coordinate operations that also
read or change files.

## What SQLite handles

Every tree mutation is one transaction; the store opens the database in WAL mode
with `BEGIN IMMEDIATE` write transactions and a busy timeout. Concurrent API
requests interleave safely — invariants are enforced by the schema, and losers
of a name race get a typed `name already exists` error, not corruption.

## What SQLite can't handle

Garbage collection must coordinate its database query with file deletion.
Without that coordination, the operations would have this race:

1. GC identifies a blob that no metadata retains.
2. An import finds the same bytes already on disk and reuses that file.
3. The import commits a new reference.
4. GC deletes the file selected in step 1.

A SQLite transaction cannot coordinate all four steps because file deletion
happens outside SQLite. The maintenance gate described below prevents this
interleaving.

Startup cleanup has a related requirement: the daemon must not remove a
`blobs/tmp/` file that another writer still uses.

## The vault lock: one exclusive holder per vault tree

`~/.docbank/vault.lock` is an advisory byte-range file lock that
`docbank daemon run` takes exclusively (`TryLockExclusive`) at startup and
releases only on shutdown. Unix uses `flock(2)` and Windows uses `LockFileEx`.
Because it's a single long-lived process rather than one lock acquisition per
command, no per-command shared/exclusive split remains: with all access funneled
through one process, the daemon *is* the serialization point, and a second
daemon on the same vault is impossible by construction.

Restore uses the same lock for its separate target tree. Kit first opens the
target without following a final symlink and keeps that directory descriptor
through publication. Docbank validates and locks that exact held directory, then
Kit performs every cleanup and write relative to it. Renaming or replacing the
target pathname cannot redirect a restore into the live vault, repository, or
another tree.

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
identity above. These locks coordinate Docbank daemons and restores that retain
the paths they were given. They do not attempt to make arbitrary same-user
filesystem reparenting a safe operation: a process able to move a restore root
into another live vault already has the authority to modify that vault directly.
The held `os.Root` still prevents such a rename from redirecting restore writes
through the old pathname.

Restore acquires this hierarchy before writing even when the target is fresh,
while the serving daemon continues to hold the live vault's hierarchy. This
prevents a second restore, a daemon pointed at the target, or a restore nested
inside an active vault from racing publication. Daemon startup creates only the
root needed for locking before it attempts the lock; it does not initialize the
database, blob tree, logs, or configuration first.

`TryLockExclusive` is **non-blocking**: a second `docbank daemon run` or restore
against an overlapping vault tree fails immediately rather than hanging. This
matches the daemon's role — waiting to acquire a lock another daemon holds for
its entire lifetime would mean waiting indefinitely. Restore reports its
conflict as `backup_restore_target_active`.

The `vault.lock` pathname is stable coordination state, not a success marker. It
remains after both successful and failed restores. Removing a held lock file
would allow a contender to create and lock a different inode at the same path,
breaking mutual exclusion; restore retries therefore ignore the retained file
when applying the empty-target rule.

Startup calls `blob.CleanTmp` while the daemon holds the exclusive vault lock.
No other Docbank process can be writing those temporary files at that point,
so cleanup does not need a separate lock or retry policy.

The lock implementation is platform-specific without changing the contract. Unix
retries interrupted `flock` calls; Windows uses non-blocking shared or exclusive
`LockFileEx` ranges. Windows directory identities come from opened handles, and
final reparse points are rejected in the same places Unix rejects symlinks. The
target-lock registry and vault root are private to the current user: POSIX modes
enforce this on Unix and restricted DACLs enforce it on Windows.

## The maintenance gate: serializing inside the daemon

With one process holding the vault lock for its whole run, `gc --run`,
`trash empty`, and `verify` cannot take the *vault* lock exclusively per command
— the daemon already holds it. Instead, an in-process `sync.RWMutex`-shaped gate
serializes maintenance against regular mutations: ordinary mutating API handlers
take the read side (concurrent with each other), and
`gc --run`/`trash empty`/`verify` take the write side, giving them the same
"observe a quiescent vault" guarantee as an exclusive per-command lock. Once
maintenance is running or queued, a new mutation fails immediately with
`503 maintenance_busy` rather than blocking. See
[HTTP API: maintenance gate](http-api.md#maintenance-gate) for the
request-handling detail.

`OperationGate` is specifically the daemon HTTP scheduler. It distinguishes
ordinary mutating handlers from whole-run maintenance handlers and lets queued
requests share one long-lived daemon safely. It is not the embedded lifecycle
lock and is not acquired by embedded methods.

## Embedded lifecycle and mutation locking

An embedded `Vault` has two separate in-process responsibilities. Its lifecycle
read/write lock pins the vault while a method, verified content stream, or
snapshot `Walker` is active; `Vault.Close` takes the write side and waits until
those leases are released before closing SQLite, physical storage, and the
hierarchy lock. A walker owns a dedicated SQLite connection and read snapshot
until `Walker.Close`, independently of the context used to set it up.

The embedded mutation mutex serializes logical mutations and physical
maintenance that must transition SQLite authority and filesystem state as one
owner-controlled operation. Kit's storage coordinator remains the lower-level
reader/mutation/maintenance boundary for loose and packed content. Neither lock
replaces the hierarchy lock: lifecycle and mutation locks coordinate goroutines
inside one embedded owner, while the hierarchy lock excludes overlapping vault
owners across processes and embedded instances.

## First-open bootstrap

Creating a fresh vault has its own race: SQLite's WAL-mode conversion and
autocommit DDL both acquire locks in ways that can fail immediately *without*
consulting the busy handler when two processes race first contact. The store
applies the schema and creates the root inside one `BEGIN IMMEDIATE` transaction
and retries `SQLITE_BUSY` with a bounded backoff; every statement is idempotent,
so whichever process wins, both converge on the same initialized vault.

`docbank daemon run` takes the vault lock before opening the store, so two
daemons racing to bootstrap the same fresh vault can no longer both reach
`store.Open` at once — the loser fails at the vault lock instead. The retry
logic stays in `internal/store` regardless: it's exercised directly by the store
package's own tests, and it's the correct behavior for any caller that opens the
store without first taking the vault lock.
