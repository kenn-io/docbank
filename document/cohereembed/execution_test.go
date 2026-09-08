package cohereembed

import (
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
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

func TestExecuteEmbeddingPreservesCohereRolesAndNeverSendsFilenames(t *testing.T) {
	for _, disclose := range []bool{false, true} {
		t.Run(fmt.Sprintf("disclose_%t", disclose), func(t *testing.T) {
			const filename = "PRIVATE_FILENAME_SENTINEL.png"
			var requests atomic.Int32
			var capturedMu sync.Mutex
			var captured [][]byte
			client := testClient(t, testProfile(t, 256), &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				capturedMu.Lock()
				captured = append(captured, body)
				capturedMu.Unlock()
				var payload wireRequest
				if err := json.Unmarshal(body, &payload, json.RejectUnknownMembers(true)); err != nil {
					return nil, err
				}
				vector := make([]float32, 256)
				response := map[string]any{"id": "synthetic-response", "embeddings": map[string]any{"float": [][]float32{vector}}}
				if len(payload.Inputs) != 0 {
					response["images"] = []map[string]any{{"width": 1, "height": 1, "format": "png", "bit_depth": 8}}
					response["meta"] = map[string]any{"billed_units": map[string]any{"images": 1}}
				}
				encoded, err := json.Marshal(response)
				if err != nil {
					return nil, err
				}
				return jsonResponse(request, http.StatusOK, encoded), nil
			}))
			source := imageUpload(t, tinyPNG(t), "image/png")
			source.metadata.Filename = filename
			inputs := []document.EmbeddingInput{
				{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "document text"},
				{Key: "image", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source},
				{Key: "query", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "query text"},
			}
			permission := authorization(client.Descriptor(), len(inputs))
			permission.DiscloseFilename = disclose

			result, err := document.ExecuteEmbedding(t.Context(), client, inputs, permission)
			require.NoError(t, err)
			require.NoError(t, document.ValidateEmbeddingQueryCompatibility(client.Descriptor(), client.Descriptor()))
			assert.Equal(t, "document text", client.Descriptor().ModelInput.EncodeDocument("document text"))
			assert.Equal(t, "query text", client.Descriptor().ModelInput.EncodeQuery("query text"))
			assert.Equal(t, int32(3), requests.Load())
			require.Len(t, result.Vectors, 3)
			assert.Equal(t, []string{"document", "image", "query"}, []string{result.Vectors[0].Key, result.Vectors[1].Key, result.Vectors[2].Key})
			for _, body := range captured {
				assert.NotContains(t, string(body), filename)
			}
			assert.True(t, source.closed)
		})
	}
}

