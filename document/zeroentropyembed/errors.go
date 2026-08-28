package zeroentropyembed

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	delay, set := parseRetryAfter(retryAfter, now)
	if !errors.Is(kind, ErrTransientResponse) {
		delay, set = 0, false
	}
	return &ProviderError{Kind: kind, StatusCode: status, RetryDelay: delay, RetrySet: set}
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
