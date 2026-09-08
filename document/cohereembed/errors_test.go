package cohereembed

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseRetryAfterClampsIntegerSecondsBeforeDurationOverflow(t *testing.T) {
	values := []int64{math.MaxInt64/int64(time.Second) + 1, math.MaxInt64}
	for _, value := range values {
		delay, ok := RetryAfter(statusError(429, strconv.FormatInt(value, 10), time.Time{}))
		assert.True(t, ok)
		assert.Equal(t, time.Hour, delay)
	}
}
