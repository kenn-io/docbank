package voyage

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
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

func TestRequestMetricsSumAttemptLatencyWithoutBackoff(t *testing.T) {
	policy, err := NewPolicy(PolicyConfig{Media: media.DefaultPolicy()})
	require.NoError(t, err)
	vector := make([]float32, policy.values.Dimension)
	vector[0] = 1
	payload, err := json.Marshal(map[string]any{
		"model": policy.values.Model,
		"data":  []map[string]any{{"embedding": vector, "index": 0}},
	})
	require.NoError(t, err)
	var calls atomic.Int32
	client := &Client{
		policy: policy, apiKey: "synthetic-key", maxRetries: 2, retryBaseDelay: time.Nanosecond,
		http: &http.Client{Transport: internalRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			status, body := http.StatusInternalServerError, []byte(nil)
			if calls.Add(1) == 2 {
				status, body = http.StatusOK, payload
			}
			return &http.Response{
				StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request,
			}, nil
		})},
	}
	times := []time.Time{
		time.Unix(0, 0), time.Unix(0, int64(10*time.Millisecond)),
		time.Unix(0, int64(1010*time.Millisecond)), time.Unix(0, int64(1020*time.Millisecond)),
	}
	var clockIndex atomic.Int32
	client.now = func() time.Time { return times[clockIndex.Add(1)-1] }

	_, _, metrics, err := client.embed(t.Context(), []wireInput{{Content: []wireContentPart{{Type: "text", Text: "x"}}}}, inputTypeDocument)
	require.NoError(t, err)
	assert.Equal(t, RequestMetrics{Requests: 2, Retries: 1, Latency: 20 * time.Millisecond}, metrics)
}

func TestCancellationAfterProviderWorkCarriesMetrics(t *testing.T) {
	policy, err := NewPolicy(PolicyConfig{Media: media.DefaultPolicy()})
	require.NoError(t, err)
	for _, duringBackoff := range []bool{false, true} {
		name := "request"
		if duringBackoff {
			name = "backoff"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			client := &Client{policy: policy, apiKey: "synthetic-key", maxRetries: 2, retryBaseDelay: time.Hour}
			client.http = &http.Client{Transport: internalRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				cancel()
				if !duringBackoff {
					return nil, context.Canceled
				}
				return &http.Response{
					StatusCode: http.StatusInternalServerError, Header: make(http.Header),
					Body: io.NopCloser(bytes.NewReader(nil)), Request: request,
				}, nil
			})}
			times := []time.Time{time.Unix(0, 0), time.Unix(0, int64(5*time.Millisecond))}
			var clockIndex atomic.Int32
			client.now = func() time.Time { return times[clockIndex.Add(1)-1] }

			_, _, _, err := client.embed(ctx, []wireInput{{Content: []wireContentPart{{Type: "text", Text: "x"}}}}, inputTypeDocument)
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, RequestMetrics{Requests: 1, Latency: 5 * time.Millisecond}, MetricsFromError(err))
		})
	}
}
