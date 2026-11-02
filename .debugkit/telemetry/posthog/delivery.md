# Confirmed milestone delivery

Use `CaptureConfirmed` for a one-time, non-daily milestone that the application
records only after PostHog accepts it:

```go
delivery := posthog.Delivery{ID: uuid.New(), Time: time.Now()}
// Persist delivery, then send it.
status, err := reporter.CaptureConfirmed(ctx, "setup_completed", nil, delivery)
if err == nil && status == posthog.StatusDelivered {
    // Record the milestone as complete.
}
```

Only `(StatusDelivered, nil)` confirms completion. `(StatusDisabled, nil)` and
every error leave the milestone pending. On the next attempt, reuse the
persisted `Delivery` with the same event and `Options.DistinctID`: a timed-out
or cancelled send may still reach PostHog, which drops the duplicate.

The context deadline bounds the wait, not the SDK's retries. Flushing also
sends other queued events. Daily events return `ErrUnsupportedEvent`.
