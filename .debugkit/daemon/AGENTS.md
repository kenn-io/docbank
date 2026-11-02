# Daemon Package Instructions

## Scope

`daemon/` owns shared lifecycle pieces for local background daemons: endpoint
parsing, loopback or Unix-socket clients, runtime records, liveness probes,
listen locks, and caller-driven auto-start. Application server setup, auth,
databases, command parsing, and shutdown policy belong to the caller.

## Invariants

- Never infer live daemon state from a runtime record alone. Probe the endpoint
  before claiming a process is reachable.
- With PID-aware discovery, retain a live record's probe failure until another
  reachable record wins. Absence is a definite result, not a fallback for an
  endpoint that could not be probed.
- Treat process-creation identity as exact-match only, except that the Linux
  identity's boot ID, PID namespace, and start ticks are read to place the
  process. An unknown identity never authorizes destructive action against a
  process or record.
- A record whose identity this caller can't judge is kept until removed: a
  versioned identity from another PID namespace on this boot, a nonempty one
  this build can't read (malformed, or from a newer version), or a well-formed
  Linux one when the caller can't read its own boot ID or PID namespace.
  `CleanupDead` keeps it, `Discover` probes it, and a failed probe is an
  `UnreachableError` that blocks `Ensure`. It leaves when a caller that can
  prove the server gone by its own means (for example an application-level
  ownership lock it can acquire) removes it, when a process in the server's
  namespace cleans up, or when it's deleted by hand. After a downgrade, a dead
  newer server's record blocks `Ensure` this way.
  - `RuntimeProcessScope` reports Local for this namespace or absent
    `ProcessIdentityV2` (all non-Linux records), where a PID check applies;
    Foreign for another namespace on this boot; Unknown for unrecognized or
    unreadable identities, boot IDs, or own PID namespaces; Gone for a proven
    dead process. Neither Foreign nor Unknown authorizes deleting the record
    or signaling its PID. For Foreign records, `RuntimeProcessAlive` stays true
    to keep the record, while `RuntimeRecordBlocksStart` is false because its
    PID makes no local claim. `RuntimeRecordBlocksStart` blocks only for a live
    Local or Unknown PID whose identity isn't a mismatch, never for the caller's
    own PID.
- A caller in the initial PID namespace (inode 4026531836) treats a same-boot
  record from a foreign namespace as gone when no process in a nested PID
  namespace has the record's PID as its innermost `NSpid` entry and the
  identity's start ticks. The walk reads only `/proc/<pid>/status` and `stat`,
  which every uid can read, so it sees servers under any host uid. Any read
  failure other than an exited process, or a `/proc` that hides pid 1
  (`hidepid`), leaves the record unjudged. Don't narrow the walk to the
  caller's uid or to files that need ptrace access, such as `ns/pid`: a missed
  live server reads as gone and its record is deleted. A server in a time
  namespace with a boot-time offset reports shifted start ticks and reads as
  gone.
- An identity that proves the process gone, whether from an earlier boot or a
  gone nested process, ends the liveness question: `RuntimeProcessAlive` is
  false even if the record's PID is alive here. A live container with its own
  boot ID therefore reads as an earlier boot, and its record is not kept. Only
  a same-namespace identity or a record with no versioned identity takes the
  plain PID check.
- Never send a bearer credential to a runtime-record endpoint before it proves
  possession of the daemon token and its recorded service, version, PID,
  network, and address. Use the package proof ping and probe contract for this
  ordering.
- Default network behavior stays local. Do not add public listen addresses or
  broad bind behavior without an explicit caller option.
- `Stop` runs the shutdown request only after `runtimeRecordLive` and the
  endpoint verify.
  - TCP records require `Proof` in every process scope.
  - Unix records may use a plain PID ping.
  - When set, only `Exited` confirms exit.
  - Otherwise, a refused connection or another PID answering ends the wait.
  - `Stop` removes no files.
- Unix sockets and runtime records must live under private current-user
  directories.
- Runtime records are written with `atomicfile.WriteFile` (mode 0644, fsynced).
  Its `.<name>.tmp-*` staging files and the write-check probe must stay outside
  the `<prefix>.<pid>.json` namespace that `List` and `CleanupDead` match, so
  leftovers are never read or removed as records.
- When a runtime write returns an error wrapping `atomicfile.ErrPublished`,
  `RuntimeStore.Write` returns the final path with that error because the
  record is already visible there.
- `RuntimeStore.Prefix` must pass `fsname.Check` and be at most 214 bytes so
  generated staging names stay within the portable 255-byte component limit.
  It becomes part of every runtime file name, so reject it rather than rename
  it.
- Do not remove an existing path unless it is known to be the stale Unix socket
  this package created. Refuse paths whose type or ownership does not match
  that intent.
- Use listen locks to serialize startup and bind attempts.
- Unix-socket `Listen` runs the same lock, probe, remove, and bind sequence on
  Windows. A process killed there leaves its AF_UNIX socket file behind, and
  bind fails until the file is removed.
- Hold the owner lock for the daemon's full writable lifetime; use the start
  lock only to serialize discovery, replacement, and launch decisions.
- Keep blocking and nonblocking start-lock acquisition on the same local
  semaphore and file lock. Contention is authoritative; an acquisition error
  means unknown state. Application snapshots never override a held lock.
- Callers own startup snapshots and cleanup policy. Cleanup must finish while
  holding the start lock. Preserve the lock file so all holders use the same
  filesystem object.
- Route every `Manager.Ensure` lookup through `Manager.Find` so a caller's
  `FindFunc` is used for initial discovery, locked re-discovery, and polling.
- Keep platform-specific behavior in build-tagged files when ownership, sockets,
  process checks, or permissions differ.
- On Windows, an openable process object is not proof that the process is still
  running; retained handles outlive termination, so liveness must check the
  process exit status for `STILL_ACTIVE`.
- Auto-start goes through the caller-provided `StartFunc`; this package must not
  invent application launch commands.
- `StartDetached` reaps the child itself, so a caller cannot call `Wait`. A
  caller that needs to know the child died during startup uses `Exited`.
- Windows detached children use `DETACHED_PROCESS`, not `CREATE_NO_WINDOW`.
  Hidden consoles expose `CONIN$`, which can make terminal-probing libraries
  block forever at daemon startup. Non-interactive console-subsystem
  descendants that must avoid visible windows should call `consolewindow.Hide`
  at their own spawn site.

## Tests

- Use local HTTP handlers, temporary dirs, and per-test runtime stores.
- Assert what a client observes: status code, response body, runtime record, or
  returned error.
- Avoid timing assumptions. Prefer explicit locks, probes, and short contexts
  over sleeps.
- Do not require elevated privileges or external daemons.
