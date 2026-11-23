---
title: Ownership and concurrency
description: How daemon and embedded owners coordinate concurrent access with SQLite, hierarchy locks, and owner-local operation gates.
---

# Ownership and concurrency

Only one process may own a vault at a time. A daemon or embedded `Vault` holds
an exclusive lock for its entire lifetime, which also excludes another owner or
restore in any parent or descendant directory.

Standalone CLI commands and agents talk to the owner through its
[HTTP API](daemon.md), while embedded applications own a separate vault root.
Inside either owner, SQLite serializes metadata writes, and in-process locks
coordinate operations that also read or change files.

## What SQLite handles

Every tree mutation runs in one transaction. The store opens the database in WAL
mode with `BEGIN IMMEDIATE` write transactions and a busy timeout. Email
attachment receipt release retries a busy `BEGIN` until the writer lock clears
or the request context ends. Concurrent API requests interleave safely, the
schema enforces invariants, and the loser of a name race gets a typed
`name already exists` error instead of corruption.

## What SQLite can't handle

Garbage collection must coordinate its database query with file deletion.
Without that coordination, these steps could interleave:

1. GC identifies a blob that no metadata retains.
2. An import finds the same bytes already on disk and reuses that file.
3. The import commits a new reference.
4. GC deletes the file selected in step 1.

Because file deletion happens outside SQLite, a SQLite transaction cannot
coordinate all four steps. The maintenance gate below prevents this race.

Startup cleanup has a related requirement: the daemon must not remove a
`blobs/tmp/` file that another writer still uses.

## The vault lock

`~/.docbank/vault.lock` is an advisory file lock that `docbank daemon run`
takes exclusively with `OpenAndLockExclusive` at startup and releases only on
shutdown. Unix uses `flock(2)` and Windows uses `LockFileEx`. Because the
daemon is a single long-lived process, there is no per-command lock acquisition
and no per-command shared/exclusive split. All access goes through one process,
so the daemon *is* the serialization point, and a second daemon on the same
vault is impossible by construction.

Restore uses the same lock for its separate target tree. Kit opens the target
without following a final symlink and keeps that directory descriptor through
publication. Docbank validates and locks that held directory, then Kit performs
every cleanup and write relative to it. Renaming or replacing the target
pathname cannot redirect a restore into the live vault, repository, or another
tree.

A `vault.lock` alone cannot coordinate overlapping roots because `/restore` and
`/restore/nested` contain different lock files. Docbank therefore also keeps a
canonical per-user target-lock registry at
`~/.local/state/docbank/target-locks`, resolved from the operating-system user
record rather than `HOME` or XDG environment variables. Each daemon or restore
takes shared locks for the filesystem identities of all ancestors and an
exclusive lock for its root identity. Unix keys these identities by device and
inode, and Windows uses volume serial and file ID. Parent and descendant trees
consequently conflict in either acquisition order, while disjoint sibling vault
daemons remain independent.

If you run in an isolated environment with an unwritable account home, you can
set `DOCBANK_LOCK_DIR` to an absolute directory outside your vaults and restore
targets. This replaces the registry location for all lock operations, including
embedded vaults, daemon startup, and restore, while the existing
private-directory checks still apply. Docbank never falls back to this setting
automatically. You select it explicitly, and every process accessing
overlapping trees must use the same setting. Do not change this setting while a
process is running; stop all such processes first. Processes with different
settings do not coordinate hierarchy locks.

The persistent registry files contain no vault data and must not be removed.
Their stable names are coordination state keyed by the platform filesystem
identity above. These locks coordinate Docbank daemons and restores that retain
the paths they were given. They do not attempt to make arbitrary same-user
filesystem reparenting safe: a process able to move a restore root into another
live vault already has authority to modify that vault directly. The held
`os.Root` still prevents such a rename from redirecting restore writes through
the old pathname.

Restore acquires this hierarchy before writing even when the target is fresh,
while the serving daemon continues to hold the live vault's hierarchy. This
prevents a second restore, a daemon pointed at the target, or a restore nested
inside an active vault from racing publication. Daemon startup creates only the
root needed for locking before attempting the lock; it does not initialize the
database, blob tree, logs, or configuration first.

