package voyage

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrUnauthorized marks a provider 401 or 403; the credential, not the
	// input, is wrong.
	ErrUnauthorized = errors.New("voyage embedding authorization failed")
	// ErrBatchTooLarge marks a request refused locally by policy limits or by
	// the provider for size; splitting the batch may succeed.
	ErrBatchTooLarge = errors.New("voyage embedding request too large")
	// ErrPermanentResponse marks a provider 4xx other than rate limiting,
	// authorization, or size.
	ErrPermanentResponse = errors.New("voyage embedding permanent response")
	// ErrTransientResponse marks an exhausted retryable provider or transport
	// failure.
	ErrTransientResponse = errors.New("voyage embedding transient response")
	// ErrMalformedResponse marks a provider response that could not be
	// validated; it is retried once because it may be transient corruption.
	ErrMalformedResponse = errors.New("voyage embedding malformed response")
	// ErrCapabilityContract marks input that no supplied authorization covers.
	ErrCapabilityContract = errors.New("voyage embedding input lacks capability authorization")
	// ErrInvalidInput marks input that violates the request shape or the
	// policy media bounds before any request is made.
	ErrInvalidInput = errors.New("voyage embedding input is invalid")
)

// RequestMetrics describes actual provider HTTP work for one logical request.
type RequestMetrics struct {
	Requests int
	Retries  int
	Latency  time.Duration
}

// ProviderError carries the classification and accounting for a failed
// request. Error strings never include provider response bodies.
type ProviderError struct {
	Kind       error
	StatusCode int
	RetryAfter time.Duration
	RetrySet   bool
	Metrics    RequestMetrics
	cause      error
}

func (e *ProviderError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("voyage embedding HTTP %d: %v", e.StatusCode, e.Kind)
	}
	return fmt.Sprintf("voyage embedding: %v", e.Kind)
}

// Unwrap exposes the kind and, when present, the transport cause.
func (e *ProviderError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.Kind, e.cause}
	}
	return []error{e.Kind}
}

// IsRetryable reports whether err is a transient provider or transport
// failure an application can schedule again later.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrTransientResponse)
}

// RetryAfter returns the provider's Retry-After delay when the failure carried
// one.
func RetryAfter(err error) (time.Duration, bool) {
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || !providerErr.RetrySet {
		return 0, false
	}
	return providerErr.RetryAfter, true
}

// MetricsFromError recovers provider request accounting from an embedding
// error.
func MetricsFromError(err error) RequestMetrics {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Metrics
	}
	return RequestMetrics{}
}

const maxRetryAfter = time.Hour

// parseRetryAfter accepts integer seconds or an HTTP date, clamped to one
// hour. Invalid values report false so the caller falls back to backoff.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(when.Sub(now), 0), maxRetryAfter), true
}
