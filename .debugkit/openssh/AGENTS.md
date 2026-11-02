# OpenSSH Package Invariants

- This package orchestrates the caller's OpenSSH executable. Do not replace
  system `ssh_config`, agent, known-hosts, askpass, or multiplexing behavior
  with a Go-native SSH protocol implementation.
- Keep process launching injectable. Callers own environment and login-shell
  policy; the package owns argument construction and connection lifecycle.
- Keep ControlMaster startup noninteractive by default. A caller may opt into
  interaction only when its bound runner owns a bounded terminal or askpass
  policy; the package does not supply or discover an interaction channel.
- Let callers bound detached-master survival with `ControlPersistTimeout`.
  Zero preserves explicit-lifecycle-only behavior; reject negative durations.
- Bind an explicitly supplied runner to the ControlMaster generation it starts
  or adopts. Use that runner for every later probe and teardown of the same
  generation; never switch execution policy underneath a live master.
- Treat programmatic `Target` values as untrusted until `ValidateTarget`
  succeeds before every OpenSSH invocation. Unusual account names belong in
  trusted `ssh_config` behind a safe host alias; do not weaken the explicit
  target allowlist to mirror OpenSSH's version-dependent filtering.
- Keep IPv6 brackets in display and URI-style target strings, but pass raw IPv6
  hostnames to OpenSSH positional destinations because ports use a separate
  argument.
- A missing control path is a supported command-construction mode, not an
  instruction to create or discover an implicit master.
- Put target-specific resolver user and port options before caller base
  arguments, with a host-only positional destination, so explicit destination
  policy wins OpenSSH's first-value option precedence. Reject custom connection
  options with a nonpositive server-alive count.
- Persistent ControlMaster ownership is Unix-only. Keep masterless command
  construction portable and return `ErrPersistentUnsupported` elsewhere.
- Control directories must sit below a caller-trusted parent. Reuse
  `safefileio.EnsurePrivateDir`; verify socket type and ownership before probe,
  adoption, exit, or removal.
- Never unlink on an indeterminate probe or hold a lifecycle mutex across
  filesystem, subprocess, cleanup, or waiter I/O.
- Bind persistent socket names to both route identity and validated target.
  Retain at least 128 bits of their cryptographic digest in the socket name.
  Keep a recorded target authoritative in every state until successful
  teardown clears it; never probe or start a replacement target first.
- Keep teardown reserved after a successful exit command until the socket is
  absent or positively stale; only the stale outcome authorizes removal. If
  draining fails after exit acknowledgment, retain the target in an unavailable
  stopping state and never probe or adopt that socket as a live master. Apply
  this quarantine to ordinary teardown, failed-start cleanup, and repeated
  disconnects, which must drain without sending another exit command.
- Keep failed-start cleanup reserved for its full cleanup window while the
  socket is absent, including a final ownership/type inspection when that
  window expires, even when the expiring window cut a socket dial short. A
  detached master may bind late; if it does, terminate and drain it through
  the normal verified teardown path, with its own cleanup window, before
  releasing the lifecycle.
- Bind unlocked results to the per-identity generation. Waiters always reread
  state after the in-flight completion channel closes.
- Require the expected generation when returning client connection arguments,
  and derive the socket path from the same locked connection snapshot.
- Probe liveness only while the expected generation remains active and has no
  reserved operation, checking those conditions before and after unlocked I/O.
- Bound the entire teardown operation, including the exit subprocess, by the
  cleanup timeout. Report teardown failures as error events while retaining
  the authoritative target and socket state for recovery.
- Dispatch events outside reserved lifecycle operations so blocking or
  reentrant callbacks cannot stall SSH work. Drop queued events whose
  generation has been replaced, and coalesce duplicate states so a blocked
  callback cannot grow the per-host queue without bound.
- Stop idle scans promptly when their context is canceled, including a final
  check under the entry lock before reserving teardown.
- Resolve persistent control directories to absolute paths at manager
  construction. Pass control socket paths through `-S`, double `%` for literal
  OpenSSH token handling, reject environment expansion, encode the reserved
  `none` value as an explicit relative path, and prevent leading `~` expansion
  so SSH and local filesystem operations address the same entry.
- Keep remote product protocols and shell fragments out of this package.
