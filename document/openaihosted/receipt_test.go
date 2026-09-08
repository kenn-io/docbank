package openaihosted

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmbedWithReceiptReturnsValidatedUsageAndConfiguredIdentity(t *testing.T) {
	const requestSentinel = "REQUEST_TEXT_SENTINEL"
	const credentialSentinel = "CREDENTIAL_SENTINEL"
	profile := hostedTestProfile(t)
	input := []document.EmbeddingInput{{
		Key: "first", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: requestSentinel,
	}}
	client := newHostedTestClient(t, profile, hostedSecrets{"secret:openai": credentialSentinel}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return hostedJSONResponse(request, http.StatusOK, `{
			"object":"list",
			"data":[{"object":"embedding","embedding":[0.6,0.8,0],"index":0}],
			"model":"text-embedding-3-large",
			"usage":{"prompt_tokens":2,"total_tokens":2}
		}`), nil
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), input, hostedAuthorization(client.descriptor, 1))
	require.NoError(t, err)
	assert.Equal(t, document.EmbeddingResult{Vectors: []document.EmbeddingVector{{
		Key: "first", Values: []float32{0.6, 0.8, 0},
	}}}, execution.Result)
	assert.Equal(t, Receipt{
		ProviderID: ProviderID, DescriptorFingerprint: client.Descriptor().Fingerprint,
		PolicyFingerprint: client.Descriptor().PolicyFingerprint, Model: Model,
		ModelRevision: client.Descriptor().ModelRevision, RequestCount: 1,
		PromptTokens: 2, TotalTokens: 2,
	}, execution.Receipt)

	encoded, err := json.Marshal(execution.Receipt)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), requestSentinel)
	assert.NotContains(t, string(encoded), credentialSentinel)
	assert.NotContains(t, string(encoded), "0.6")
	assert.NotContains(t, string(encoded), "0.8")
	assert.NotContains(t, string(encoded), "Vectors")
}

