package providerutil

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/docbank/document"
)

func TestRetryDelayPrefersTheLongerOfHintAndInterval(t *testing.T) {
	hinted := ClassifiedError("synthetic", document.RenditionErrorRateLimited, "slow down", 3*time.Second, nil)
	assert.Equal(t, 3*time.Second, RetryDelay(hinted, time.Second))
	assert.Equal(t, 5*time.Second, RetryDelay(hinted, 5*time.Second),
		"a hint shorter than the configured interval does not speed polling up")
	assert.Equal(t, time.Second, RetryDelay(errors.New("plain"), time.Second))
	assert.Equal(t, time.Second, RetryDelay(nil, time.Second))
}