An embedded content reader takes the same exclusive hierarchy locks as an
owner, so it conflicts with daemons, embedded vaults, restores, and other
readers on overlapping trees. It never creates the vault root or `vault.lock`;
if either is missing, the vault needs an owner first. The reader also refuses a
registry inside the vault because its lock files would change the tree it
promises not to write.

The vault lock is **non-blocking**: a second `docbank daemon run` or a restore
against an overlapping vault tree fails immediately rather than hanging. That
matches the daemon's role, because waiting for a lock that another daemon holds
for its entire lifetime would mean waiting indefinitely. Restore reports this
conflict as `backup_restore_target_active`.

The `vault.lock` pathname is stable coordination state, not a success marker.
It remains after both successful and failed restores. Removing a held lock
file would let a contender create and lock a different inode at the same path,
breaking mutual exclusion. Restore retries therefore ignore the retained file
when applying the empty-target rule.

Startup calls `blob.CleanTmp` while the daemon holds the exclusive vault lock.
No other Docbank process can be writing those temporary files at that point, so
cleanup needs no separate lock or retry policy.

The lock implementation is platform-specific without changing the contract.
Unix retries interrupted `flock` calls, while Windows uses non-blocking shared
or exclusive `LockFileEx` ranges. On Windows, directory identities come from
opened handles, and final reparse points are rejected in the same places Unix
rejects symlinks. Both the target-lock registry and the vault root are private
to the current user, enforced by POSIX modes on Unix and restricted DACLs on
Windows.

## The maintenance gate

With one process holding the vault lock for its whole run, `vault gc --run`,
`trash empty`, and `vault verify` cannot take the *vault* lock exclusively per
command because the daemon already holds it. Instead, an in-process
`sync.RWMutex`-shaped gate serializes maintenance against regular mutations.
Ordinary mutating API handlers take the read side and run concurrently with
each other. `vault gc --run`, `trash empty`, and `vault verify` take the write
side, giving them the same "observe a quiescent vault" guarantee as an
exclusive per-command lock. Once maintenance is running or waiting for
in-flight mutations, a new mutation fails immediately with
`503 maintenance_busy` rather than blocking. Maintenance that is still waiting
for a backup capture does not refuse mutations. See
[HTTP API: maintenance gate](http-api.md#maintenance-gate) for the
request-handling details.

`OperationGate` is the daemon HTTP scheduler. It distinguishes ordinary
mutating handlers from whole-run maintenance handlers and lets queued requests
share one long-lived daemon safely. It is not the embedded lifecycle lock and
is not acquired by embedded methods.

## Embedded lifecycle and mutation locking

An embedded `Vault` separates two in-process responsibilities. Its lifecycle
read/write lock pins the vault while a method, verified content stream, or
snapshot `Walker` is active. `Vault.Close` takes the write side and waits until
those leases are released before closing SQLite, physical storage, and the
hierarchy lock. A walker owns a dedicated SQLite connection and read snapshot
until `Walker.Close`, independently of the context used to set it up.

The embedded mutation mutex serializes logical mutations and physical
maintenance that must transition SQLite authority and filesystem state as one
owner-controlled operation. Kit's storage coordinator remains the lower-level
reader, mutation, and maintenance boundary for loose and packed content.
Neither lock replaces the hierarchy lock: lifecycle and mutation locks
coordinate goroutines inside one embedded owner, while the hierarchy lock
excludes overlapping vault owners across processes and embedded instances.

## First-open bootstrap

Creating a fresh vault has its own race: SQLite's WAL-mode conversion and
autocommit DDL both acquire locks in ways that can fail immediately *without*
consulting the busy handler when two processes race on first contact. The store
applies the schema and creates the root inside one `BEGIN IMMEDIATE`
transaction and retries `SQLITE_BUSY` with a bounded backoff. Every statement
is idempotent, so whichever process wins, both converge on the same initialized
vault.

Because `docbank daemon run` takes the vault lock before opening the store, two
daemons racing to bootstrap the same fresh vault can no longer both reach
`store.Open` at once; the loser fails at the vault lock instead. The retry
logic stays in `internal/store` regardless, because the store package's own
tests exercise it directly and it is the right behavior for any caller that
opens the store without first taking the vault lock.
