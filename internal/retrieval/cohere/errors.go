package cohere

import (
	"errors"
	"time"

	"go.kenn.io/docbank/internal/cohereapi"
)

var (
	ErrTransientResponse = errors.New("cohere rerank: transient provider response")
	ErrCapacityResponse  = errors.New("cohere rerank: provider capacity exceeded")
	ErrPermanentResponse = errors.New("cohere rerank: permanent provider response")
)

type ProviderError struct {
	cohereapi.ResponseError
}

func RetryAfter(err error) (time.Duration, bool) {
	failure, ok := errors.AsType[*ProviderError](err)
	if !ok {
		return 0, false
	}
	return failure.RetryAfter()
}

func statusError(status int, value string, now time.Time) error {
	result := cohereapi.ClassifyStatus(status, value, now)
	return &ProviderError{ResponseError: result.ResponseError(status, ErrPermanentResponse, ErrTransientResponse, ErrCapacityResponse)}
}
