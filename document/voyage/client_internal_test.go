package voyage

import (
	"context"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

type internalRoundTripFunc func(*http.Request) (*http.Response, error)

func (f internalRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestOversizedQueryIsRejectedBeforeBase64Allocation(t *testing.T) {
	policy, err := NewPolicy(PolicyConfig{Media: media.DefaultPolicy(), MaxRequestBytes: 1024})
	require.NoError(t, err)
	var calls atomic.Int32
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: internalRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, assert.AnError
		})},
	})
	require.NoError(t, err)
	data := append(mediatest.JPEG(1, 1, nil), make([]byte, 8<<20)...)
	metadata, err := media.DetectBytes(data, "")
	require.NoError(t, err)
	capability, ok := CapabilityByID(CapabilityQueryImageJPEG)
	require.True(t, ok)
	authorization := Authorization{capability: capability, policyDigest: policy.digest}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err = client.EmbedQuery(context.Background(), Input{Parts: []Part{{Media: &Media{Metadata: metadata, Bytes: data}}}}, authorization)
	runtime.ReadMemStats(&after)

	require.ErrorIs(t, err, ErrBatchTooLarge)
	assert.Zero(t, calls.Load())
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(4<<20), "rejection must not allocate the base64 request body")
}

func TestRetryBackoffDelaySaturates(t *testing.T) {
	assert.Equal(t, 100*time.Millisecond, retryBackoffDelay(100*time.Millisecond, 1))
	assert.Equal(t, 200*time.Millisecond, retryBackoffDelay(100*time.Millisecond, 2))
	assert.Equal(t, maxRetryAfter, retryBackoffDelay(maxRetryAfter, MaxRetries))
}
