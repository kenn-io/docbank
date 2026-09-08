package cohereembed

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestEmbedWithReceiptRejectsInvalidVectorRepresentationsWithZeroExecution(t *testing.T) {
	tests := map[string]string{
		"null coordinate":      `{"id":"synthetic","embeddings":{"float":[[` + strings.Repeat("0,", 255) + `null]]}}`,
		"string coordinate":    `{"id":"synthetic","embeddings":{"float":[[` + strings.Repeat("0,", 255) + `"PRIVATE_VECTOR"]]}}`,
		"overflow coordinate":  `{"id":"synthetic","embeddings":{"float":[[` + strings.Repeat("0,", 255) + `1e999]]}}`,
		"missing vector":       `{"id":"synthetic","embeddings":{"float":[]}}`,
		"extra representation": `{"id":"synthetic","embeddings":{"float":[` + vectorJSON() + `],"int8":[[1]]}}`,
		"image count mismatch": `{"id":"synthetic","embeddings":{"float":[` + vectorJSON() + `]},"meta":{"billed_units":{"images":1}}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client := cohereClientReturning(t, body)
			execution, err := client.EmbedWithReceipt(t.Context(), oneCohereTextInput("PRIVATE_INPUT"), authorization(client.Descriptor(), 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Equal(t, Execution{}, execution)
			assert.NotContains(t, err.Error(), "PRIVATE")
		})
	}
}

func TestEmbedWithReceiptAcceptsGenuineNumericZeroCoordinates(t *testing.T) {
	client := cohereClientReturning(t, `{"id":"synthetic-zero","embeddings":{"float":[[`+strings.TrimSuffix(strings.Repeat("0,", 256), ",")+`]]}}`)

	execution, err := client.EmbedWithReceipt(t.Context(), oneCohereTextInput("zero control"), authorization(client.Descriptor(), 1))
	require.NoError(t, err)
	require.Len(t, execution.Result.Vectors, 1)
	assert.Equal(t, make([]float32, 256), execution.Result.Vectors[0].Values)
	assert.Equal(t, []string{"synthetic-zero"}, execution.Receipt.ProviderResponseIDs)
}

func cohereClientReturning(t *testing.T, body string) *Client {
	t.Helper()
	return testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(request, http.StatusOK, []byte(body)), nil
	}))
}

func oneCohereTextInput(text string) []document.EmbeddingInput {
	return []document.EmbeddingInput{{
		Key: "document", Role: document.EmbeddingRoleDocument,
		Kind: document.EmbeddingInputRenditionChunk, Text: text,
	}}
}
