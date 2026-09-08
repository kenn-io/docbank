package openaihosted

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

type ownershipSecretResolver struct {
	calls    *int
	callback func()
}

func (resolver ownershipSecretResolver) ResolveSecret(context.Context, string) (string, error) {
	(*resolver.calls)++
	if resolver.callback != nil {
		resolver.callback()
	}
	return "sk-synthetic", nil
}

func TestOwnershipPreservesOriginalIdentityAcrossSynchronousCallbacks(t *testing.T) {
	for _, api := range []string{"Embed", "EmbedWithReceipt", "ExecuteEmbedding"} {
		t.Run(api, func(t *testing.T) {
			inputs := twoHostedInputs()
			var secretCalls int
			client := newHostedTestClient(t, hostedTestProfile(t), ownershipSecretResolver{
				calls: &secretCalls,
				callback: func() {
					inputs[0].Key = "replacement-first"
					inputs[1].Key = "replacement-second"
				},
			}, ownershipSuccessTransport(t))

			result, receipt, err := callOwnershipAPI(t, api, client, inputs)
			require.NoError(t, err)
			assert.Equal(t, 1, secretCalls)
			assert.Equal(t, document.EmbeddingResult{Vectors: []document.EmbeddingVector{
				{Key: "first", Values: []float32{1, 0, 0}},
				{Key: "second", Values: []float32{0, 1, 0}},
			}}, result)
			if api == "EmbedWithReceipt" {
				assert.Equal(t, 1, receipt.RequestCount)
				assert.Equal(t, int64(2), receipt.PromptTokens)
				assert.Equal(t, int64(2), receipt.TotalTokens)
			}
		})
	}
}

func TestOwnershipPreservesValidatedDocumentAuxiliariesAcrossSynchronousCallbacks(t *testing.T) {
	mutations := []struct {
		name   string
		setup  func([]document.EmbeddingInput)
		mutate func([]document.EmbeddingInput)
	}{
		{
			name: "heading path",
			setup: func(inputs []document.EmbeddingInput) {
				inputs[0].HeadingPath = []string{"Synthetic section"}
			},
			mutate: func(inputs []document.EmbeddingInput) {
				inputs[0].HeadingPath[0] = ""
			},
		},
		{
			name: "source spans",
			setup: func(inputs []document.EmbeddingInput) {
				inputs[0].SourceSpans = []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 5}}
			},
			mutate: func(inputs []document.EmbeddingInput) {
				inputs[0].SourceSpans[0].CharEnd = 0
			},
		},
	}
	for _, api := range []string{"Embed", "EmbedWithReceipt"} {
		for _, mutation := range mutations {
			t.Run(api+"/"+mutation.name, func(t *testing.T) {
				inputs := twoHostedInputs()
				mutation.setup(inputs)
				var secretCalls int
				client := newHostedTestClient(t, hostedTestProfile(t), ownershipSecretResolver{
					calls:    &secretCalls,
					callback: func() { mutation.mutate(inputs) },
				}, ownershipSuccessTransport(t))

				result, receipt, err := callOwnershipAPI(t, api, client, inputs)
				require.NoError(t, err)
				assert.Equal(t, 1, secretCalls)
				assert.Equal(t, document.EmbeddingResult{Vectors: []document.EmbeddingVector{
					{Key: "first", Values: []float32{1, 0, 0}},
					{Key: "second", Values: []float32{0, 1, 0}},
				}}, result)
				if api == "EmbedWithReceipt" {
					assert.Equal(t, 1, receipt.RequestCount)
					assert.Equal(t, int64(2), receipt.PromptTokens)
					assert.Equal(t, int64(2), receipt.TotalTokens)
				}
			})
		}
	}
}

func TestOwnershipRejectsPreexistingInvalidAuxiliariesBeforeCallbacks(t *testing.T) {
	invalidInputs := []struct {
		name   string
		mutate func([]document.EmbeddingInput)
	}{
		{
			name: "heading path",
			mutate: func(inputs []document.EmbeddingInput) {
				inputs[0].HeadingPath = []string{""}
			},
		},
		{
			name: "source spans",
			mutate: func(inputs []document.EmbeddingInput) {
				inputs[0].SourceSpans = []document.ChunkSpan{{UnitIndex: 0, CharStart: 0, CharEnd: 0}}
			},
		},
	}
	for _, api := range []string{"Embed", "EmbedWithReceipt"} {
		for _, invalid := range invalidInputs {
			t.Run(api+"/"+invalid.name, func(t *testing.T) {
				inputs := twoHostedInputs()
				invalid.mutate(inputs)
				var secretCalls, transportCalls int
				client := newHostedTestClient(t, hostedTestProfile(t), ownershipSecretResolver{calls: &secretCalls},
					roundTripFunc(func(*http.Request) (*http.Response, error) {
						transportCalls++
						return nil, errors.New("unexpected synthetic transport call")
					}))

				result, receipt, err := callOwnershipAPI(t, api, client, inputs)
				require.Error(t, err)
				assert.Empty(t, result)
				assert.Equal(t, Receipt{}, receipt)
				assert.Zero(t, secretCalls)
				assert.Zero(t, transportCalls)
			})
		}
	}
}

func callOwnershipAPI(
	t *testing.T, api string, client *Client, inputs []document.EmbeddingInput,
) (document.EmbeddingResult, Receipt, error) {
	t.Helper()
	authorization := hostedAuthorization(client.Descriptor(), 2)
	switch api {
	case "EmbedWithReceipt":
		execution, err := client.EmbedWithReceipt(t.Context(), inputs, authorization)
		return execution.Result, execution.Receipt, err
	case "ExecuteEmbedding":
		result, err := document.ExecuteEmbedding(t.Context(), client, inputs, authorization)
		return result, Receipt{}, err
	default:
		result, err := client.Embed(t.Context(), inputs, authorization)
		return result, Receipt{}, err
	}
}

func ownershipSuccessTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"input":["passage: alpha","query: beta"],
			"model":"text-embedding-3-large",
			"dimensions":3,
			"encoding_format":"float"
		}`, string(payload))
		return hostedJSONResponse(request, http.StatusOK, `{
			"object":"list",
			"data":[
				{"object":"embedding","embedding":[0,1,0],"index":1},
				{"object":"embedding","embedding":[1,0,0],"index":0}
			],
			"model":"text-embedding-3-large",
			"usage":{"prompt_tokens":2,"total_tokens":2}
		}`), nil
	})
}
