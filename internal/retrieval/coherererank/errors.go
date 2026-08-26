package coherererank

import (
	"errors"
	"fmt"
	"time"

	"go.kenn.io/docbank/internal/cohereapi"
)

var (
	ErrTransientResponse = errors.New("cohere rerank: transient provider response")
	ErrCapacityResponse  = errors.New("cohere rerank: provider capacity exceeded")
	ErrPermanentResponse = errors.New("cohere rerank: permanent provider response")
)

type ProviderError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
}

func (failure *ProviderError) Error() string {
	if failure.StatusCode != 0 {
		return fmt.Sprintf("cohere rerank: HTTP %d: %v", failure.StatusCode, failure.Kind)
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

func statusError(status int, value string, now time.Time) error {
	result := cohereapi.ClassifyStatus(status, value, now)
	kind := ErrPermanentResponse
	switch result.Kind {
	case cohereapi.StatusPermanent:
	case cohereapi.StatusTransient:
		kind = ErrTransientResponse
	case cohereapi.StatusCapacity:
		kind = ErrCapacityResponse
	}
	return &ProviderError{Kind: kind, StatusCode: status,
		RetryDelay: result.RetryDelay, RetrySet: result.RetrySet}
}
