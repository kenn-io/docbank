package geminiembed

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusErrorClassifiesGeminiResponses catches retryable, capacity, and
// permanent provider failures being collapsed into one unsafe retry policy.
func TestStatusErrorClassifiesGeminiResponses(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		status     int
		retryAfter string
		want       error
		delay      time.Duration
		delaySet   bool
	}{
		{name: "timeout", status: http.StatusRequestTimeout, want: ErrTransientResponse},
		{name: "rate limit", status: http.StatusTooManyRequests, retryAfter: "2", want: ErrTransientResponse, delay: 2 * time.Second, delaySet: true},
		{name: "capacity", status: http.StatusRequestEntityTooLarge, want: ErrCapacityResponse},
		{name: "invalid request", status: http.StatusBadRequest, want: ErrPermanentResponse},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := statusError(testCase.status, testCase.retryAfter, time.Unix(0, 0))
			require.ErrorIs(t, err, testCase.want)
			delay, set := RetryAfter(err)
			assert.Equal(t, testCase.delaySet, set)
			assert.Equal(t, testCase.delay, delay)
		})
	}
}
