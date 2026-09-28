package zeroentropy

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
)

var (
	ErrTransientResponse = errors.New("zeroentropy embed: transient provider response")
	ErrCapacityResponse  = errors.New("zeroentropy embed: provider capacity exceeded")
	ErrPermanentResponse = errors.New("zeroentropy embed: permanent provider response")
)

type ProviderError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
}

func (failure *ProviderError) Error() string {
	if failure.StatusCode != 0 {
		return fmt.Sprintf("zeroentropy embed: HTTP %d: %v", failure.StatusCode, failure.Kind)
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
	delay, set := providerhttp.ParseRetryAfter(retryAfter, now)
	if !errors.Is(kind, ErrTransientResponse) {
		delay, set = 0, false
	}
	return &ProviderError{Kind: kind, StatusCode: status, RetryDelay: delay, RetrySet: set}
}
