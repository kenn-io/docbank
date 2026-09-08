package embeddingbridge

import (
	"errors"
	"fmt"
)

// ErrorCategory is a stable, content-free worker classification.
type ErrorCategory string

const (
	ErrorAuthentication      ErrorCategory = "authentication"
	ErrorCapacity            ErrorCategory = "capacity"
	ErrorTransient           ErrorCategory = "transient"
	ErrorPermanent           ErrorCategory = "permanent"
	ErrorMalformedResponse   ErrorCategory = "malformed_response"
	ErrorAmbiguousSubmission ErrorCategory = "ambiguous_submission"
	ErrorSourceChanged       ErrorCategory = "source_changed"
)

// ProviderError contains only a stable category and optional HTTP status.
// Provider bodies, transport messages, secrets, and inputs are never retained.
type ProviderError struct {
	category ErrorCategory
	status   int
}

func (err *ProviderError) Error() string {
	if err.status != 0 {
		return fmt.Sprintf("embedding bridge: %s (HTTP %d)", err.category, err.status)
	}
	return "embedding bridge: " + string(err.category)
}

// Category returns the stable category for err. Unknown errors are permanent.
func Category(err error) ErrorCategory {
	if providerError, ok := errors.AsType[*ProviderError](err); ok {
		return providerError.category
	}
	return ErrorPermanent
}

// IsRetryable reports whether retrying the same idempotency checksum is safe.
func IsRetryable(err error) bool {
	category := Category(err)
	return category == ErrorTransient || category == ErrorAmbiguousSubmission
}

func classified(category ErrorCategory, status int) error {
	return &ProviderError{category: category, status: status}
}
