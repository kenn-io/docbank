# OpenSSH Package Threat Model

This package constructs OpenSSH commands, resolves effective configuration,
and manages persistent ControlMaster processes. It does not implement the SSH
protocol or replace the user's OpenSSH policy.

## Security goals

The package aims to ensure that:

- programmatic destination data cannot become local shell syntax through
  OpenSSH configuration-token expansion;
- rejected credential-bearing destination URIs do not expose passwords through
  returned errors;
- persistent clients use only a verified, same-user OpenSSH control socket in
  a private directory;
- persistent masters keep a positive server-alive failure threshold;
- the manager removes only socket paths that it owns and has positively shown
  to be stale or successfully terminated;
- cancellation, probe uncertainty, and cleanup failures remain detectable by
  callers; and
- arguments issued for one logical target cannot silently reach another
  target's authenticated master.

## Trust boundaries

### Trusted local policy and code

The local user account, selected OpenSSH executable, user-owned `ssh_config`,
known-hosts files, agent, askpass program, injected process runners, and caller
supplied base arguments are trusted. A process able to replace these inputs or
modify the caller's state already has the local authority this package is
intended to protect.

The package treats effective configuration returned by `ssh -G` as trusted
local policy. Direct `ProxyJump` targets are nevertheless parsed and validated
before the package invokes OpenSSH for those targets again. An explicit target
user and port precede trusted base arguments so OpenSSH's first-value option
precedence cannot resolve configuration for another account or endpoint.

### Programmatic destinations

`Target` values are untrusted data until `ValidateTarget` succeeds. Validation
happens before every package-owned OpenSSH invocation. Explicit users and host
aliases accept only ASCII letters, digits, `.`, `_`, and `-`, and may not begin
with `-`; exact `.` and `..` components are also rejected because OpenSSH may
expand them into local policy and credential paths. Hostnames may additionally
be valid IPv4 or IPv6 literals. Explicit ports must be in the OpenSSH port
range.

This deliberately excludes account-name forms whose punctuation could become
shell syntax after `%r` expansion. Callers needing an unusual account name
should put `User` in trusted `ssh_config` and pass a safe host alias as the
programmatic target. SSH URIs containing passwords are rejected without
including the original credential-bearing input in the error.

### Control sockets

The caller must place the manager's control directory below a parent directory
it trusts. The package creates or validates the control directory as private
and same-user, rejects symlinked directories, and verifies socket type and
ownership before probe, adoption, exit, or removal. These checks do not defend
against an attacker who can replace a trusted parent directory or act as the
same local user.

Persistent socket names bind both the caller's route identity and the validated
logical target and retain 128 bits of SHA-256 output in a compact filename. A
recorded target remains authoritative until its socket is successfully stopped,
absent, or positively stale; failed cleanup cannot make that socket adoptable
as another target. Failed-start cleanup keeps the lifecycle reserved for its
full cleanup window, including a final socket ownership/type inspection at
expiry, so a detached master that binds its socket late is still terminated and
drained before the manager releases ownership.

When a caller supplies a runner for one connection, the manager retains that
runner with the connection generation. Readiness checks, later liveness probes,
replacement teardown, explicit disconnect, and idle cleanup use the same
runner. The caller is responsible for keeping the runner and its captured
execution policy valid for the lifetime of the generation.

### Network and remote peers

The network is untrusted. Host authentication, transport confidentiality, and
credential exchange remain OpenSSH responsibilities under the user's policy.
The package does not treat a reachable endpoint as trusted and does not weaken
host-key checking.

## Caller responsibilities

Callers own:

- the environment and login-shell boundary used to run OpenSSH;
- host-key review, interactive authentication, and secret handling;
- choosing trusted OpenSSH configuration and base arguments;
- providing a control directory beneath a trusted parent; and
- touching activity immediately before using a persistent connection and
  tolerating a detectable reconnect if idle teardown has already begun.

## Out of scope

The package does not defend against a compromised local account, malicious
user-owned SSH configuration, a replaced OpenSSH executable, a hostile
injected runner, or a caller that deliberately passes unsafe options through
trusted base arguments. It does not guarantee availability or safe application
behavior after OpenSSH authenticates a malicious remote peer.
