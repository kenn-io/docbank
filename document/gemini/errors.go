package gemini

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

var (
	ErrUnauthorized      = errors.New("gemini embed: provider authorization failed")
	ErrTransientResponse = errors.New("gemini embed: transient provider response")
	ErrCapacityResponse  = errors.New("gemini embed: provider capacity exceeded")
	ErrPermanentResponse = errors.New("gemini embed: permanent provider response")
)

type ProviderError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
	cause      error
}

func (failure *ProviderError) Error() string {
	if failure.StatusCode != 0 {
		return fmt.Sprintf("gemini embed: HTTP %d: %v", failure.StatusCode, failure.Kind)
	}
	return failure.Kind.Error()
}

// Unwrap preserves the classification and transport cause without exposing the
// potentially sensitive transport diagnostic through Error.
func (failure *ProviderError) Unwrap() []error {
	if failure.cause != nil {
		return []error{failure.Kind, failure.cause}
	}
	return []error{failure.Kind}
}

func RetryAfter(err error) (time.Duration, bool) {
	failure, ok := errors.AsType[*ProviderError](err)
	if !ok || !failure.RetrySet {
		return 0, false
	}
	return failure.RetryDelay, true
}

func statusError(status int, retryAfter string, now time.Time) error {
	kind := ErrPermanentResponse
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = ErrUnauthorized
	case status == http.StatusRequestEntityTooLarge:
		kind = ErrCapacityResponse
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599:
		kind = ErrTransientResponse
	}
	var delay time.Duration
	var set bool
	if errors.Is(kind, ErrTransientResponse) {
		delay, set = parseRetryAfter(retryAfter, now)
	}
	return &ProviderError{Kind: kind, StatusCode: status, RetryDelay: delay, RetrySet: set}
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return clampRetryDelay(seconds), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return clampRetryDuration(when.Sub(now)), true
}

func clampRetryDelay(seconds int64) time.Duration {
	if seconds > int64(time.Hour/time.Second) {
		return time.Hour
	}
	return time.Duration(seconds) * time.Second
}

func clampRetryDuration(delay time.Duration) time.Duration {
	if delay < 0 {
		return 0
	}
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}
