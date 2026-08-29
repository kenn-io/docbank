package geminiembed

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

var (
	ErrTransientResponse = errors.New("gemini embed: transient provider response")
	ErrCapacityResponse  = errors.New("gemini embed: provider capacity exceeded")
	ErrPermanentResponse = errors.New("gemini embed: permanent provider response")
)

type ProviderError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
}

func (failure *ProviderError) Error() string {
	if failure.StatusCode != 0 {
		return fmt.Sprintf("gemini embed: HTTP %d: %v", failure.StatusCode, failure.Kind)
	}
	return failure.Kind.Error()
}

func (failure *ProviderError) Unwrap() error { return failure.Kind }

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
	case status == http.StatusRequestEntityTooLarge:
		kind = ErrCapacityResponse
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599:
		kind = ErrTransientResponse
	}
	delay, set := parseRetryAfter(retryAfter, now)
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
