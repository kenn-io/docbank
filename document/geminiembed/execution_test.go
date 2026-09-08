package geminiembed

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestEmbedSendsExactFormattedTextRequestsAndNormalizesVectors(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	var requests [][]byte
	responseNumber := 0
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		requests = append(requests, body)
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-embedding-2:embedContent", request.URL.String())
		assert.Equal(t, "application/json", request.Header.Get("Accept"))
		assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
		assert.Equal(t, "synthetic-key", request.Header.Get("X-Goog-Api-Key"))
		responseNumber++
		response := geminiJSONResponse(request, `{"embedding":{"values":[`+testNonUnitVectorJSON(128)+`]},"usageMetadata":{"promptTokenCount":3,"promptTokenDetails":[{"modality":"TEXT","tokenCount":2}]}}`)
		response.Header.Set("X-Goog-Request-Id", "request-"+string(rune('0'+responseNumber)))
		return response, nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "document-1", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"},
		{Key: "query-1", Role: document.EmbeddingRoleQuery, Kind: document.EmbeddingInputQueryText, Text: "question"},
	}

	execution, err := client.EmbedWithReceipt(t.Context(), inputs, geminiAuthorization(profile.Descriptor, 2))
	require.NoError(t, err)
	require.Equal(t, []string{
		`{"model":"models/gemini-embedding-2","content":{"parts":[{"text":"title: none | text: passage"}]},"outputDimensionality":128}`,
		`{"model":"models/gemini-embedding-2","content":{"parts":[{"text":"task: search result | query: question"}]},"outputDimensionality":128}`,
	}, byteStrings(requests))
	for _, request := range requests {
		assert.NotContains(t, string(request), "taskType")
		assert.NotContains(t, string(request), "filename")
	}
	require.Len(t, execution.Result.Vectors, 2)
	assert.Equal(t, math.Float32bits(0.6), math.Float32bits(execution.Result.Vectors[0].Values[0]))
	assert.Equal(t, math.Float32bits(0.8), math.Float32bits(execution.Result.Vectors[0].Values[1]))
	assert.Equal(t, 2, execution.Receipt.RequestCount)
	assert.Equal(t, 2, execution.Receipt.EmbeddingResponseCount)
	assert.Equal(t, 2, execution.Receipt.UsageResponseCount)
	assert.Equal(t, int64(6), execution.Receipt.PromptTokens)
	assert.Equal(t, int64(4), execution.Receipt.PromptTokenDetails.Text)
	assert.Equal(t, []string{"request-1", "request-2"}, execution.Receipt.ProviderResponseIDs)
	assert.Equal(t, fixedSemantics(), execution.Receipt.Semantics)
}

func TestExecuteEmbeddingMixedTextAndDirectInputUsesOneAggregateByteBound(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "aggregate.png", "image/png")
	formatted := profile.Descriptor.ModelInput.EncodeDocument("passage")
	exact := int64(len(data) + len(formatted))
	inputs := func(source document.AuthorizedUpload) []document.EmbeddingInput {
		return []document.EmbeddingInput{
			{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: source},
			{Key: "text", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"},
		}
	}

	t.Run("exact", func(t *testing.T) {
		source := newProofUpload(data, metadata, proof)
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
		}))
		authorization := geminiAuthorization(profile.Descriptor, 2)
		authorization.MaxInputBytes = exact

		result, err := document.ExecuteEmbedding(t.Context(), client, inputs(source), authorization)

		require.NoError(t, err)
		assert.Len(t, result.Vectors, 2)
		assert.Equal(t, int32(2), requests.Load())
		assert.Equal(t, int32(1), source.closeCalls.Load())
	})

	t.Run("one byte over", func(t *testing.T) {
		source := newProofUpload(data, metadata, proof)
		client, secrets, requests := noEgressGeminiClient(t, profile)
		authorization := geminiAuthorization(profile.Descriptor, 2)
		authorization.MaxInputBytes = exact - 1

		_, err := document.ExecuteEmbedding(t.Context(), client, inputs(source), authorization)

		require.ErrorContains(t, err, "input bytes")
		assert.Zero(t, source.readPasses.Load())
		assert.Equal(t, int32(1), source.closeCalls.Load())
		assert.Zero(t, secrets.calls.Load())
		assert.Zero(t, requests.Load())
	})
}