func TestEmbedWithReceiptPreservesExplicitZeroUsage(t *testing.T) {
	client := hostedClientReturning(t, "application/json", oneHostedResponse(0, 0))

	execution, err := client.EmbedWithReceipt(t.Context(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
	require.NoError(t, err)
	assert.Equal(t, int64(0), execution.Receipt.PromptTokens)
	assert.Equal(t, int64(0), execution.Receipt.TotalTokens)
	assert.Equal(t, 1, execution.Receipt.RequestCount)
}

func TestEmbedWithReceiptRejectsInvalidUsageWithZeroExecution(t *testing.T) {
	validPrefix := `{"object":"list","data":[{"object":"embedding","embedding":[1,0,0],"index":0}],"model":"text-embedding-3-large"`
	tests := map[string]string{
		"usage omitted":      validPrefix + `}`,
		"usage null":         validPrefix + `,"usage":null}`,
		"prompt omitted":     validPrefix + `,"usage":{"total_tokens":2}}`,
		"prompt null":        validPrefix + `,"usage":{"prompt_tokens":null,"total_tokens":2}}`,
		"total omitted":      validPrefix + `,"usage":{"prompt_tokens":2}}`,
		"total null":         validPrefix + `,"usage":{"prompt_tokens":2,"total_tokens":null}}`,
		"negative":           validPrefix + `,"usage":{"prompt_tokens":-1,"total_tokens":2}}`,
		"inconsistent":       validPrefix + `,"usage":{"prompt_tokens":2,"total_tokens":1}}`,
		"prompt overflowing": validPrefix + `,"usage":{"prompt_tokens":9223372036854775808,"total_tokens":9223372036854775808}}`,
		"total overflowing":  validPrefix + `,"usage":{"prompt_tokens":2,"total_tokens":9223372036854775808}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client := hostedClientReturning(t, "application/json", body)
			execution, err := client.EmbedWithReceipt(t.Context(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, Execution{}, execution)
		})
	}
}

func TestEmbedWithReceiptReturnsZeroExecutionWhenResultValidationFails(t *testing.T) {
	client := hostedClientReturning(t, "application/json", `{
		"object":"list",
		"data":[{"object":"embedding","embedding":[0,0,0],"index":0}],
		"model":"text-embedding-3-large",
		"usage":{"prompt_tokens":2,"total_tokens":2}
	}`)

	execution, err := client.EmbedWithReceipt(t.Context(), oneHostedInput(), hostedAuthorization(client.descriptor, 1))
	require.ErrorIs(t, err, ErrPermanentResponse)
	assert.Equal(t, Execution{}, execution)
}

func TestEmbedAndEmbedWithReceiptUseOneIdenticalAuthorizedRequestEach(t *testing.T) {
	var payloads [][]byte
	client := newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
		return hostedJSONResponse(request, http.StatusOK, oneHostedResponse(2, 2)), nil
	}))
	authorization := hostedAuthorization(client.descriptor, 1)

	execution, err := client.EmbedWithReceipt(t.Context(), oneHostedInput(), authorization)
	require.NoError(t, err)
	result, err := client.Embed(t.Context(), oneHostedInput(), authorization)
	require.NoError(t, err)

	require.Len(t, payloads, 2)
	assert.Equal(t, payloads[0], payloads[1])
	assert.Equal(t, execution.Result, result)
	assert.Equal(t, 1, execution.Receipt.RequestCount)
}

func TestEmbedWithReceiptKeepsConcurrentUsageRequestLocal(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var requests atomic.Int32
	client := newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var body string
		switch {
		case strings.Contains(string(payload), "usage-one"):
			body = oneHostedResponse(1, 3)
		case strings.Contains(string(payload), "usage-two"):
			body = oneHostedResponse(5, 8)
		default:
			return nil, errors.New("unexpected synthetic request")
		}
		arrived <- struct{}{}
		<-release
		return hostedJSONResponse(request, http.StatusOK, body), nil
	}))
	authorization := hostedAuthorization(client.descriptor, 1)
	type outcome struct {
		execution Execution
		err       error
	}
	outcomes := make([]outcome, 2)
	var group sync.WaitGroup
	for index, text := range []string{"usage-one", "usage-two"} {
		group.Add(1)
		go func(index int, text string) {
			defer group.Done()
			inputs := []document.EmbeddingInput{{Key: fmt.Sprintf("input-%d", index), Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: text}}
			outcomes[index].execution, outcomes[index].err = client.EmbedWithReceipt(t.Context(), inputs, authorization)
		}(index, text)
	}
	<-arrived
	<-arrived
	close(release)
	group.Wait()

	require.NoError(t, outcomes[0].err)
	require.NoError(t, outcomes[1].err)
	assert.Equal(t, int64(1), outcomes[0].execution.Receipt.PromptTokens)
	assert.Equal(t, int64(3), outcomes[0].execution.Receipt.TotalTokens)
	assert.Equal(t, int64(5), outcomes[1].execution.Receipt.PromptTokens)
	assert.Equal(t, int64(8), outcomes[1].execution.Receipt.TotalTokens)
	assert.Equal(t, int32(2), requests.Load())
}

func TestEmbedWithReceiptReturnsCallerOwnedResultBuffers(t *testing.T) {
	client := hostedClientReturning(t, "application/json", oneHostedResponse(1, 1))
	authorization := hostedAuthorization(client.descriptor, 1)

	first, err := client.EmbedWithReceipt(t.Context(), oneHostedInput(), authorization)
	require.NoError(t, err)
	first.Result.Vectors[0].Values[0] = 0
	second, err := client.EmbedWithReceipt(t.Context(), oneHostedInput(), authorization)
	require.NoError(t, err)

	assert.Equal(t, []float32{1, 0, 0}, second.Result.Vectors[0].Values)
	assert.NotEqual(t, first.Result.Vectors[0].Values, second.Result.Vectors[0].Values)
}

func TestEmbedWithReceiptReturnsZeroExecutionAfterLateSuccess(t *testing.T) {
	started := make(chan struct{})
	client := newHostedTestClient(t, hostedTestProfile(t), hostedSecrets{"secret:openai": "sk-synthetic"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return hostedJSONResponse(request, http.StatusOK, oneHostedResponse(1, 1)), nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan outcomeWithReceipt, 1)
	go func() {
		execution, err := client.EmbedWithReceipt(ctx, oneHostedInput(), hostedAuthorization(client.descriptor, 1))
		done <- outcomeWithReceipt{execution: execution, err: err}
	}()
	<-started
	cancel()

	outcome := <-done
	require.ErrorIs(t, outcome.err, context.Canceled)
	assert.Equal(t, Execution{}, outcome.execution)
}

func TestExecuteEmbeddingUsesHostedAdapterValidation(t *testing.T) {
	client := hostedClientReturning(t, "application/json", oneHostedResponse(1, 1))
	inputs := oneHostedInput()

	result, err := document.ExecuteEmbedding(t.Context(), client, inputs, hostedAuthorization(client.descriptor, 1))
	require.NoError(t, err)
	assert.Equal(t, document.EmbeddingResult{Vectors: []document.EmbeddingVector{{Key: "first", Values: []float32{1, 0, 0}}}}, result)
}

type outcomeWithReceipt struct {
	execution Execution
	err       error
}

func oneHostedResponse(promptTokens, totalTokens int64) string {
	return fmt.Sprintf(`{
		"object":"list",
		"data":[{"object":"embedding","embedding":[1,0,0],"index":0}],
		"model":"text-embedding-3-large",
		"usage":{"prompt_tokens":%d,"total_tokens":%d}
	}`, promptTokens, totalTokens)
}
