# PostHog

The reporter owns event admission, property filtering, identity and default
properties; HTTP and direct capture share Report. HTTP validates required
properties before enabled state. Capture-specific event rejection follows
method, content-type, bounded decoding and trailing-data checks, before Report.
The handler requires application/json so browsers preflight cross-origin posts,
and it bounds the body itself rather than trusting the caller's router.
Disabled construction keeps the configured allowlist without creating a client.
Kit's source never embeds a project key. Options.APIKey wins; when it is empty
the reporter uses projectKey, which release builds set at link time. With
neither, construction is disabled, so source builds and tests report nothing.
Reporting starts on. A trimmed, case-insensitive 0, false, no, or off in
either TELEMETRY_ENABLED or its prefixed equivalent disables reporting.
The version must be plain vX.Y.Z or X.Y.Z. Process disable, any present
CI, and a present DO_NOT_TRACK except trimmed, case-insensitive 0 or false
veto reporting. Forwarding clients call Eligible(envPrefix, version)
before each send.
Admission runs before client configuration validation; invalid event declarations
still return errors.
Close waits at most ShutdownTimeout.

The transport removes `$os`, `$os_version`, `$os_distro`, and `$go_version`
after serialization on every upload, including redirect replays. Keep fields.md
and the serialized-payload test in sync with that contract.

The package imports no OpenTelemetry, so products that only report events stay
small; TestPackageLinksNoOpenTelemetry guards this. Applications own route
registration, authentication, where the install file lives, when the
heartbeat runs and reporter shutdown.

Callers own send timeouts and choose where to wait during shutdown. SendGroup
bounds that wait while unfinished sends continue until completion or process exit.

The heartbeat checks wall-clock UTC dates hourly so system sleep does not turn
daily activity into a count of awake hours. Disabled clients start no ticker;
running heartbeats stop at the next hourly check after disabling, so
Client.Enabled must not return true after returning false. SDK logs and
daily-claim warnings use the supplied slog logger (or slog.Default) tagged
component=posthog, preserving levels and the application's log sink.

Daily claims hold a context-aware instance semaphore and file lock through durable
reservation, enqueue, and settlement, and sample time under the lock. Retain the
35 most recently accepted UTC dates per key in reservation order; unresolved
reservations carry v1-compatible owner markers that never count toward the 35.
Compaction retires a marker in the same pass as its date, so a stale token-aware
owner's cleanup cannot delete a reused date. Failed skip compaction warns and returns
StatusSkipped; failed acceptance settlement warns and returns StatusQueued, and
the instance's next call retries it. Other storage failures return errors.
See daily.md for the persisted format, version handling, recovery rules and the
mixed-writer upgrade caveat. Applications own finite property enums and client
retries. Queue acceptance does not guarantee network delivery.
CaptureConfirmed confirms non-daily events only on (StatusDelivered, nil);
(StatusDisabled, nil) leaves completion pending. Callers persist and reuse the
delivery UUID, timestamp, event and distinct ID. Callbacks match all four, and
Close resolves every pending delivery after its drain, so a flush refused by
Close still waits for the drained result. The context bounds the wait, not SDK
retries; flush may send other queued events.