func TestEmbedWithReceiptLaterDirectFailureReturnsNoPartialExecution(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, _ := issuedGeminiAuthority(t, profile, data, "receipt.png", "image/png")
	invalid := newProofUpload(data, metadata, document.VerifiedUploadProof{})
	client, secrets, requests := noEgressGeminiClient(t, profile)
	inputs := []document.EmbeddingInput{
		{Key: "text", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"},
		{Key: "file", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputOriginalFile, Source: invalid},
	}

	execution, err := client.EmbedWithReceipt(t.Context(), inputs, directGeminiAuthorization(profile, 2))

	require.ErrorContains(t, err, "proof is invalid")
	assert.Empty(t, execution.Result)
	assert.Equal(t, ProviderID, execution.Receipt.ProviderID)
	assert.Equal(t, profile.Descriptor.Fingerprint, execution.Receipt.DescriptorFingerprint)
	assert.Zero(t, execution.Receipt.RequestCount)
	assert.Zero(t, execution.Receipt.EmbeddingResponseCount)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
}

func TestEmbedAcceptsZeroCoordinateButRejectsMalformedResponseContracts(t *testing.T) {
	validVector := testVectorJSON(128)
	tests := map[string]string{
		"null coordinate":  `{"embedding":{"values":[null,1,` + testZeroVectorJSON(126) + `]}}`,
		"wrong dimension":  `{"embedding":{"values":[` + testVectorJSON(127) + `]}}`,
		"nonfinite":        `{"embedding":{"values":[1e999]}}`,
		"zero vector":      `{"embedding":{"values":[` + testZeroVectorJSON(128) + `]}}`,
		"unknown root":     `{"embedding":{"values":[` + validVector + `]},"unknown":true}`,
		"duplicate root":   `{"embedding":{"values":[` + validVector + `]},"embedding":{"values":[` + validVector + `]}}`,
		"trailing JSON":    `{"embedding":{"values":[` + validVector + `]}}{}`,
		"soft token shape": `{"embedding":{"values":[` + validVector + `],"shape":[1,128]}}`,
		"null shape":       `{"embedding":{"values":[` + validVector + `],"shape":null}}`,
		"null usage":       `{"embedding":{"values":[` + validVector + `]},"usageMetadata":null}`,
		"generation usage": `{"embedding":{"values":[` + validVector + `]},"usageMetadata":{"candidatesTokenCount":1}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			client, profile := clientReturning(t, "application/json", body)
			result, err := client.Embed(t.Context(), oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Empty(t, result)
		})
	}

	client, profile := clientReturning(t, "application/json", `{"embedding":{"values":[`+validVector+`],"shape":[]}}`)
	result, err := client.Embed(t.Context(), oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
	require.NoError(t, err)
	assert.Zero(t, result.Vectors[0].Values[1])
}

func TestReadBoundedClearsPartialResponseAndPreservesReadError(t *testing.T) {
	wantErr := errors.New("synthetic response read failure")
	reader := &partialErrorReader{err: wantErr}

	data, err := readBounded(t.Context(), reader, 1024)

	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, data)
	assert.Equal(t, make([]byte, len(reader.observed)), reader.observed)
}

func TestEmbedRejectsMalformedContentTypes(t *testing.T) {
	for _, mediaType := range []string{"", "text/plain", "application/json; charset=latin1", "application/json; charset=utf-8; version=1", "application/json; broken"} {
		t.Run(mediaType, func(t *testing.T) {
			client, profile := clientReturning(t, mediaType, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`)
			_, err := client.Embed(t.Context(), oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
		})
	}
	for _, mediaType := range []string{"application/json", "application/json; charset=UTF-8"} {
		t.Run(mediaType, func(t *testing.T) {
			client, profile := clientReturning(t, mediaType, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`)
			_, err := client.Embed(t.Context(), oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
			require.NoError(t, err)
		})
	}
}

func TestEmbedRejectsMalformedUsageAndAcceptsDocumentedModalities(t *testing.T) {
	validVector := testVectorJSON(128)
	tests := map[string]string{
		"null prompt":        `{"promptTokenCount":null}`,
		"fractional prompt":  `{"promptTokenCount":1.5}`,
		"negative prompt":    `{"promptTokenCount":-1}`,
		"large prompt":       `{"promptTokenCount":1125899906842625}`,
		"null details":       `{"promptTokenDetails":null}`,
		"null detail":        `{"promptTokenDetails":[{"modality":"TEXT","tokenCount":null}]}`,
		"missing detail":     `{"promptTokenDetails":[{"modality":"TEXT"}]}`,
		"unknown modality":   `{"promptTokenDetails":[{"modality":"PRIVATE","tokenCount":1}]}`,
		"duplicate modality": `{"promptTokenDetails":[{"modality":"TEXT","tokenCount":1},{"modality":"TEXT","tokenCount":2}]}`,
		"unknown usage":      `{"privateTokenCount":1}`,
	}
	for name, usage := range tests {
		t.Run(name, func(t *testing.T) {
			client, profile := clientReturning(t, "application/json", `{"embedding":{"values":[`+validVector+`]},"usageMetadata":`+usage+`}`)
			execution, err := client.EmbedWithReceipt(t.Context(), oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
			require.ErrorIs(t, err, ErrPermanentResponse)
			assert.Empty(t, execution.Result)
			assert.Equal(t, 1, execution.Receipt.RequestCount)
		})
	}

	usage := `{"promptTokenCount":12,"promptTokenDetails":[` +
		`{"modality":"MODALITY_UNSPECIFIED","tokenCount":1},{"modality":"TEXT","tokenCount":2},` +
		`{"modality":"IMAGE","tokenCount":3},{"modality":"VIDEO","tokenCount":4},` +
		`{"modality":"AUDIO","tokenCount":5},{"modality":"DOCUMENT","tokenCount":6}]}`
	client, profile := clientReturning(t, "application/json", `{"embedding":{"values":[`+validVector+`]},"usageMetadata":`+usage+`}`)
	execution, err := client.EmbedWithReceipt(t.Context(), oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
	require.NoError(t, err)
	assert.Equal(t, int64(12), execution.Receipt.PromptTokens)
	assert.Equal(t, ModalityTokenCounts{Unspecified: 1, Text: 2, Image: 3, Video: 4, Audio: 5, Document: 6}, execution.Receipt.PromptTokenDetails)
}

func TestReceiptDistinguishesAvailableAndUnavailableUsage(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	calls := 0
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		usage := ""
		if calls == 1 {
			usage = `,"usageMetadata":{"promptTokenCount":0}`
		}
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}`+usage+`}`), nil
	}))
	inputs := []document.EmbeddingInput{
		{Key: "one", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "one"},
		{Key: "two", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "two"},
	}
	execution, err := client.EmbedWithReceipt(t.Context(), inputs, geminiAuthorization(profile.Descriptor, 2))
	require.NoError(t, err)
	assert.Equal(t, 2, execution.Receipt.EmbeddingResponseCount)
	assert.Equal(t, 1, execution.Receipt.UsageResponseCount)
}