func TestEmbedWithReceiptKeepsConcurrentFractionalUsageRequestLocalAndRedacted(t *testing.T) {
	const secret = "PRIVATE_CREDENTIAL_SENTINEL"
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	client := testClient(t, testProfile(t, 256), &countingSecrets{value: secret}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var id string
		var inputTokens, outputTokens float64
		switch {
		case strings.Contains(string(body), "usage-one"):
			id, inputTokens, outputTokens = "response-one", 1.25, 2.5
		case strings.Contains(string(body), "usage-two"):
			id, inputTokens, outputTokens = "response-two", 5.5, 8.75
		default:
			return nil, errors.New("unexpected synthetic request")
		}
		arrived <- struct{}{}
		<-release
		response := fmt.Sprintf(`{"id":%q,"embeddings":{"float":[%s]},"meta":{"billed_units":{"input_tokens":%g,"output_tokens":%g},"tokens":{"input_tokens":%g,"output_tokens":%g}}}`, id, vectorJSON(), inputTokens, outputTokens, inputTokens, outputTokens)
		return jsonResponse(request, http.StatusOK, []byte(response)), nil
	}))
	type outcome struct {
		execution Execution
		err       error
	}
	outcomes := make([]outcome, 2)
	var group sync.WaitGroup
	for index, text := range []string{"usage-one", "usage-two"} {
		group.Add(1)
		go func(index int, inputText string) {
			defer group.Done()
			outcomes[index].execution, outcomes[index].err = client.EmbedWithReceipt(t.Context(), oneCohereTextInput(inputText), authorization(client.Descriptor(), 1))
		}(index, text)
	}
	<-arrived
	<-arrived
	close(release)
	group.Wait()

	for index := range outcomes {
		require.NoError(t, outcomes[index].err)
		encoded, err := json.Marshal(outcomes[index].execution.Receipt)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "usage-one")
		assert.NotContains(t, string(encoded), "usage-two")
		assert.NotContains(t, string(encoded), secret)
		assert.NotContains(t, string(encoded), "embeddings")
	}
	assert.InDelta(t, 1.25, outcomes[0].execution.Receipt.InputTokens, 0)
	assert.InDelta(t, 2.5, outcomes[0].execution.Receipt.OutputTokens, 0)
	assert.Equal(t, []string{"response-one"}, outcomes[0].execution.Receipt.ProviderResponseIDs)
	assert.InDelta(t, 5.5, outcomes[1].execution.Receipt.InputTokens, 0)
	assert.InDelta(t, 8.75, outcomes[1].execution.Receipt.OutputTokens, 0)
	assert.Equal(t, []string{"response-two"}, outcomes[1].execution.Receipt.ProviderResponseIDs)
}

func TestEmbedWithReceiptExcludesImageFilenameBytesAndSecret(t *testing.T) {
	const (
		filename = "PRIVATE_IMAGE_FILENAME.png"
		secret   = "PRIVATE_IMAGE_CREDENTIAL"
	)
	data := tinyPNG(t)
	client := testClient(t, testProfile(t, 256), &countingSecrets{value: secret}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"id":"image-response","response_type":"embeddings_by_type","embeddings":{"float":[` + vectorJSON() + `]},"images":[{"width":1,"height":1,"format":"png","bit_depth":8}],"meta":{"billed_units":{"images":1,"image_tokens":2.25}}}`
		return jsonResponse(request, http.StatusOK, []byte(body)), nil
	}))
	source := imageUpload(t, data, "image/png")
	source.metadata.Filename = filename
	input := []document.EmbeddingInput{{Key: "image", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source}}
	permission := authorization(client.Descriptor(), 1)
	permission.DiscloseFilename = true

	execution, err := client.EmbedWithReceipt(t.Context(), input, permission)
	require.NoError(t, err)
	encoded, err := json.Marshal(execution.Receipt)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), filename)
	assert.NotContains(t, string(encoded), base64.StdEncoding.EncodeToString(data))
	assert.NotContains(t, string(encoded), secret)
	assert.Equal(t, 1, execution.Receipt.ImageInputs)
	assert.InDelta(t, 1, execution.Receipt.BilledImages, 0)
	assert.InDelta(t, 2.25, execution.Receipt.ImageTokens, 0)
}

func TestEmbedWithReceiptRejectsLateSuccessAfterCancellationOrDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancellation"
		if timeout {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			profile := testProfile(t, 256)
			if timeout {
				profile.RequestTimeout = 25 * time.Millisecond
				profile.Descriptor = descriptorFor(t, profile)
			}
			client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				close(started)
				<-request.Context().Done()
				return jsonResponse(request, http.StatusOK, []byte(`{"id":"late-success","embeddings":{"float":[`+vectorJSON()+`]}}`)), nil
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct {
				execution Execution
				err       error
			}, 1)
			go func() {
				execution, err := client.EmbedWithReceipt(ctx, oneCohereTextInput("late success"), authorization(client.Descriptor(), 1))
				done <- struct {
					execution Execution
					err       error
				}{execution: execution, err: err}
			}()
			<-started
			if !timeout {
				cancel()
			}
			outcome := <-done
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			require.ErrorIs(t, outcome.err, want)
			assert.Equal(t, Execution{}, outcome.execution)
		})
	}
}
