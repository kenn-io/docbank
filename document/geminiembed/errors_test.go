package geminiembed

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusErrorClassifiesResponsesAndBoundsRetryHints(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		kind       error
		delay      time.Duration
		set        bool
	}{
		{name: "timeout", status: http.StatusRequestTimeout, kind: ErrTransientResponse},
		{name: "rate limit", status: http.StatusTooManyRequests, retryAfter: "2", kind: ErrTransientResponse, delay: 2 * time.Second, set: true},
		{name: "bounded delay", status: http.StatusServiceUnavailable, retryAfter: "99999", kind: ErrTransientResponse, delay: time.Hour, set: true},
		{name: "capacity", status: http.StatusRequestEntityTooLarge, kind: ErrCapacityResponse},
		{name: "bad request", status: http.StatusBadRequest, kind: ErrPermanentResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := statusError(test.status, test.retryAfter, time.Unix(0, 0))
			require.ErrorIs(t, err, test.kind)
			delay, set := RetryAfter(err)
			assert.Equal(t, test.set, set)
			assert.Equal(t, test.delay, delay)
		})
	}
}

func TestRetryAfterSuppressesHintForUnconfirmedRetention(t *testing.T) {
	transient := &ProviderError{Kind: ErrTransientResponse, StatusCode: http.StatusServiceUnavailable, RetryDelay: time.Second, RetrySet: true}
	err := errors.Join(transient, ErrRemoteRetentionUnconfirmed)
	require.ErrorIs(t, err, ErrTransientResponse)
	require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	_, set := RetryAfter(err)
	assert.False(t, set)
}
