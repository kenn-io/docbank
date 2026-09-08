package openaihosted

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrTransientResponse identifies retryable transport, 408, 429, and 5xx failures.
	ErrTransientResponse = errors.New("openaihosted: transient provider response")
	// ErrCapacityResponse identifies request or response capacity failures.
	ErrCapacityResponse = errors.New("openaihosted: provider capacity exceeded")
	// ErrPermanentResponse identifies non-retryable provider or schema failures.
	ErrPermanentResponse = errors.New("openaihosted: permanent provider response")
)

// ProviderError contains only stable classification and bounded retry metadata.
type ProviderError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
}

func (err *ProviderError) Error() string {
	if err.StatusCode != 0 {
		return fmt.Sprintf("openaihosted: HTTP %d: %v", err.StatusCode, err.Kind)
	}
	return err.Kind.Error()
}

func (err *ProviderError) Unwrap() error { return err.Kind }

// RetryAfter returns bounded provider retry guidance when one was valid.
func RetryAfter(err error) (time.Duration, bool) {
	providerErr, ok := errors.AsType[*ProviderError](err)
	if !ok || !providerErr.RetrySet {
		return 0, false
	}
	return providerErr.RetryDelay, true
}

func statusError(status int, retryAfter string, now time.Time) error {
	switch {
	case status == http.StatusRequestEntityTooLarge:
		return &ProviderError{Kind: ErrCapacityResponse, StatusCode: status}
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599:
		delay, set := parseRetryAfter(retryAfter, now)
		return &ProviderError{Kind: ErrTransientResponse, StatusCode: status, RetryDelay: delay, RetrySet: set}
	default:
		return &ProviderError{Kind: ErrPermanentResponse, StatusCode: status}
	}
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds >= int64(time.Hour/time.Second) {
			return time.Hour, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(when.Sub(now), 0), time.Hour), true
}
