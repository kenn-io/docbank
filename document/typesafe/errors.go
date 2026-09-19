package typesafe

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.kenn.io/docbank/document/internal/providerutil"
)

var (
	ErrTransientResponse = errors.New("typesafe rerank: transient provider response")
	ErrCapacityResponse  = errors.New("typesafe rerank: provider capacity exceeded")
	ErrPermanentResponse = errors.New("typesafe rerank: permanent provider response")
)

type ProviderError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
}

func (failure *ProviderError) Error() string {
	if failure == nil || failure.Kind == nil {
		return "typesafe rerank: provider error"
	}
	if failure.StatusCode != 0 {
		return fmt.Sprintf("typesafe rerank: HTTP %d: %v", failure.StatusCode, failure.Kind)
	}
	return failure.Kind.Error()
}

func (failure *ProviderError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Kind
}

func RetryAfter(err error) (time.Duration, bool) {
	failure, ok := errors.AsType[*ProviderError](err)
	if !ok || failure == nil || !failure.RetrySet {
		return 0, false
	}
	return failure.RetryDelay, true
}

func statusError(status int, header http.Header) error {
	kind := ErrPermanentResponse
	switch {
	case status == http.StatusRequestEntityTooLarge:
		kind = ErrCapacityResponse
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		kind = ErrTransientResponse
	}
	delay := providerutil.ParseRetryAfter(header)
	return &ProviderError{Kind: kind, StatusCode: status, RetryDelay: delay,
		RetrySet: delay != 0 && errors.Is(kind, ErrTransientResponse)}
}
