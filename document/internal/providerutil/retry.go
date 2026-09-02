package providerutil

import (
	"errors"
	"time"

	"go.kenn.io/docbank/document"
)

// RetryDelay returns how long a polling loop should wait before retrying
// after err: the provider's Retry-After hint when it is longer than the
// configured interval, otherwise the interval. Ignoring the hint turns one
// rate-limit answer into a run of them and burns the polling budget.
func RetryDelay(err error, interval time.Duration) time.Duration {
	if providerError, ok := errors.AsType[*document.RenditionProviderError](err); ok {
		return max(interval, providerError.RetryAfter())
	}
	return interval
}
