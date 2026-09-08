package cohereapi

import (
	"fmt"
	"time"
)

// ResponseError carries an adapter-owned error identity and bounded retry advice.
type ResponseError struct {
	Kind       error
	StatusCode int
	RetryDelay time.Duration
	RetrySet   bool
}

func (failure *ResponseError) Error() string {
	if failure.StatusCode != 0 {
		return fmt.Sprintf("%v (HTTP %d)", failure.Kind, failure.StatusCode)
	}
	return failure.Kind.Error()
}

func (failure *ResponseError) Unwrap() error { return failure.Kind }

func (failure *ResponseError) RetryAfter() (time.Duration, bool) {
	if !failure.RetrySet {
		return 0, false
	}
	return failure.RetryDelay, true
}

// ResponseError applies this HTTP classification to an adapter's error identities.
func (result StatusResult) ResponseError(status int, permanent, transient, capacity error) ResponseError {
	kind := permanent
	switch result.Kind {
	case StatusPermanent:
	case StatusTransient:
		kind = transient
	case StatusCapacity:
		kind = capacity
	}
	return ResponseError{Kind: kind, StatusCode: status, RetryDelay: result.RetryDelay, RetrySet: result.RetrySet}
}