func TestFilesReceiptTracksUsageAndConfirmedCleanup(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, data, "usage.png", "image/png")
	lifecycle := newSuccessfulFilesLifecycle(t, data, "image/png", "usage.png")
	lifecycle.embedBody = `{"embedding":{"values":[` + testVectorJSON(128) + `]},"usageMetadata":{"promptTokenCount":9,"promptTokenDetails":[{"modality":"IMAGE","tokenCount":9}]}}`
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, lifecycle)

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.NoError(t, err)
	require.Len(t, execution.Result.Vectors, 1)
	assert.Equal(t, 6, execution.Receipt.RequestCount)
	assert.Equal(t, 1, execution.Receipt.EmbeddingResponseCount)
	assert.Equal(t, 1, execution.Receipt.UsageResponseCount)
	assert.Equal(t, int64(9), execution.Receipt.PromptTokens)
	assert.Equal(t, int64(9), execution.Receipt.PromptTokenDetails.Image)
	assert.Zero(t, execution.Receipt.UnconfirmedFileRetentions)
	assert.Empty(t, execution.Receipt.Warnings)
}

func TestFilesLifecycleResponseIDsDoNotInflateEmbeddingCounts(t *testing.T) {
	profile, _, source, fixture := newFilesExecutionFixture(t, "response-ids.png")
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		step := requests.Add(1)
		var response *http.Response
		switch step {
		case 1:
			response = filesStartResponse(request)
		case 2:
			response = geminiJSONResponse(request, `{"file":`+fixture.fileJSON("ACTIVE")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
		case 3:
			response = geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`)
		case 4:
			response = geminiJSONResponse(request, `{}`)
		default:
			return nil, errors.New("unexpected response-ID lifecycle request")
		}
		response.Header.Set("X-Goog-Request-Id", "request-"+strconv.Itoa(int(step)))
		return response, nil
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	require.NoError(t, err)
	assert.Equal(t, 4, execution.Receipt.RequestCount)
	assert.Equal(t, 1, execution.Receipt.EmbeddingResponseCount)
	assert.Equal(t, []string{"request-1", "request-2", "request-3", "request-4"}, execution.Receipt.ProviderResponseIDs)
}

func TestFilesDeleteFailureReturnsNoResultThroughBothPublicAPIs(t *testing.T) {
	for _, publicAPI := range []string{"Embed", "EmbedWithReceipt"} {
		t.Run(publicAPI, func(t *testing.T) {
			profile, data, source, fixture := newFilesExecutionFixture(t, "delete-failure.png")
			client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, activeFilesTransport(t, fixture, func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
			}))
			var result document.EmbeddingResult
			var receipt Receipt
			var err error
			if publicAPI == "Embed" {
				result, err = client.Embed(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))
			} else {
				var execution Execution
				execution, err = client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))
				result, receipt = execution.Result, execution.Receipt
			}

			require.ErrorIs(t, err, ErrTransientResponse)
			require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
			_, retry := RetryAfter(err)
			assert.False(t, retry)
			assert.Empty(t, result)
			if publicAPI == "EmbedWithReceipt" {
				assert.Equal(t, 1, receipt.UnconfirmedFileRetentions)
				assert.Equal(t, []string{"provider file retention is unconfirmed"}, receipt.Warnings)
				assert.Equal(t, 4, receipt.RequestCount)
				assertReceiptHasNoFileEvidence(t, receipt, data, fixture)
			}
		})
	}
}

func TestFilesCancellationAndCleanupFailurePreserveEveryErrorIdentity(t *testing.T) {
	profile, _, source, fixture := newFilesExecutionFixture(t, "cancel-cleanup.png")
	embedStarted := make(chan struct{})
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON("ACTIVE")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case 3:
			close(embedStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		case 4:
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
		default:
			return nil, errors.New("unexpected cancellation lifecycle request")
		}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct {
		execution Execution
		err       error
	}, 1)
	go func() {
		execution, err := client.EmbedWithReceipt(ctx, directGeminiInputs(source), directGeminiAuthorization(profile, 1))
		done <- struct {
			execution Execution
			err       error
		}{execution: execution, err: err}
	}()
	awaitSignal(t, embedStarted)
	cancel()
	var outcome struct {
		execution Execution
		err       error
	}
	select {
	case outcome = <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled Files execution did not finish")
	}

	require.ErrorIs(t, outcome.err, context.Canceled)
	require.ErrorIs(t, outcome.err, ErrTransientResponse)
	require.ErrorIs(t, outcome.err, ErrRemoteRetentionUnconfirmed)
	assert.Empty(t, outcome.execution.Result)
	assert.Equal(t, 1, outcome.execution.Receipt.UnconfirmedFileRetentions)
	assert.Equal(t, int32(4), requests.Load())
}

func TestFilesTransientPrimaryAndTimedOutCleanupPreserveEveryErrorIdentity(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.CleanupTimeout = 20 * time.Millisecond
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	source := authorizeGeminiFixture(t, profile, data, "transient-cleanup.png", "image/png")
	fixture := newSuccessfulFilesLifecycle(t, data, "image/png", "transient-cleanup.png")
	deleteStarted := make(chan struct{})
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch requests.Add(1) {
		case 1:
			return filesStartResponse(request), nil
		case 2:
			response := geminiJSONResponse(request, `{"file":`+fixture.fileJSON("ACTIVE")+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		case 3:
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
		case 4:
			close(deleteStarted)
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			return nil, errors.New("unexpected transient cleanup lifecycle request")
		}
	}))

	execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(source), directGeminiAuthorization(profile, 1))

	awaitSignal(t, deleteStarted)
	require.ErrorIs(t, err, ErrTransientResponse)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrRemoteRetentionUnconfirmed)
	assert.Empty(t, execution.Result)
	assert.Equal(t, 1, execution.Receipt.UnconfirmedFileRetentions)
	assert.Equal(t, int32(4), requests.Load())
}

func TestFilesRetentionCountersAndWarningsSaturate(t *testing.T) {
	receipt := Receipt{UnconfirmedFileRetentions: int(^uint(0)>>1) - 1, Warnings: make([]string, 32)}
	recordUnconfirmedFileRetention(&receipt)
	recordUnconfirmedFileRetention(&receipt)
	assert.Equal(t, int(^uint(0)>>1), receipt.UnconfirmedFileRetentions)
	assert.Len(t, receipt.Warnings, 32)
}

func TestFilesConcurrentReceiptsRemainRequestLocal(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.Transport = TransportFilesAPI
	profile.Descriptor = geminiDescriptorFor(t, profile)
	data := mediatest.PNG(2, 2, nil)
	metadata, proof := issuedGeminiAuthority(t, profile, data, "concurrent.png", "image/png")
	created := time.Now().UTC().Truncate(time.Second)
	timeline := fileTestTimeline{created: created, updated: created, expires: created.Add(48 * time.Hour)}
	var starts atomic.Int32
	var requests atomic.Int32
	client := newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		if request.Method == http.MethodPost && request.URL.Path == filesUploadPath && request.URL.RawQuery == "" {
			id := starts.Add(1)
			response := geminiJSONResponse(request, "")
			response.Header.Set("X-Goog-Upload-Url", origin+filesUploadPath+"?upload_id=session-"+strconv.Itoa(int(id))+"&upload_protocol=resumable")
			return response, nil
		}
		if request.Method == http.MethodPost && request.URL.Path == filesUploadPath {
			id := strings.TrimPrefix(request.URL.Query().Get("upload_id"), "session-")
			name := "files/file-" + id
			uri := origin + "/v1beta/" + name
			response := geminiJSONResponse(request, `{"file":`+fileTestJSON(data, "image/png", name, uri, "ACTIVE", timeline)+`}`)
			response.Header.Set("X-Goog-Upload-Status", "final")
			return response, nil
		}
		if request.Method == http.MethodPost && request.URL.Path == embedPath {
			return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
		}
		if request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, filesPathPrefix) {
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: request}, nil
		}
		return nil, errors.New("unexpected concurrent Files request")
	}))

	const calls = 8
	outcomes := make(chan struct {
		execution Execution
		err       error
	}, calls)
	for range calls {
		go func() {
			execution, err := client.EmbedWithReceipt(t.Context(), directGeminiInputs(newProofUpload(data, metadata, proof)), directGeminiAuthorization(profile, 1))
			outcomes <- struct {
				execution Execution
				err       error
			}{execution: execution, err: err}
		}()
	}
	for range calls {
		outcome := <-outcomes
		require.NoError(t, outcome.err)
		require.Len(t, outcome.execution.Result.Vectors, 1)
		assert.Equal(t, 4, outcome.execution.Receipt.RequestCount)
		assert.Equal(t, 1, outcome.execution.Receipt.EmbeddingResponseCount)
		assert.Zero(t, outcome.execution.Receipt.UnconfirmedFileRetentions)
		assert.Empty(t, outcome.execution.Receipt.Warnings)
	}
	assert.Equal(t, int32(calls*4), requests.Load())
}

func TestEmbedRejectsLateSuccessAfterCancellation(t *testing.T) {
	started := make(chan struct{})
	client, profile := clientWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := client.Embed(ctx, oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
		done <- err
	}()
	awaitSignal(t, started)
	cancel()
	require.ErrorIs(t, awaitError(t, done), context.Canceled)
}

func TestEmbedRejectsLateSuccessfulSecretAndBodyRead(t *testing.T) {
	t.Run("secret", func(t *testing.T) {
		profile := geminiTestProfile(t, 128)
		profile.Descriptor = geminiDescriptorFor(t, profile)
		started := make(chan struct{})
		var requests atomic.Int32
		client := newGeminiTestClient(t, profile, lateSecretResolver{started: started}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			return geminiJSONResponse(request, `{"embedding":{"values":[`+testVectorJSON(128)+`]}}`), nil
		}))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct {
			result document.EmbeddingResult
			err    error
		}, 1)
		go func() {
			result, err := client.Embed(ctx, oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
			done <- struct {
				result document.EmbeddingResult
				err    error
			}{result: result, err: err}
		}()
		awaitSignal(t, started)
		cancel()
		outcome := awaitOutcome(t, done)
		require.ErrorIs(t, outcome.err, context.Canceled)
		assert.Empty(t, outcome.result)
		assert.Zero(t, requests.Load())
	})

	t.Run("body read", func(t *testing.T) {
		started := make(chan struct{})
		client, profile := clientWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: &lateSuccessBody{ctx: request.Context(), started: started, payload: []byte(`{"embedding":{"values":[` + testVectorJSON(128) + `]}}`)}, Request: request}, nil
		}))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct {
			result document.EmbeddingResult
			err    error
		}, 1)
		go func() {
			result, err := client.Embed(ctx, oneGeminiInput(), geminiAuthorization(profile.Descriptor, 1))
			done <- struct {
				result document.EmbeddingResult
				err    error
			}{result: result, err: err}
		}()
		awaitSignal(t, started)
		cancel()
		outcome := awaitOutcome(t, done)
		require.ErrorIs(t, outcome.err, context.Canceled)
		assert.Empty(t, outcome.result)
	})
}

func TestRecordReceiptRejectsAggregateOverflowAndBoundsResponseIDs(t *testing.T) {
	receipt := Receipt{PromptTokens: maxUsageValue}
	usage := &wireUsage{PromptTokenCount: tokenCountPointer(1)}
	assert.False(t, recordReceiptResponse(&receipt, usage, ""))

	receipt = Receipt{}
	for index := range 129 {
		require.True(t, recordReceiptResponse(&receipt, nil, strings.Repeat("a", 120)+string(rune('A'+index%26))))
	}
	assert.Len(t, receipt.ProviderResponseIDs, 128)
	assert.Equal(t, 1, receipt.OmittedProviderResponseIDs)
}

func TestRecordReceiptRejectsInvalidOrOverflowingUpdateAtomically(t *testing.T) {
	t.Run("duplicate modality", func(t *testing.T) {
		receipt := Receipt{PromptTokens: 7}
		before := receipt
		usage := &wireUsage{PromptTokenDetails: wirePromptTokenDetails{set: true, values: []wireModalityTokenCount{
			{Modality: "TEXT", TokenCount: tokenCountPointer(1)},
			{Modality: "TEXT", TokenCount: tokenCountPointer(2)},
		}}}
		assert.False(t, recordReceiptResponse(&receipt, usage, ""))
		assert.Equal(t, before, receipt)
	})

	t.Run("omitted response ID overflow", func(t *testing.T) {
		receipt := Receipt{ProviderResponseIDs: make([]string, 128), OmittedProviderResponseIDs: int(^uint(0) >> 1)}
		before := receipt
		assert.False(t, recordReceiptResponse(&receipt, nil, "response-id"))
		assert.Equal(t, before, receipt)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type lateSecretResolver struct{ started chan<- struct{} }

func (resolver lateSecretResolver) ResolveSecret(ctx context.Context, _ string) (string, error) {
	close(resolver.started)
	<-ctx.Done()
	return "synthetic-key", nil
}

type lateSuccessBody struct {
	ctx     context.Context
	started chan<- struct{}
	payload []byte
	done    bool
}

type partialErrorReader struct {
	observed []byte
	err      error
}

func (reader *partialErrorReader) Read(destination []byte) (int, error) {
	if reader.observed != nil {
		return 0, io.EOF
	}
	count := copy(destination, "private partial response")
	reader.observed = destination[:count]
	return count, reader.err
}

func (body *lateSuccessBody) Read(destination []byte) (int, error) {
	if body.done {
		return 0, io.EOF
	}
	close(body.started)
	<-body.ctx.Done()
	body.done = true
	return copy(destination, body.payload), nil
}

func (*lateSuccessBody) Close() error { return nil }

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for synthetic callback")
	}
}

func awaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for embedding result")
		return nil
	}
}

func awaitOutcome(t *testing.T, result <-chan struct {
	result document.EmbeddingResult
	err    error
}) struct {
	result document.EmbeddingResult
	err    error
} {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for embedding result")
		return struct {
			result document.EmbeddingResult
			err    error
		}{}
	}
}

func clientReturning(t *testing.T, mediaType, body string) (*Client, Profile) {
	t.Helper()
	return clientWithTransport(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := geminiJSONResponse(request, body)
		response.Header.Set("Content-Type", mediaType)
		return response, nil
	}))
}

func clientWithTransport(t *testing.T, transport http.RoundTripper) (*Client, Profile) {
	t.Helper()
	profile := geminiTestProfile(t, 128)
	profile.Descriptor = geminiDescriptorFor(t, profile)
	return newGeminiTestClient(t, profile, syntheticSecrets{"secret:gemini": "synthetic-key"}, transport), profile
}

func oneGeminiInput() []document.EmbeddingInput {
	return []document.EmbeddingInput{{Key: "document", Role: document.EmbeddingRoleDocument, Kind: document.EmbeddingInputRenditionChunk, Text: "passage"}}
}

func geminiJSONResponse(request *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func testVectorJSON(dimension int) string {
	values := make([]float32, dimension)
	values[0] = 1
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(bytes.Trim(encoded, "[]"))
}

func testZeroVectorJSON(dimension int) string {
	encoded, err := json.Marshal(make([]float32, dimension))
	if err != nil {
		panic(err)
	}
	return string(bytes.Trim(encoded, "[]"))
}

func testNonUnitVectorJSON(dimension int) string {
	values := make([]float32, dimension)
	values[0], values[1] = 3, 4
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(bytes.Trim(encoded, "[]"))
}

func byteStrings(values [][]byte) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = string(values[index])
	}
	return result
}

func tokenCountPointer(value int64) tokenCount {
	return tokenCount{value: value, set: true}
}
