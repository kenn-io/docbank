package embedclient

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/cenkalti/backoff/v7"
)

// Retry configures retries of failures that may succeed later. The zero value
// does not retry. Only a retryable APIError (408, 429, or 5xx) and a
// TransportError are retried; an input or credential rejection never is. Each
// retry waits for the backoff, or for the provider's Retry-After when it sends
// one, and stops when the call's context ends.
type Retry struct {
	// MaxAttempts is the total number of tries, including the first. Values
	// of one or less do not retry.
	MaxAttempts int
	// InitialBackoff is the wait before the first retry. Later retries grow
	// it exponentially with jitter. Zero uses 500ms.
	InitialBackoff time.Duration
	// MaxBackoff caps the growing backoff before jitter, so one wait can
	// reach 1.5 times this value. Zero uses 30s.
	MaxBackoff time.Duration
	// MaxRetryAfter caps how long a provider's Retry-After can make a retry
	// wait. Zero uses 60s.
	MaxRetryAfter time.Duration
}

// do runs call until it succeeds, fails with an error that a retry cannot
// fix, runs out of attempts, or ctx ends.
func (r Retry) do(ctx context.Context, call func() ([][]float32, error)) ([][]float32, error) {
	if r.MaxAttempts <= 1 {
		return call()
	}
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = cmp.Or(r.InitialBackoff, 500*time.Millisecond)
	policy.MaxInterval = cmp.Or(r.MaxBackoff, 30*time.Second)
	maxRetryAfter := cmp.Or(r.MaxRetryAfter, 60*time.Second)

	vectors, err := backoff.Retry(ctx, func() ([][]float32, error) {
		vectors, err := call()
		if err == nil {
			return vectors, nil
		}
		if apiErr, ok := errors.AsType[*APIError](err); ok {
			if !apiErr.Retryable() {
				return nil, backoff.Permanent(err)
			}
			if apiErr.RetryAfter > 0 {
				return nil, backoff.RetryAfter(min(apiErr.RetryAfter, maxRetryAfter), err)
			}
			return nil, err
		}
		if _, ok := errors.AsType[*TransportError](err); ok {
			return nil, err
		}
		return nil, backoff.Permanent(err)
	},
		backoff.WithBackOff(policy),
		backoff.WithMaxTries(uint(r.MaxAttempts)),
		backoff.WithMaxElapsedTime(0),
	)
	// Report the provider's error as the call would without retries, joined
	// with the context error when the context ended the retries.
	if retryErr := backoff.AsRetryError(err); retryErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Join(retryErr.LastErr, ctxErr)
		}
		return nil, retryErr.LastErr
	}
	return vectors, err
}
