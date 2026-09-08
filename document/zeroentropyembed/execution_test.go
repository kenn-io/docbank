package zeroentropyembed

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

const synchronizationWatchdog = 5 * time.Second

func TestEmbedFloatVectorRejectsNullButAcceptsZero(t *testing.T) {
	validVector := strings.TrimSuffix(strings.Repeat("0,", 40), ",")
	for _, test := range []struct {
		name       string
		coordinate string
		wantError  bool
	}{
		{name: "null", coordinate: "null", wantError: true},
		{name: "numeric zero", coordinate: "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vector := strings.Replace(validVector, "0", test.coordinate, 1)
			body := `{"results":[{"embedding":[` + vector + `]}],"usage":{"total_bytes":155,"total_tokens":2}}`
			profile := testProfile(t, 40, EncodingFloat, LatencyFast)
			client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(body)), nil
			}))

			result, err := client.Embed(context.Background(), oneInput(), authorization(client.descriptor, 1))
			if test.wantError {
				require.ErrorIs(t, err, ErrPermanentResponse)
				assert.Empty(t, result.Vectors)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Vectors, 1)
			assert.Zero(t, result.Vectors[0].Values[0])
		})
	}
}

func TestEmbedRejectsExtendedResponseDrift(t *testing.T) {
	validVector := strings.TrimSuffix(strings.Repeat("0,", 40), ",")
	validResult := `[{"embedding":[` + validVector + `]}]`
	validUsage := `{"total_bytes":155,"total_tokens":2}`
	base64NaN := make([]byte, 40*4)
	binary.LittleEndian.PutUint32(base64NaN, math.Float32bits(float32(math.NaN())))
	tests := []struct {
		name     string
		encoding EncodingFormat
		body     string
	}{
		{name: "null total bytes", encoding: EncodingFloat, body: `{"results":` + validResult + `,"usage":{"total_bytes":null,"total_tokens":2}}`},
		{name: "missing total bytes", encoding: EncodingFloat, body: `{"results":` + validResult + `,"usage":{"total_tokens":2}}`},
		{name: "null total tokens", encoding: EncodingFloat, body: `{"results":` + validResult + `,"usage":{"total_bytes":155,"total_tokens":null}}`},
		{name: "missing total tokens", encoding: EncodingFloat, body: `{"results":` + validResult + `,"usage":{"total_bytes":155}}`},
		{name: "duplicate results", encoding: EncodingFloat, body: `{"results":` + validResult + `,"results":` + validResult + `,"usage":` + validUsage + `}`},
		{name: "trailing response data", encoding: EncodingFloat, body: `{"results":` + validResult + `,"usage":` + validUsage + `}{}`},
		{name: "invalid base64 byte length", encoding: EncodingBase64, body: `{"results":[{"embedding":"` + base64.StdEncoding.EncodeToString(make([]byte, 40*4-1)) + `"}],"usage":` + validUsage + `}`},
		{name: "base64 nonfinite coordinate", encoding: EncodingBase64, body: `{"results":[{"embedding":"` + base64.StdEncoding.EncodeToString(base64NaN) + `"}],"usage":` + validUsage + `}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := testProfile(t, 40, test.encoding, LatencyFast)
			client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return jsonResponse(request, http.StatusOK, []byte(test.body)), nil
			}))

			result, err := client.Embed(t.Context(), oneInput(), authorization(client.descriptor, 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Empty(t, result.Vectors)
			assert.NotContains(t, err.Error(), test.body)
		})
	}
}

func TestEmbedClassifiesHTTPFailuresWithoutProviderContent(t *testing.T) {
	const providerContent = "sensitive synthetic provider detail"
	for _, test := range []struct {
		name      string
		status    int
		kind      error
		retry     string
		wantRetry bool
	}{
		{name: "transient", status: http.StatusTooManyRequests, kind: ErrTransientResponse, retry: "7200", wantRetry: true},
		{name: "capacity", status: http.StatusRequestEntityTooLarge, kind: ErrCapacityResponse, retry: "7200"},
		{name: "permanent", status: http.StatusBadRequest, kind: ErrPermanentResponse, retry: "7200"},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := testProfile(t, 40, EncodingFloat, LatencyFast)
			client := testClient(t, profile, testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := jsonResponse(request, test.status, []byte(providerContent))
				response.Header.Set("Retry-After", test.retry)
				return response, nil
			}))

			result, err := client.Embed(t.Context(), oneInput(), authorization(client.descriptor, 1))
			require.ErrorIs(t, err, test.kind)
			assert.Empty(t, result.Vectors)
			assert.NotContains(t, err.Error(), providerContent)
			delay, set := RetryAfter(err)
			assert.Equal(t, test.wantRetry, set)
			if test.wantRetry {
				assert.Equal(t, time.Hour, delay)
			} else {
				assert.Zero(t, delay)
			}
		})
	}
}

func TestEmbedEnforcesExactProviderByteCapacityBeforeSecretsOrEgress(t *testing.T) {
	for _, test := range []struct {
		name        string
		textBytes   int64
		wantFailure bool
	}{
		{name: "equal", textBytes: providerPayloadMax - 150},
		{name: "one byte over", textBytes: providerPayloadMax - 149, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := testProfile(t, 40, EncodingFloat, LatencyFast)
			profile.MaxInputItemBytes = maximumInputBytes
			profile.MaxInputBytes = maximumInputBytes
			profile.Descriptor = descriptorFor(t, profile)
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, profile, secrets, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				return jsonResponse(request, http.StatusOK, validFloatResponse(t, 155, 2, 0)), nil
			}))
			inputs := oneInput()
			inputs[0].Text = strings.Repeat("a", int(test.textBytes))
			permission := authorization(client.descriptor, 1)
			permission.MaxInputBytes = maximumInputBytes

			result, err := client.Embed(t.Context(), inputs, permission)
			if test.wantFailure {
				require.ErrorIs(t, err, ErrCapacityResponse)
				assert.Empty(t, result.Vectors)
				assert.Zero(t, secrets.calls.Load())
				assert.Zero(t, requests.Load())
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Vectors, 1)
			assert.Equal(t, int32(1), secrets.calls.Load())
			assert.Equal(t, int32(1), requests.Load())
		})
	}
}

func TestEmbedBindsPreparedPayloadAndResultsToValidatedInputSnapshot(t *testing.T) {
	for _, withReceipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("reordered_entries_receipt_%t", withReceipt), func(t *testing.T) {
			inputs := []document.EmbeddingInput{
				{Key: "first-key", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "first text"},
				{Key: "second-key", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "second text"},
			}
			var captured wireRequest
			profile := testProfile(t, 40, EncodingFloat, LatencyFast)
			client := testClient(t, profile, mutatingSecretResolver(func() {
				inputs[0], inputs[1] = inputs[1], inputs[0]
			}), roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(body, &captured, json.RejectUnknownMembers(true)); err != nil {
					return nil, err
				}
				return jsonResponse(request, http.StatusOK, validFloatResultsResponse(t, 310, 4, 1, 2)), nil
			}))

			var result document.EmbeddingResult
			if withReceipt {
				execution, err := client.EmbedWithReceipt(t.Context(), inputs, authorization(client.descriptor, 2))
				require.NoError(t, err)
				result = execution.Result
				assert.Equal(t, int64(310), execution.Receipt.TotalBytes)
				assert.Equal(t, int64(4), execution.Receipt.TotalTokens)
			} else {
				var err error
				result, err = client.Embed(t.Context(), inputs, authorization(client.descriptor, 2))
				require.NoError(t, err)
			}
			assert.Equal(t, []string{"first text", "second text"}, captured.Input)
			require.Len(t, result.Vectors, 2)
			assert.Equal(t, "first-key", result.Vectors[0].Key)
			assert.InDelta(t, 1, result.Vectors[0].Values[0], 0)
			assert.Equal(t, "second-key", result.Vectors[1].Key)
			assert.InDelta(t, 2, result.Vectors[1].Values[0], 0)
		})
	}

	t.Run("auxiliary slices", func(t *testing.T) {
		inputs := []document.EmbeddingInput{{Key: "original-key", Role: document.EmbeddingRoleDocument,
			Kind: document.EmbeddingInputRenditionChunk, Text: "original text", HeadingPath: []string{"Heading"},
			SourceSpans: []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 1}}}}
		profile := testProfile(t, 40, EncodingFloat, LatencyFast)
		client := testClient(t, profile, mutatingSecretResolver(func() {
			inputs[0].HeadingPath[0] = ""
			inputs[0].SourceSpans[0].CharEnd = 0
		}), roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return jsonResponse(request, http.StatusOK, validFloatResponse(t, 155, 2, 0)), nil
		}))

		result, err := client.Embed(t.Context(), inputs, authorization(client.descriptor, 1))
		require.NoError(t, err)
		require.Len(t, result.Vectors, 1)
		assert.Equal(t, "original-key", result.Vectors[0].Key)
	})
}

func TestEmbedWithReceiptKeepsConcurrentUsageRequestLocalAndRedacted(t *testing.T) {
	type responseSpec struct {
		coordinate  float32
		totalBytes  int64
		totalTokens int64
	}
	specs := map[string]responseSpec{
		"call-one-document-text": {coordinate: 1, totalBytes: 101, totalTokens: 11},
		"call-one-query-text":    {coordinate: 2, totalBytes: 102, totalTokens: 12},
		"call-two-document-text": {coordinate: 3, totalBytes: 201, totalTokens: 21},
		"call-two-query-text":    {coordinate: 4, totalBytes: 202, totalTokens: 22},
	}
	arrived := make(chan struct{}, len(specs))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	client := testClient(t, testProfile(t, 40, EncodingFloat, LatencyFast), testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var payload wireRequest
		if err := json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		if len(payload.Input) != 1 {
			return nil, fmt.Errorf("unexpected synthetic input count: %d", len(payload.Input))
		}
		spec, ok := specs[payload.Input[0]]
		if !ok {
			return nil, errors.New("unexpected synthetic input")
		}
		select {
		case arrived <- struct{}{}:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return jsonResponse(request, http.StatusOK, validFloatResponse(t, spec.totalBytes, spec.totalTokens, spec.coordinate)), nil
	}))
	type outcome struct {
		index     int
		execution Execution
		err       error
	}
	inputs := [][]document.EmbeddingInput{
		{
			{Key: "call-one-document-key", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "call-one-document-text"},
			{Key: "call-one-query-key", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "call-one-query-text"},
		},
		{
			{Key: "call-two-document-key", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "call-two-document-text"},
			{Key: "call-two-query-key", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "call-two-query-text"},
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outcomes := make(chan outcome, len(inputs))
	for index := range inputs {
		go func(index int) {
			execution, err := client.EmbedWithReceipt(ctx, inputs[index], authorization(client.descriptor, len(inputs[index])))
			outcomes <- outcome{index: index, execution: execution, err: err}
		}(index)
	}
	watchdog := time.NewTimer(synchronizationWatchdog)
	defer watchdog.Stop()
	for range inputs {
		select {
		case <-arrived:
		case <-watchdog.C:
			require.FailNow(t, "timed out waiting for concurrent provider requests")
		}
	}
	releaseAll()
	outcomeWatchdog := time.NewTimer(synchronizationWatchdog)
	defer outcomeWatchdog.Stop()
	got := make([]outcome, len(inputs))
	for range inputs {
		select {
		case current := <-outcomes:
			got[current.index] = current
		case <-outcomeWatchdog.C:
			require.FailNow(t, "timed out waiting for concurrent embedding calls")
		}
	}

	wants := []struct {
		keys        []string
		coordinates []float32
		totalBytes  int64
		totalTokens int64
	}{
		{keys: []string{"call-one-document-key", "call-one-query-key"}, coordinates: []float32{1, 2}, totalBytes: 203, totalTokens: 23},
		{keys: []string{"call-two-document-key", "call-two-query-key"}, coordinates: []float32{3, 4}, totalBytes: 403, totalTokens: 43},
	}
	for index, current := range got {
		require.NoError(t, current.err)
		require.Len(t, current.execution.Result.Vectors, 2)
		for vectorIndex, vector := range current.execution.Result.Vectors {
			assert.Equal(t, wants[index].keys[vectorIndex], vector.Key)
			assert.InDelta(t, wants[index].coordinates[vectorIndex], vector.Values[0], 0)
		}
		receipt := current.execution.Receipt
		assert.Equal(t, ProviderID, receipt.ProviderID)
		assert.Equal(t, client.descriptor.Fingerprint, receipt.DescriptorFingerprint)
		assert.Equal(t, client.descriptor.PolicyFingerprint, receipt.PolicyFingerprint)
		assert.Equal(t, Model, receipt.Model)
		assert.Equal(t, client.descriptor.ModelRevision, receipt.ModelRevision)
		assert.Equal(t, EncodingFloat, receipt.EncodingFormat)
		assert.Equal(t, LatencyFast, receipt.RequestedLatency)
		assert.Equal(t, 2, receipt.RequestCount)
		assert.Equal(t, wants[index].totalBytes, receipt.TotalBytes)
		assert.Equal(t, wants[index].totalTokens, receipt.TotalTokens)
		assert.LessOrEqual(t, receipt.TotalBytes, maximumUsage)
		assert.LessOrEqual(t, receipt.TotalTokens, maximumUsage)
		encoded, err := json.Marshal(receipt)
		require.NoError(t, err)
		for text := range specs {
			assert.NotContains(t, string(encoded), text)
		}
	}
}

func TestEmbedWithReceiptReturnsZeroExecutionOnAccumulatedUsageOverflow(t *testing.T) {
	client := testClient(t, testProfile(t, 40, EncodingFloat, LatencyFast), testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var payload wireRequest
		if err := json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
		totalBytes := int64(1)
		if payload.InputType == "document" {
			totalBytes = maximumUsage
		}
		return jsonResponse(request, http.StatusOK, validFloatResponse(t, totalBytes, 0, 0)), nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "document text"},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "query text"},
	}

	execution, err := client.EmbedWithReceipt(t.Context(), inputs, authorization(client.descriptor, len(inputs)))
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Equal(t, Execution{}, execution)
}

func TestEmbedMethodsRejectCanceledContextBeforeSecretsOrEgress(t *testing.T) {
	for _, withReceipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("receipt_%t", withReceipt), func(t *testing.T) {
			secrets := &countingSecrets{value: "synthetic-key"}
			var requests atomic.Int32
			client := testClient(t, testProfile(t, 40, EncodingFloat, LatencyFast), secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected synthetic request")
			}))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			if withReceipt {
				execution, err := client.EmbedWithReceipt(ctx, oneInput(), authorization(client.descriptor, 1))
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, Execution{}, execution)
			} else {
				result, err := client.Embed(ctx, oneInput(), authorization(client.descriptor, 1))
				require.ErrorIs(t, err, context.Canceled)
				assert.Empty(t, result.Vectors)
			}
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
		})
	}
}

func TestEmbedMethodsRejectLateSuccessAfterCancellation(t *testing.T) {
	for _, withReceipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("receipt_%t", withReceipt), func(t *testing.T) {
			started := make(chan struct{})
			client := testClient(t, testProfile(t, 40, EncodingFloat, LatencyFast), testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				close(started)
				<-request.Context().Done()
				return jsonResponse(request, http.StatusOK, validFloatResponse(t, 155, 2, 0)), nil
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				execution Execution
				result    document.EmbeddingResult
				err       error
			}
			done := make(chan outcome, 1)
			go func() {
				if withReceipt {
					execution, err := client.EmbedWithReceipt(ctx, oneInput(), authorization(client.descriptor, 1))
					done <- outcome{execution: execution, err: err}
					return
				}
				result, err := client.Embed(ctx, oneInput(), authorization(client.descriptor, 1))
				done <- outcome{result: result, err: err}
			}()
			select {
			case <-started:
			case <-time.After(synchronizationWatchdog):
				require.FailNow(t, "timed out waiting for provider request")
			}
			cancel()
			select {
			case current := <-done:
				require.ErrorIs(t, current.err, context.Canceled)
				assert.Equal(t, Execution{}, current.execution)
				assert.Empty(t, current.result.Vectors)
			case <-time.After(synchronizationWatchdog):
				require.FailNow(t, "timed out waiting for canceled embedding call")
			}
		})
	}
}

func TestExecuteEmbeddingUsesZeroEntropyAdapter(t *testing.T) {
	client := testClient(t, testProfile(t, 40, EncodingFloat, LatencyFast), testSecrets{"secret:zeroentropy": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, validFloatResponse(t, 155, 2, 0)), nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "document text"},
		{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "query text"},
	}

	result, err := document.ExecuteEmbedding(t.Context(), client, inputs, authorization(client.Descriptor(), len(inputs)))
	require.NoError(t, err)
	require.Len(t, result.Vectors, 2)
	assert.Equal(t, "document", result.Vectors[0].Key)
	assert.Equal(t, "query", result.Vectors[1].Key)
}

func validFloatResponse(t *testing.T, totalBytes, totalTokens int64, first float32) []byte {
	t.Helper()
	return validFloatResultsResponse(t, totalBytes, totalTokens, first)
}

func validFloatResultsResponse(t *testing.T, totalBytes, totalTokens int64, firstCoordinates ...float32) []byte {
	t.Helper()
	results := make([]map[string]any, len(firstCoordinates))
	for index, coordinate := range firstCoordinates {
		vector := make([]float32, 40)
		vector[0] = coordinate
		results[index] = map[string]any{"embedding": vector}
	}
	body, err := json.Marshal(map[string]any{
		"results": results,
		"usage":   map[string]any{"total_bytes": totalBytes, "total_tokens": totalTokens},
	})
	require.NoError(t, err)
	return body
}

type mutatingSecretResolver func()

func (resolver mutatingSecretResolver) ResolveSecret(context.Context, string) (string, error) {
	resolver()
	return "synthetic-key", nil
}
