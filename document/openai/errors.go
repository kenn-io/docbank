package openai

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
)

var (
	// ErrUnauthorized identifies an unavailable or rejected API key.
	ErrUnauthorized = errors.New("openai: credential is unavailable or unauthorized")
	// ErrTransientResponse identifies retryable transport, 408, 429, and 5xx failures.
	ErrTransientResponse = errors.New("openai: transient provider response")
	// ErrCapacityResponse identifies request or response capacity failures.
	ErrCapacityResponse = errors.New("openai: provider capacity exceeded")
	// ErrPermanentResponse identifies non-retryable provider or schema failures.
	ErrPermanentResponse = errors.New("openai: permanent provider response")
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
		return fmt.Sprintf("openai: HTTP %d: %v", err.StatusCode, err.Kind)
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
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &ProviderError{Kind: ErrUnauthorized, StatusCode: status}
	case status == http.StatusRequestEntityTooLarge:
		return &ProviderError{Kind: ErrCapacityResponse, StatusCode: status}
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 599:
		delay, set := providerhttp.ParseRetryAfter(retryAfter, now)
		return &ProviderError{Kind: ErrTransientResponse, StatusCode: status, RetryDelay: delay, RetrySet: set}
	default:
		return &ProviderError{Kind: ErrPermanentResponse, StatusCode: status}
	}
}
