# Daily screen reports

Configure a reporter with the application's screen names and a private state
path whose parent directory already exists:

```go
posthog.WithAllowedEvent("screen_viewed",
    posthog.RequireProperty("screen", posthog.AllowStringValues("queue", "review"))),
posthog.WithDailyEvent("screen_viewed", "screen", posthog.NewDailyClaims(statePath)),
```

`Options.DistinctID` scopes claims to an installation. HTTP and `Report` reject
invalid properties before claiming. The handler requires `application/json`,
one JSON value and a body of at most 64 KiB. Applications own routes and
authentication.

Keep daily event and key allowlists finite. Reporter construction rejects an
unknown daily event, an unlisted key property or missing state path, including
when opted out. `Report` and enabled `Capture` return `ErrUnsupportedEvent`
for blank events.
Daily keys must be caller-owned properties; reporter defaults cannot identify a daily claim.

The state file reserves each installation, event and screen before SDK enqueue.
The in-process semaphore and file lock cover reservation, enqueue and settlement.
Rejected enqueues release their reservation. Failed reservation saves release
published claims; errors include both failures if cleanup fails before publication.
After acceptance, settlement saves again. A failed save warns and returns `queued`;
the same `DailyClaims` instance retries settlement on its next call, which
returns an error without reporting if settlement fails again.
Each key keeps its 35 most recently accepted UTC dates in reservation order,
even when another writer accepts later dates before settlement retries. Retained
dates skip after clock corrections; older dates can count again. Sparse activity
can retain dates older than 35 calendar days. Unresolved reservations add a
four-string JSON key containing the installation, event, screen, and owner token,
with a one-date list as its value. Tokens protect replacement claims from stale
cleanup by token-aware writers. Reports compact every key, including on skips;
a failed skip-path save warns and returns `skipped`. Other storage failures
return errors. Reservation, event timestamp and installation age use one time
sampled under the claim lock.

Use `Report` with a caller context. Reports on the same `DailyClaims` instance
wait on an in-process semaphore before the file lock. Both waits honor the
caller's context; `Capture` bounds them with `ShutdownTimeout`. Version 1 date
lists remain readable and writable by released writers.

Upgrade all writers sharing a claims path together. If an old writer's rejected
report leaves cleanup pending, 35 later acceptances can retire its date. After a
clock rollback reuses that date, the old cleanup can delete the new claim and
allow another count.

A numeric version that may be 1 or more but isn't written as `1`, for example
`2` or `1.0`, is refused unchanged. Other unreadable content resets to empty
claims. Read errors other than a missing file are returned.
Request lock waits follow caller cancellation and deadlines; HTTP owners should bound request contexts as needed.
A report racing `Close` can hold the in-process semaphore and daily file lock
while waiting for SDK shutdown, up to `ShutdownTimeout`. Other reports on the
same instance wait on the semaphore with the same context bound, so they and
other writers can time out and retry.
Recovering damaged state forgets accepted dates and can count them again.

Accepted reports mean SDK queue acceptance; crashes before acceptance or failed
network delivery can lose a count. Unresolved reservations dedupe until their
owner retries or 35 later known acceptances retire them. New dates can report
throughout recovery. Repeated failures can grow unresolved history; the 35-date
bound applies to known accepted dates. Replacing the file forgets all claims.

Report screens on later focus, navigation or terminal interaction,
independently of app-open reporting. Use delivery results to detect failures
and retry on later interaction. Report the initial terminal queue and use its
acceptance result. A split pane showing queue
and review reports both screen keys. Daily deduplication belongs to the server.
`queued`, `skipped` and `disabled` complete the current attempt; retry errors on later interaction.

When adopting this API, remove copied daily gates, use `Report` or `PostEvent`
results for retries, and transfer existing same-day claims if upgrade continuity
is required. Before each `PostEvent` send, check `Eligible(prefix, version)` with
the forwarding process's prefix and version. Product adoption belongs in each
application's own change.

Require a session duration through the existing caller-owned filter:

```go
posthog.WithAllowedEvent("session_ended",
    posthog.RequireProperty("duration_bucket", posthog.AllowStringValues(
        "under_1m", "1_to_5m", "5_to_30m", "over_30m", "30m_to_2h", "over_2h"))),
```

Older tabs can still send `over_30m`, meaning more than thirty minutes with no
further detail. Preserve that meaning alongside current buckets. Missing or
unsupported required durations return an error before enqueue.
