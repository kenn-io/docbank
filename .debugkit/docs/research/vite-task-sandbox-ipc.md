# Vite Task's sandbox-compatible IPC changes

## Scope

Vite+ v0.2.9 says `vp run` now works in the default Codex CLI and Claude Code
sandboxes. The release attributes the change to Vite Task PRs
[#569](https://github.com/voidzero-dev/vite-task/pull/569) and
[#576](https://github.com/voidzero-dev/vite-task/pull/576). These are two
separate fixes: runner communication stopped using Unix domain sockets, and
file-access tracking stopped using POSIX shared memory and a Linux descriptor
broker. The [release note](https://github.com/voidzero-dev/vite-plus/releases/tag/v0.2.9)
describes the user-visible result.

Vite+ picked up both fixes by updating its Vite Task revision to
`d05b1dcdbaabaa69643ee0b89cebe3cd390957e9` in
[#2403](https://github.com/voidzero-dev/vite-plus/pull/2403). The pinned
revision appears on the
[`fspy` dependency](https://github.com/voidzero-dev/vite-plus/blob/b0fa8dc1ba242cb1d81326fb06ac54a81f75ef40/Cargo.toml#L186)
and the
[`vt` dependencies](https://github.com/voidzero-dev/vite-plus/blob/b0fa8dc1ba242cb1d81326fb06ac54a81f75ef40/Cargo.toml#L299-L304).
That revision contains the merged commits
[`f5919072`](https://github.com/voidzero-dev/vite-task/commit/f5919072f109463286f872b36fa2bed3d213cb91)
for PR #569 and
[`29fcbd62`](https://github.com/voidzero-dev/vite-task/commit/29fcbd62587360433886f265516b264129cdf439)
for PR #576.

## What Vite Task changed

### Runner communication uses named pipes

The original failure happened before task code ran. The cached task runner
tried to bind a Unix domain socket and received `EPERM` in both default
sandboxes, as recorded in
[#562](https://github.com/voidzero-dev/vite-task/issues/562). PR #569 kept the
socket-like API but changed its Unix implementation to filesystem FIFOs.
Windows continues to use named pipes. The transport's public API exposes only
bind, name, accept, and connect, leaving the platform choice inside the
[`socket_ipc` crate](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/lib.rs#L20-L107).

On Unix, each server owns a private temporary directory with a well-known
`connect` FIFO. A client creates one request FIFO and one response FIFO in that
directory, then announces a 16-byte UUID. The announcement fits within
`PIPE_BUF`, so concurrent announcements cannot interleave. The server replies
with one ready byte before the two FIFOs become the connection's byte streams.
The protocol and its permissions are documented and implemented in
[`unix.rs`](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L1-L38)
and the
[`Server` setup](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L68-L127).

The interesting part is failure behavior. The client opens the rendezvous FIFO
nonblocking before it creates per-client state, translating `ENXIO` into
`ConnectionRefused` when no server reader exists. During the ready-byte
handshake it polls the response and rendezvous descriptors, then reopens the
rendezvous every 100 ms as a liveness probe for macOS, where FIFO events from
`poll` are not reliable enough. The server treats a client that dies during
the handshake as a per-client failure and accepts the next announcement. See
the
[`Client::connect` implementation](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L168-L303)
and the
[`accept` loop](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L98-L127).
Integration tests cover both a stale announcement and a server that disappears
before or during connection, with timeouts that make a hang observable
([tests](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/tests/integration.rs#L79-L175)).

The server does not register established FIFOs with Tokio's reactor on macOS.
It hands ordinary file descriptors to Tokio's blocking file implementation
because kqueue did not wake reliably for these FIFOs
([source](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L118-L125)).

This is child-runner IPC, not general daemon discovery. The server creates a
unique endpoint and passes its opaque name to child processes through an
environment variable
([server](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/vt_server/src/lib.rs#L261-L294),
[client](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/vt_client/src/lib.rs#L28-L48)).
A long-lived daemon still needs a stable, access-controlled way for unrelated
clients to find the current endpoint.

### File tracking uses a sparse temporary file

The second failure appeared after runner IPC was unblocked. Codex's macOS
Seatbelt profile denied `shm_open`; on Linux, the earlier `memfd` design needed
a Unix socket broker to pass descriptors. Vite Task tracked those failures in
[#563](https://github.com/voidzero-dev/vite-task/issues/563) and replaced all
three platform backends in PR #576.

The replacement creates a uniquely named sparse file in the system temporary
directory, resolves its absolute path once, and passes that path as the opaque
identifier. Unix creates it with mode `0600`; Windows relies on the per-user
temporary-directory ACL and marks the file sparse before setting its length.
Every platform maps the file with `memmap2`. This removes the descriptor
broker, global shared-memory names, and the need for an asynchronous runtime
([design](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/README.md#L25-L56),
[creation code](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/src/file_backed.rs#L58-L110)).

Vite Task separates the lifetime of the name, the open file, and each mapping.
Dropping the keeper removes the name so later opens fail, while existing
handles and mappings remain usable. Channel shutdown uses a separate lock-file
gate rather than treating unlink as a stop signal
([lifetime contract](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/README.md#L62-L72),
[channel gate](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shared/src/ipc/channel/mod.rs#L41-L58)).
Tests pin name removal and continued access through existing mappings and
handles
([tests](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/src/file_backed.rs#L261-L304)).

The design accepts one cleanup limit explicitly. If the keeper process is
killed, its drop handler does not run and the backing file remains for a temp
reaper or cleanup tool. Because the file is sparse, its physical cost is tied
to pages actually written rather than its logical capacity
([lifetime notes](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/README.md#L62-L72)).

## Lessons worth carrying into daemon work

1. Treat sandbox compatibility as a transport constraint. Vite Task chose
   primitives available in the default profiles, ordinary files and named
   pipes, instead of asking callers for wider sandbox permissions
   ([release](https://github.com/voidzero-dev/vite-plus/releases/tag/v0.2.9)).
2. Make endpoint identity portable across process context. Vite Task sends an
   opaque absolute path, then tests a child with a different working directory
   and temporary-directory environment
   ([implementation and test](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/src/file_backed.rs#L81-L135),
   [subprocess test](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/src/file_backed.rs#L227-L259)).
3. Design dead-peer behavior before the happy path. Nonblocking opens,
   bounded liveness probes, stale-client isolation, and explicit no-hang tests
   are the strongest reusable part of PR #569
   ([client](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L168-L303),
   [tests](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/tests/integration.rs#L79-L175)).
4. Separate discovery, liveness, data lifetime, and shutdown. In Vite Task,
   the endpoint name finds a server, the rendezvous detects server death, open
   mappings keep bytes alive, and the channel's gate rejects new writers.
   Unlinking alone does not carry all four meanings
   ([FIFO protocol](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L1-L38),
   [mapping lifetime](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/README.md#L62-L72)).
5. Test the actual restricted environment and the user-visible behavior. The
   Codex fixture runs a nested cached task in the workspace profile, changes a
   file read by that task, and records the next run as a cache miss
   ([Codex snapshot](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/vt_bin/tests/e2e_snapshots/fixtures/sandboxed_fspy/snapshots/fspy_under_codex_sandbox.md#L1-L24)).
   The Claude fixture records the equivalent flow under Sandbox Runtime
   ([Claude snapshot](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/vt_bin/tests/e2e_snapshots/fixtures/sandboxed_fspy/snapshots/fspy_under_anthropic_sandbox_runtime.md#L1-L32)).

The FIFO protocol is useful when both processes share a writable directory
but Unix sockets and loopback networking are unavailable. It is not a drop-in
replacement for every daemon socket. Vite Task pays for it with a custom
handshake, two FIFOs per client, SIGPIPE precautions, a macOS polling probe,
and blocking-pool I/O. The sparse mapped file solves high-volume file-access
recording, not request-response control traffic. Those boundaries are visible
in the
[`socket_ipc` implementation](https://github.com/voidzero-dev/vite-task/blob/f5919072f109463286f872b36fa2bed3d213cb91/crates/socket_ipc/src/unix.rs#L1-L38)
and the
[`fspy_shm` design](https://github.com/voidzero-dev/vite-task/blob/29fcbd62587360433886f265516b264129cdf439/crates/fspy_shm/README.md#L25-L60).

## Comparison with Kit's daemon package

Kit currently supports HTTP over TCP and Unix sockets. Empty-address parsing
prefers a caller-supplied Unix path, `DefaultSocketPath` prepares that path,
and `Listen` ultimately calls `net.Listen` for the chosen network
([endpoint.go](../../daemon/endpoint.go), [listen.go](../../daemon/listen.go)).
`StartDetached` creates a new Unix session with `setsid`, but it does not and
cannot remove an inherited sandbox policy
([start.go](../../daemon/start.go), [detach_unix.go](../../daemon/detach_unix.go)).

Loopback TCP is not a dependable automatic fallback. Current official OpenAI
documentation says Codex defaults command network access off. When the command
network proxy is enabled, local binding defaults to false and Unix sockets need
explicit allow rules
([Codex agent approvals and security](https://learn.chatgpt.com/docs/agent-approvals-security#network-access)).
Claude Code also applies its sandbox boundary to every Bash command and child
process. Its documented defaults set `network.allowLocalBinding` and
`network.allowAllUnixSockets` to false
([sandboxing](https://code.claude.com/docs/en/sandboxing),
[settings](https://code.claude.com/docs/en/settings#sandbox-settings)).

The practical options are:

1. Improve failure classification first. Preserve the underlying permission
   error, name the transport and endpoint that failed, and tell callers that
   the sandbox must allow that primitive. This makes today's failure actionable
   without pretending another socket type will work.
2. If the package must work in the default sandbox profiles without permission
   changes, add a file-backed byte-stream transport behind `Endpoint`. Keep its
   name opaque and absolute, place it in a private writable directory, and
   define bounded connect and dead-peer behavior before exposing it to HTTP.
   A silent Unix-to-TCP retry would not meet that requirement.
3. Keep sandbox-compatible transport separate from daemon persistence. Vite
   Task proves child IPC during one command. It does not establish that a
   detached daemon survives the end of a Codex or Claude command, or that later
   sandbox invocations can see the same temporary directory. Those observables
   need their own end-to-end tests in both real default profiles.

## Kata and RoboRev follow-up

Recent Kata and RoboRev changes confirm one immediate Kit defect and put useful
limits around the larger transport idea. Both applications observed a readable
runtime record whose live process could not be reached from a sandbox. Both had
to stop treating that state as daemon absence.

### Kata

[Kata PR #266](https://github.com/kenn-io/kata/pull/266), merged as
[`e62bb677`](https://github.com/kenn-io/kata/commit/e62bb6776e304684f33b2a5d8465793e3120e0ab),
added `ErrLocalDaemonUnreachable`. Discovery now retains the first probe error
for a runtime whose process identity is still live, continues looking for a
usable record, and returns the retained error only when no usable daemon wins.
The caller therefore does not auto-start a competing daemon merely because its
sandbox cannot dial the recorded endpoint
([discovery](https://github.com/kenn-io/kata/blob/e62bb6776e304684f33b2a5d8465793e3120e0ab/internal/client/ensure.go#L179-L218),
[error](https://github.com/kenn-io/kata/blob/e62bb6776e304684f33b2a5d8465793e3120e0ab/internal/client/client.go#L63-L117),
[tests](https://github.com/kenn-io/kata/blob/e62bb6776e304684f33b2a5d8465793e3120e0ab/internal/client/ensure_test.go#L205-L257)).
The same PR rejects definite process-identity mismatches before reporting,
selecting, or signaling a PID. This matters because an old runtime file can
refer to an unrelated process after PID reuse.

[Kata PR #273](https://github.com/kenn-io/kata/pull/273), merged as
[`abf39b23`](https://github.com/kenn-io/kata/commit/abf39b23eb06b8f3e8576b6e973b1669d16c9cf8),
added a runtime-directory write probe before spawning a detached child. A
permission failure now names the state directory, preserves `os.ErrPermission`,
mentions filesystem or sandbox access, and never starts the child
([implementation](https://github.com/kenn-io/kata/blob/abf39b23eb06b8f3e8576b6e973b1669d16c9cf8/internal/client/ensure.go#L281-L345),
[test](https://github.com/kenn-io/kata/blob/abf39b23eb06b8f3e8576b6e973b1669d16c9cf8/internal/client/ensure_test.go#L177-L200)).
Kit already absorbed the reusable part in
[Kit PR #72](https://github.com/kenn-io/kit/pull/72), merged as
[`eda6f084`](https://github.com/kenn-io/kit/commit/eda6f0848a53c634749192101654031f8d8e2956),
through `RuntimeStore.CheckWritable`. [Kata PR
#280](https://github.com/kenn-io/kata/pull/280), merged as
[`556def29`](https://github.com/kenn-io/kata/commit/556def2934b4132698c5e45d8c6890ed8dab6e03),
then deleted Kata's local copy and kept only its operator-facing wording. The
Kit contract correctly calls this preflight advisory. It does not prove that a
child has the same filesystem access or can bind its endpoint
([Kit implementation](../../daemon/runtime.go)).

[Kata PR #278](https://github.com/kenn-io/kata/pull/278), merged as
[`c31c2a87`](https://github.com/kenn-io/kata/commit/c31c2a875e18115a74557ef59dc94ad600e5a521),
added `kata daemon locate`. External clients can ask Kata to apply its actual
named-daemon, configured-remote, active-daemon, and local precedence, then
receive transport metadata without credentials
([contract](https://github.com/kenn-io/kata/blob/c31c2a875e18115a74557ef59dc94ad600e5a521/docs/reference/daemon-discovery.md#L19-L109)).
That command is a good consumer pattern. Its application-specific precedence
and output schema do not belong in Kit.

[Kata PR #281](https://github.com/kenn-io/kata/pull/281), merged as
[`e6356f8a`](https://github.com/kenn-io/kata/commit/e6356f8a2a18bf56f6e315c213de55432a964e6e),
added opt-in idle shutdown for auto-started daemons. It distinguishes
foreground activity that renews residency from finite drain work that merely
blocks exit. All shutdown sources enter one coordinator, close admission, and
share one absolute deadline. An explicit `kata daemon start` replaces an
idle-enabled auto-start process because explicit start promises a resident
daemon
([design](https://github.com/kenn-io/kata/blob/e6356f8a2a18bf56f6e315c213de55432a964e6e/docs/design/autostart-idle-shutdown.md#L14-L139),
[controller](https://github.com/kenn-io/kata/blob/e6356f8a2a18bf56f6e315c213de55432a964e6e/internal/daemon/idle_controller.go#L66-L366)).
This improves intentional residency and shutdown ordering. It does not make a
daemon survive sandbox teardown, and the policy is too application-specific to
move wholesale into Kit.

### RoboRev

[RoboRev PR #1021](https://github.com/kenn-io/roborev/pull/1021), merged as
[`97364377`](https://github.com/kenn-io/roborev/commit/97364377f1117261911b4a8b18386b0c6c189a8a),
fixed the same false-absence path reported in
[issue #1006](https://github.com/kenn-io/roborev/issues/1006). It introduced
`ErrDaemonAccessDenied`, recognizes wrapped `os.ErrPermission`, `EACCES`, and
`EPERM`, and keeps that result distinct from `os.ErrNotExist`
([classification and discovery](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/daemon/runtime.go#L293-L346)).
Ensure, cold start, version checks, zombie cleanup, and server startup all stop
when discovery is indeterminate. Tests verify that none of cleanup, restart,
or spawn runs after an access-denied probe
([lifecycle tests](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/cmd/roborev/daemon_lifecycle_test.go#L190-L335),
[cleanup test](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/daemon/runtime_test.go#L615-L637)).

The PR also lets a TCP daemon publish one best-effort Unix-socket alternate.
The socket lives in a private `0700` runtime directory, has mode `0600`, and
uses a data-directory hash in its service name so separate RoboRev data
directories do not collide
([listener](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/daemon/auxiliary_listener_unix.go#L17-L66)).
Discovery probes the primary followed by the alternate and requires the ping
PID to match the runtime PID. The daemon publishes the alternate only after it
accepts requests; failure of the alternate leaves the primary service intact
([server startup](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/daemon/server.go#L214-L320),
[tests](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/daemon/server_test.go#L481-L533)).
Explicit Unix configuration, systemd socket activation, and Windows remain
single-transport cases.

The alternate is a useful availability pattern, but it is not default-sandbox
compatibility. Codex and Claude may deny both loopback and Unix sockets. If
every endpoint is denied, RoboRev prints a sandbox-specific recovery message
and its Codex and Claude skills request each harness's native escalation while
forbidding a restart
([status](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/cmd/roborev/status.go#L42-L77),
[Codex wording](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/skills/codex/roborev-review/SKILL.md#L23-L28),
[Claude wording](https://github.com/kenn-io/roborev/blob/97364377f1117261911b4a8b18386b0c6c189a8a/internal/skills/claude/roborev-review/SKILL.md#L24-L29)).

[RoboRev PR #1036](https://github.com/kenn-io/roborev/pull/1036), merged as
[`94de16d9`](https://github.com/kenn-io/roborev/commit/94de16d907037469639fa6ca4bbb8eb7e831aeea),
tightened stop and replacement behavior. The daemon blocks new claims, drains
accepted work, and removes discovery metadata last. Lifecycle clients request
shutdown, then wait for the exact recorded PID to exit rather than using
endpoint failure as proof of exit. They do not force-kill an unresponsive live
process, and they remove only that process's runtime record because a service
manager may already have reused the endpoint
([client lifecycle](https://github.com/kenn-io/roborev/blob/94de16d907037469639fa6ca4bbb8eb7e831aeea/internal/daemon/runtime.go#L467-L579),
[server ordering](https://github.com/kenn-io/roborev/blob/94de16d907037469639fa6ca4bbb8eb7e831aeea/internal/daemon/server.go#L561-L637)).
The review-drain policy belongs to RoboRev, but exact-process exit and
ownership-aware cleanup are reusable daemon invariants.

[RoboRev PR #1059](https://github.com/kenn-io/roborev/pull/1059), merged as
[`850a0766`](https://github.com/kenn-io/roborev/commit/850a0766ff7e896c6acba98018b06ccc62e8c25b),
removed a second Agent Hook daemon and moved those routes onto the regular
daemon. The practical lesson is simple: when data and work already share one
owner, another daemon adds discovery, replacement, and persistence failure
modes without creating a useful isolation boundary. RoboRev deliberately did
not add a legacy takeover path; old auxiliary daemons must be stopped with the
old release.

### What should move into Kit next

The evidence is sufficient for one focused improvement: Kit discovery should
return a typed live-but-unreachable result. Current `Discover` discards every
probe error and reports no match
([source](../../daemon/probe.go)). `Manager.Ensure` already returns immediately
when `Find` returns an error, so preserving the error there prevents `Start`
without changing manager control flow
([source](../../daemon/manager.go)). The reusable contract is:

1. Reject a definite process-identity mismatch. Treat an identity that cannot
   be checked as indeterminate rather than using it to authorize cleanup or
   replacement.
2. Continue scanning after an unreachable live record so another usable record
   can win.
3. If none wins, return a typed error carrying the runtime record, endpoint,
   and wrapped probe error. Do not collapse it into absence or a boolean
   `running` value.
4. Never clean up, signal, restart, or auto-start solely because a probe was
   denied.

This behavior is intentionally opt-in through `RequirePIDAlive`. Without a
live-process check, a failed probe cannot distinguish an inaccessible daemon
from a stale runtime record. Keeping the false zero value also preserves the
existing behavior for callers that do not use process-aware discovery.

| PID checks | Recorded identity | Probe result | Discovery result when no later candidate wins |
| --- | --- | --- | --- |
| Disabled | Any | Failure | Definite absence, preserving the prior behavior |
| Enabled | Definite mismatch or dead PID | Not attempted | Definite absence |
| Enabled | Match or unknown | Failure | `UnreachableError` wrapping the probe failure |
| Enabled | Match or unknown | Success and accepted | Candidate returned |
| Enabled | Match or unknown | Success but rejected by `Accept` | Continue scanning, then definite absence |

Context cancellation takes precedence over a retained probe failure, and a
later reachable candidate takes precedence over every earlier failure. A probe
failure includes transport denial, a non-success HTTP response, malformed ping
data, service mismatch, and proof-of-possession failure. Applications should
use the wrapped error, not only `ErrDaemonUnreachable`, when writing recovery
guidance.

Ordered endpoint candidates are a plausible second Kit improvement. RoboRev
currently encodes its alternate in `RuntimeRecord.Metadata`, then reimplements
candidate parsing and probing. A structured candidate list would remove that
application convention and could later carry a FIFO endpoint. It should not
silently retry TCP after Unix failure, since default sandboxes may deny both.

`RuntimeStore.CheckWritable` should remain the pre-spawn filesystem check.
Application wording stays with callers. Kata's idle controller and RoboRev's
review drain should also remain caller-owned until another reusable package
contract emerges.

One smaller lifecycle gap remains in Kit. `StartDetached` launches a goroutine
that discards `cmd.Wait()` errors
([source](../../daemon/start.go)). A bounded child-exit signal during readiness
could preserve bind `EPERM`, configuration failures, and child-only permission
errors that the advisory write check cannot catch. The API shape needs care
because a successfully detached daemon is expected to outlive the starter.

### Remaining unknowns

- Neither Kata nor RoboRev added a FIFO or file-backed request transport. Both
  still use Unix sockets or TCP.
- Neither project runs an end-to-end test inside the real default Codex and
  Claude sandboxes.
- The inspected changes do not prove that a detached daemon survives sandbox
  teardown or remains discoverable in a later sandbox invocation.
- Vite Task proves related-process IPC within one sandboxed command. It does
  not settle cross-command daemon persistence.
- A write preflight does not prove child permissions or endpoint binding.
- RoboRev's alternate socket improves the chance of access only when the
  sandbox permits one of the two socket types.
