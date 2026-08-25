package docling

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

//go:embed testdata/docling-pages.json
var recordedPagesResponse []byte

//go:embed testdata/docling-schema-drift.json
var recordedSchemaDriftResponse []byte

type testUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
}

func (*testUpload) Close() error                                       { return nil }
func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

type testSecrets map[string]string

func (secrets testSecrets) ResolveSecret(_ context.Context, name string) (string, error) {
	value, ok := secrets[name]
	if !ok {
		return "", errors.New("missing test secret")
	}
	return value, nil
}

func TestClientRendersDoclingPagesAndRequestsBothFormats(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	var polls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/convert/file/async":
			assert.Equal(t, "synthetic-secret", request.Header.Get("X-Api-Key"))
			assertDoclingSubmission(t, request, fixture.metadata, fixture.source)
			writeJSON(t, response, doclingTask("task-1", "pending"))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/status/poll/task-1":
			if polls.Add(1) == 1 {
				writeJSON(t, response, doclingTask("task-1", "started"))
				return
			}
			writeJSON(t, response, doclingTask("task-1", "success"))
		case request.Method == http.MethodGet && request.URL.Path == "/v1/result/task-1":
			writeRecordedJSON(t, response, recordedPagesResponse)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newClient(t, server.URL, fixture.descriptor, testSecrets{"docling-api": "synthetic-secret"}, http.DefaultClient)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
	assert.Equal(t, "first page", result.Evidence.Units[0].Text)
	assert.Equal(t, int64(1), result.Evidence.Units[0].Locator.Start)
	assert.Equal(t, "second page", result.Evidence.Units[1].Text)
	assert.Equal(t, "# Synthetic report\n", string(result.ProviderMarkdown))
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, document.EvidenceArtifactStructured, result.Artifacts[0].Role)
	assert.Equal(t, int64(2), polls.Load())
}

func TestClientRequiresConvertTasksAndOfficialStatuses(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "missing task type", body: `{"task_id":"task-1","task_status":"success"}`},
		{name: "wrong task type", body: `{"task_id":"task-1","task_type":"classify","task_status":"success"}`},
		{name: "undocumented running status", body: `{"task_id":"task-1","task_type":"convert","task_status":"running"}`},
		{name: "undocumented queued status", body: `{"task_id":"task-1","task_type":"convert","task_status":"queued"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseTask([]byte(testCase.body))
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
		})
	}
}

func TestTaskIDsFitDocbankReceiptTokens(t *testing.T) {
	for _, taskID := range []string{"UPPER", ".", "..", strings.Repeat("a", 121)} {
		_, err := parseTask([]byte(`{"task_id":"` + taskID + `","task_type":"convert","task_status":"success"}`))
		require.Error(t, err, taskID)
	}
}

func TestMapEvidenceUsesContiguousPageRegistryAndNeverDropsText(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{name: "blank registered page", want: true, raw: map[string]any{"schema_name": "DoclingDocument", "version": "1.7.0", "pages": map[string]any{"1": map[string]any{}, "2": map[string]any{}}, "texts": []any{map[string]any{"text": "one", "prov": []any{map[string]any{"page_no": 1}}}}}},
		{name: "page gap", want: false, raw: map[string]any{"schema_name": "DoclingDocument", "version": "1.7.0", "pages": map[string]any{"1": map[string]any{}, "3": map[string]any{}}, "texts": []any{}}},
		{name: "unlocated text", want: false, raw: map[string]any{"schema_name": "DoclingDocument", "version": "1.7.0", "pages": map[string]any{"1": map[string]any{}}, "texts": []any{map[string]any{"text": "one"}}}},
		{name: "cross page provenance", want: false, raw: map[string]any{"schema_name": "DoclingDocument", "version": "1.7.0", "pages": map[string]any{"1": map[string]any{}, "2": map[string]any{}}, "texts": []any{map[string]any{"text": "one", "prov": []any{map[string]any{"page_no": 1}, map[string]any{"page_no": 2}}}}}},
		{name: "v2 drift", want: false, raw: map[string]any{"schema_name": "DoclingDocument", "version": "2.0.0", "pages": map[string]any{"1": map[string]any{}}, "texts": []any{}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw, err := json.Marshal(testCase.raw)
			require.NoError(t, err)
			evidence, _, usable := mapEvidence(raw, "pdf")
			assert.Equal(t, testCase.want, usable)
			if usable {
				require.Len(t, evidence.Units, 2)
				assert.Empty(t, evidence.Units[1].Text)
			}
		})
	}
}

func TestClientRejectsChangedTaskIDWhilePolling(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			writeJSON(t, response, doclingTask("original", "pending"))
		case pollPath + "original":
			writeJSON(t, response, doclingTask("substituted", "success"))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
}

func TestClientStopsAtAuthorizationExpiryBeforeEgress(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "expiry.pdf", []byte("synthetic PDF bytes"))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.NotFound(response, request)
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	fixture.authorization.ExpiresAt = time.Now().UTC().Add(15 * time.Millisecond).Format(timestampForm)
	upload := &testUpload{Reader: delayedReader{Reader: bytes.NewReader(fixture.source), delay: 30 * time.Millisecond}, metadata: fixture.metadata}
	_, err := document.RenderRendition(t.Context(), client, upload, fixture.authorization)
	require.ErrorContains(t, err, "authorization is not current")
	assert.Zero(t, requests.Load())
}

func TestClientStopsAtAuthorizationExpiryBeforeFollowupEgress(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "expiry.pdf", []byte("synthetic PDF bytes"))
	var submits, polls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			submits.Add(1)
			time.Sleep(30 * time.Millisecond)
			writeJSON(t, response, doclingTask("expiry", "pending"))
		case pollPath + "expiry":
			polls.Add(1)
			writeJSON(t, response, doclingTask("expiry", "success"))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	fixture.authorization.ExpiresAt = time.Now().UTC().Add(15 * time.Millisecond).Format(timestampForm)
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.ErrorContains(t, err, "authorization is not current")
	assert.Equal(t, int64(1), submits.Load())
	assert.Zero(t, polls.Load())
}

func TestClientClassifiesHTTPStatusByOperationBeforeContentType(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "status.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name      string
		operation string
		status    int
		want      document.RenditionErrorCode
	}{
		{name: "submit 401", operation: "submit", status: http.StatusUnauthorized, want: document.RenditionErrorAuthentication},
		{name: "submit 429", operation: "submit", status: http.StatusTooManyRequests, want: document.RenditionErrorRateLimited},
		{name: "submit 503", operation: "submit", status: http.StatusServiceUnavailable, want: document.RenditionErrorTransient},
		{name: "submit 404", operation: "submit", status: http.StatusNotFound, want: document.RenditionErrorMalformedEvidence},
		{name: "submit 400", operation: "submit", status: http.StatusBadRequest, want: document.RenditionErrorPolicyRejected},
		{name: "submit 413", operation: "submit", status: http.StatusRequestEntityTooLarge, want: document.RenditionErrorPolicyRejected},
		{name: "submit 415", operation: "submit", status: http.StatusUnsupportedMediaType, want: document.RenditionErrorPolicyRejected},
		{name: "submit 422", operation: "submit", status: http.StatusUnprocessableEntity, want: document.RenditionErrorPolicyRejected},
		{name: "poll 404", operation: "poll", status: http.StatusNotFound, want: document.RenditionErrorUnknownJob},
		{name: "poll 410", operation: "poll", status: http.StatusGone, want: document.RenditionErrorUnknownJob},
		{name: "result 404", operation: "result", status: http.StatusNotFound, want: document.RenditionErrorUnknownJob},
		{name: "result 410", operation: "result", status: http.StatusGone, want: document.RenditionErrorUnknownJob},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if testCase.operation == "submit" && request.URL.Path == convertPath ||
					testCase.operation == "poll" && request.URL.Path == pollPath+"status" ||
					testCase.operation == "result" && request.URL.Path == resultPath+"status" {
					response.Header().Set("Content-Type", "text/html")
					response.WriteHeader(testCase.status)
					_, err := io.WriteString(response, "provider-private-body")
					assert.NoError(t, err)
					return
				}
				switch request.URL.Path {
				case convertPath:
					if testCase.operation == "poll" {
						writeJSON(t, response, doclingTask("status", "pending"))
					} else {
						writeJSON(t, response, doclingTask("status", "success"))
					}
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
			_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, testCase.want, providerErr.Code())
			assert.NotContains(t, err.Error(), "provider-private-body")
		})
	}
}

func TestClientTreatsFailedUploadTransportAsAmbiguousSubmission(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "ambiguous.pdf", []byte("synthetic PDF bytes"))
	var consumed atomic.Bool
	client := newClient(t, "http://127.0.0.1", fixture.descriptor, nil, &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		_, err := io.Copy(io.Discard, request.Body)
		require.NoError(t, err)
		consumed.Store(true)
		return nil, errors.New("synthetic upload transport failure")
	})})
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorAmbiguousSubmission, providerErr.Code())
	assert.True(t, consumed.Load())
}

func TestClientRecoversKnownTaskWithoutResubmitting(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "recovery.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name string
		path string
	}{
		{name: "poll", path: pollPath + "recovery"},
		{name: "result", path: resultPath + "recovery"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var submits, failures atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case convertPath:
					submits.Add(1)
					if testCase.name == "poll" {
						writeJSON(t, response, doclingTask("recovery", "pending"))
					} else {
						writeJSON(t, response, doclingTask("recovery", "success"))
					}
				case testCase.path:
					if failures.Add(1) == 1 {
						response.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if testCase.name == "poll" {
						writeJSON(t, response, doclingTask("recovery", "success"))
					} else {
						writeJSON(t, response, doclingResultResponse(fixture.metadata.Filename, "# recovery\n", []any{
							map[string]any{"text": "page", "prov": []any{map[string]any{"page_no": 1}}},
						}))
					}
				case resultPath + "recovery":
					writeJSON(t, response, doclingResultResponse(fixture.metadata.Filename, "# recovery\n", []any{
						map[string]any{"text": "page", "prov": []any{map[string]any{"page_no": 1}}},
					}))
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
			_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
			require.NoError(t, err)
			assert.Equal(t, int64(1), submits.Load())
			assert.Equal(t, int64(2), failures.Load())
		})
	}
}

func TestClientReturnsAmbiguousSubmissionWhenKnownTaskRecoveryExhausts(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "recovery.pdf", []byte("synthetic PDF bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			writeJSON(t, response, doclingTask("recovery", "pending"))
		case pollPath + "recovery":
			response.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	_, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorAmbiguousSubmission, providerErr.Code())
}

func TestClientReceiptCountsEveryResponseBodyAndRequest(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "usage.pdf", []byte("synthetic PDF bytes"))
	var outputBytes atomic.Int64
	var polls atomic.Int64
	writeCountedJSON := func(t *testing.T, response http.ResponseWriter, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		require.NoError(t, err)
		body = append(body, '\n')
		outputBytes.Add(int64(len(body)))
		response.Header().Set("Content-Type", "application/json")
		_, err = response.Write(body)
		require.NoError(t, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case convertPath:
			writeCountedJSON(t, response, doclingTask("usage", "pending"))
		case pollPath + "usage":
			if polls.Add(1) == 1 {
				body := []byte("temporary provider body")
				outputBytes.Add(int64(len(body)))
				response.WriteHeader(http.StatusServiceUnavailable)
				_, err := response.Write(body)
				assert.NoError(t, err)
				return
			}
			writeCountedJSON(t, response, doclingTask("usage", "success"))
		case resultPath + "usage":
			writeCountedJSON(t, response, doclingResultResponse(fixture.metadata.Filename, "# usage\n", []any{
				map[string]any{"text": "page", "prov": []any{map[string]any{"page_no": 1}}},
			}))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, int64(4), result.Receipt.Usage.Requests)
	assert.Equal(t, int64(1), result.Receipt.Usage.Retries)
	assert.Equal(t, outputBytes.Load(), result.Receipt.Usage.OutputBytes)
}

func TestClientRequestCountsPartialResponseBytesBeforeReadFailure(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "usage.pdf", []byte("synthetic PDF bytes"))
	payload := []byte("partial provider response")
	client := newClient(t, "http://127.0.0.1", fixture.descriptor, nil, &http.Client{
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(&readOnceError{payload: payload}),
			}, nil
		}),
	})
	usage := &requestUsage{}
	_, _, err := client.request(t.Context(), time.Now().Add(time.Minute), usage, http.MethodGet, resultPath+"usage", "", nil)
	require.Error(t, err)
	assert.Equal(t, int64(1), usage.requests)
	assert.Equal(t, int64(len(payload)), usage.outputBytes)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type readOnceError struct {
	payload []byte
}

func (reader *readOnceError) Read(buffer []byte) (int, error) {
	if len(reader.payload) == 0 {
		return 0, errors.New("synthetic response read failure")
	}
	n := copy(buffer, reader.payload)
	reader.payload = reader.payload[n:]
	return n, errors.New("synthetic response read failure")
}

type delayedReader struct {
	io.Reader

	delay time.Duration
}

func (reader delayedReader) Read(buffer []byte) (int, error) {
	time.Sleep(reader.delay)
	return reader.Reader.Read(buffer)
}

func TestClientHandlesPartialSuccessAndSanitizesTerminalFailures(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	t.Run("partial success", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case convertPath:
				writeJSON(t, response, doclingTask("partial", "partial_success"))
			case resultPath + "partial":
				writeJSON(t, response, doclingResultResponse(fixture.metadata.Filename, "# report\n", []any{
					map[string]any{"text": "page", "prov": []any{map[string]any{"page_no": 1}}},
				}, withResultErrors([]any{map[string]any{"page_no": 1}})))
			default:
				http.NotFound(response, request)
			}
		}))
		t.Cleanup(server.Close)
		client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
		result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
		require.NoError(t, err)
		assert.Equal(t, document.EvidencePartial, result.Evidence.Completeness)
		assert.Contains(t, result.Receipt.Warnings, "partial_success")
		require.NotEmpty(t, result.Evidence.Units[0].Omissions)
	})
	t.Run("partial success result wrapper", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case convertPath:
				writeJSON(t, response, doclingTask("wrapper-partial", "success"))
			case resultPath + "wrapper-partial":
				writeJSON(t, response, doclingResultResponse("report.pdf", "# report\n", []any{
					map[string]any{"text": "page", "prov": []any{map[string]any{"page_no": 1}}},
				}, withResultStatus("partial_success"), withResultErrors([]any{map[string]any{"page_no": 1}})))
			default:
				http.NotFound(response, request)
			}
		}))
		t.Cleanup(server.Close)
		client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
		result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
		require.NoError(t, err)
		assert.Equal(t, document.EvidencePartial, result.Evidence.Completeness)
		assert.Contains(t, result.Receipt.Warnings, "partial_success")
		require.NotEmpty(t, result.Evidence.Units[0].Omissions)
	})
	for _, testCase := range []struct {
		status string
		want   document.RenditionErrorCode
	}{
		{status: "failure", want: document.RenditionErrorMalformedEvidence},
		{status: "skipped", want: document.RenditionErrorPolicyRejected},
	} {
		t.Run(testCase.status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				writeJSON(t, response, map[string]any{
					"task_id": "terminal", "task_type": "convert", "task_status": testCase.status,
					"errors": []string{"provider-body-secret"},
				})
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, testCase.want, providerErr.Code())
			assert.NotContains(t, err.Error(), "provider-body-secret")
		})
	}
}

func TestClientPublishesPartialEvidenceOnlyForExactPDFPageOmissions(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "partial.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name    string
		family  string
		errors  []any
		wantErr bool
	}{
		{name: "exact page omission", errors: []any{map[string]any{"page_no": 1, "message": "private"}, map[string]any{"page_no": 1}}},
		{name: "no errors", wantErr: true},
		{name: "document scoped error", errors: []any{map[string]any{"message": "private"}}, wantErr: true},
		{name: "unknown page", errors: []any{map[string]any{"page_no": 3}}, wantErr: true},
		{name: "non PDF", family: "text", errors: []any{map[string]any{"page_no": 1}}, wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := fixture
			if testCase.family != "" {
				candidate = newFixture(t, testCase.family, "text/plain", "partial.txt", []byte("synthetic text bytes"))
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case convertPath:
					writeJSON(t, response, doclingTask("partial", "partial_success"))
				case resultPath + "partial":
					writeJSON(t, response, doclingResultResponse(candidate.metadata.Filename, "# partial\n", []any{
						map[string]any{"text": "one", "prov": []any{map[string]any{"page_no": 1}}},
					}, withResultErrors(testCase.errors)))
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, candidate.descriptor, nil, http.DefaultClient)
			result, err := document.RenderRendition(t.Context(), client, candidate.upload(), candidate.authorization)
			if testCase.wantErr {
				require.Error(t, err)
				providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
				require.True(t, ok)
				assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, document.EvidencePartial, result.Evidence.Completeness)
			assert.Empty(t, result.Evidence.Omissions)
			require.Len(t, result.Evidence.Units[0].Omissions, 1)
			assert.Equal(t, "provider_output", result.Evidence.Units[0].Omissions[0].Field)
			assert.Empty(t, result.Evidence.Units[1].Omissions)
		})
	}
}

func TestClientRejectsResultWrapperIdentityAndStatusFailures(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "report.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name   string
		result map[string]any
		want   document.RenditionErrorCode
	}{
		{name: "missing wrapper filename", result: doclingResultResponse("", "# report\n", nil), want: document.RenditionErrorPolicyRejected},
		{name: "conflicting inner filename", result: doclingResultResponseWithInnerFilename("report.pdf", "other.pdf", "# report\n", nil), want: document.RenditionErrorPolicyRejected},
		{name: "failed wrapper status", result: doclingResultResponse("report.pdf", "# report\n", nil, withResultStatus("failure")), want: document.RenditionErrorMalformedEvidence},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case convertPath:
					writeJSON(t, response, doclingTask("wrapper", "success"))
				case resultPath + "wrapper":
					writeJSON(t, response, testCase.result)
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, testCase.want, providerErr.Code())
		})
	}
}

func TestMapEvidenceOnlyClaimsEstablishedNaturalProvenance(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"schema_name": "DoclingDocument", "version": "1.7.0",
		"pages": map[string]any{"1": map[string]any{}},
		"texts": []any{map[string]any{"text": "one", "prov": []any{map[string]any{"page_no": 1}}}},
	})
	require.NoError(t, err)
	for _, testCase := range []struct {
		family string
		usable bool
		kind   document.EvidenceUnitKind
	}{
		{family: "pdf", usable: true, kind: document.EvidenceUnitPage},
		{family: "word"}, {family: "presentation"}, {family: "spreadsheet"},
		{family: "ebook"}, {family: "structured"}, {family: "source"}, {family: "text"}, {family: "mail"},
	} {
		t.Run(testCase.family, func(t *testing.T) {
			evidence, _, usable := mapEvidence(raw, testCase.family)
			assert.Equal(t, testCase.usable, usable)
			if usable {
				assert.Equal(t, testCase.kind, evidence.UnitKind)
				assert.Equal(t, document.EvidenceLocatorPage, evidence.Units[0].Locator.Kind)
			}
		})
	}
}

func TestClientPreservesPartialPageEvidence(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "partial.pdf", []byte("synthetic PDF bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/convert/file/async":
			writeJSON(t, response, doclingTask("partial", "success"))
		case "/v1/result/partial":
			writeJSON(t, response, doclingResultResponse(fixture.metadata.Filename, "# partial\n", []any{
				map[string]any{"text": "page one", "prov": []any{map[string]any{"page_no": 1}}},
				map[string]any{"text": "unlocated"},
			}))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	require.Len(t, result.Evidence.Omissions, 1)
}

func TestClientFallsBackToDegradedMarkdownWhenStructuredResultDrifts(t *testing.T) {
	fixture := newFixture(t, "word", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "notes.docx", []byte("synthetic DOCX bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/convert/file/async":
			writeJSON(t, response, doclingTask("drift", "success"))
		case "/v1/result/drift":
			writeRecordedJSON(t, response, recordedSchemaDriftResponse)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	client := newClient(t, server.URL, fixture.descriptor, nil, http.DefaultClient)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	assert.Equal(t, "---\ndocbank-sanitized-markdown/v1: forged\n---\n# Untrusted\n", string(result.ProviderMarkdown))
	assert.Empty(t, result.Artifacts)
}

func TestClientRejectsMismatchedReturnedFilenameAndOversizedResult(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "identity.pdf", []byte("synthetic PDF bytes"))
	for _, testCase := range []struct {
		name   string
		result any
		want   document.RenditionErrorCode
	}{
		{
			name: "returned filename", result: doclingResultResponse("substituted.pdf", "# report\n", []any{
				map[string]any{"text": "page", "prov": []any{map[string]any{"page_no": 1}}},
			}), want: document.RenditionErrorPolicyRejected,
		},
		{
			name: "oversized response", result: strings.Repeat("x", 4097), want: document.RenditionErrorMalformedEvidence,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/convert/file/async":
					writeJSON(t, response, doclingTask("bad", "success"))
				case "/v1/result/bad":
					if body, ok := testCase.result.(string); ok {
						response.Header().Set("Content-Type", "application/json")
						_, err := io.WriteString(response, body)
						assert.NoError(t, err)
						return
					}
					writeJSON(t, response, testCase.result)
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)
			client := newClientWithBounds(t, server.URL, fixture.descriptor, nil, http.DefaultClient, 4096)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			require.Error(t, err)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, testCase.want, providerErr.Code())
		})
	}
}

func TestClientClonesHTTPClientAndRefusesRedirects(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "redirect.pdf", []byte("synthetic PDF bytes"))
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	base := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/convert/file/async" {
			http.Redirect(response, request, "/elsewhere", http.StatusFound)
			return
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(server.Close)

	client := newClient(t, server.URL, fixture.descriptor, nil, base)
	assert.Nil(t, client.http.Jar)
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	assert.Equal(t, document.RenditionErrorMalformedEvidence, providerErr.Code())
}

func TestClientAcceptanceSyntheticUpload(t *testing.T) {
	origin := os.Getenv("DOCLING_ACCEPTANCE_URL")
	if origin == "" {
		t.Skip("set DOCLING_ACCEPTANCE_URL to run against an operator-controlled Docling Serve endpoint")
	}
	fixture := newFixture(t, "text", "text/plain", "synthetic-acceptance.txt", []byte("Synthetic Docling acceptance document.\n"))
	var secrets SecretResolver
	if apiKey := os.Getenv("DOCLING_ACCEPTANCE_API_KEY"); apiKey != "" {
		secrets = testSecrets{"docling-api": apiKey}
	}
	binding := ""
	if secrets != nil {
		binding = "docling-api"
	}
	client, err := New(Profile{
		Origin: origin, Descriptor: fixture.descriptor, SecretBinding: binding,
		RequestTimeout: 30 * time.Second, TotalTimeout: 10 * time.Minute, PollInterval: time.Second,
		MaxPollAttempts: 300, MaxResponseBytes: 64 << 20, MaxDocumentBytes: 1 << 20,
	}, secrets, http.DefaultClient)
	require.NoError(t, err)
	result, err := document.RenderRendition(t.Context(), client, fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, fixture.authorization.SourceSHA256, result.Receipt.SourceSHA256)
	assert.NotEmpty(t, result.Evidence.Units)
}

type fixture struct {
	descriptor    document.RenditionDescriptor
	metadata      document.AuthorizedUploadMetadata
	authorization document.RenditionAuthorization
	source        []byte
}

func newFixture(t *testing.T, family, mediaType, filename string, source []byte) fixture {
	t.Helper()
	descriptor, err := document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: "docling.serve-v1", ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: strings.Repeat("1", 64), TrustBoundary: document.RenditionTrustOperatorNetwork,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsMarkdown: true, ReturnsStructured: true,
		ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
	require.NoError(t, err)
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: family, MediaType: mediaType, ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64), ProviderMetadataChecksum: strings.Repeat("3", 64), InputKind: document.RenditionInputOriginalFile,
	}
	started := time.Now().UTC().Add(-time.Minute)
	return fixture{descriptor: descriptor, metadata: metadata, source: source, authorization: document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint, PolicyFingerprint: descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64), SourceSHA256: metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily: family, MediaType: mediaType, InputKind: document.RenditionInputOriginalFile,
		DiscloseFilename:     true,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, MaxProviderMarkdownBytes: 4096,
		MaxArtifactBytes: 4096, MaxArtifacts: 1, MaxTotalResultBytes: 16384,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"), ExpiresAt: started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}}
}

func (fixture fixture) upload() document.AuthorizedUpload {
	return &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

func newClient(t *testing.T, origin string, descriptor document.RenditionDescriptor, secrets SecretResolver, httpClient *http.Client) *Client {
	t.Helper()
	return newClientWithBounds(t, origin, descriptor, secrets, httpClient, 1<<20)
}

func newClientWithBounds(t *testing.T, origin string, descriptor document.RenditionDescriptor, secrets SecretResolver, httpClient *http.Client, maxResponseBytes int64) *Client {
	t.Helper()
	binding := ""
	if secrets != nil {
		binding = "docling-api"
	}
	client, err := New(Profile{
		Origin: origin, Descriptor: descriptor, SecretBinding: binding,
		RequestTimeout: time.Second, TotalTimeout: 2 * time.Second, PollInterval: time.Millisecond,
		MaxPollAttempts: 4, MaxResponseBytes: maxResponseBytes,
	}, secrets, httpClient)
	require.NoError(t, err)
	return client
}

func assertDoclingSubmission(t *testing.T, request *http.Request, metadata document.AuthorizedUploadMetadata, source []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, params["boundary"])
	file, err := reader.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "files", file.FormName())
	assert.Equal(t, metadata.Filename, file.FileName())
	gotSource, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, source, gotSource)
	formats := make([]string, 0, 2)
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		require.NoError(t, partErr)
		value, readErr := io.ReadAll(part)
		require.NoError(t, readErr)
		switch part.FormName() {
		case "to_formats":
			formats = append(formats, string(value))
		case "target_type":
			assert.Equal(t, "inbody", string(value))
		default:
			t.Errorf("unexpected form field %q", part.FormName())
		}
	}
	assert.Equal(t, []string{"md", "json"}, formats)
}

type resultOption func(map[string]any)

func withResultStatus(status string) resultOption {
	return func(result map[string]any) { result["status"] = status }
}

func withResultErrors(errors []any) resultOption {
	return func(result map[string]any) { result["errors"] = errors }
}

func doclingTask(taskID, status string) map[string]any {
	return map[string]any{"task_id": taskID, "task_type": "convert", "task_status": status}
}

func doclingResultResponse(filename, markdown string, texts []any, options ...resultOption) map[string]any {
	return doclingResultResponseWithInnerFilename(filename, filename, markdown, texts, options...)
}

func doclingResultResponseWithInnerFilename(
	filename, innerFilename, markdown string, texts []any, options ...resultOption,
) map[string]any {
	result := map[string]any{
		"status": "success", "processing_time": 0.01, "errors": []string{},
		"document": map[string]any{
			"filename": filename, "md_content": markdown,
			"json_content": map[string]any{
				"schema_name": "DoclingDocument", "version": "1.7.0",
				"origin": map[string]any{"filename": innerFilename}, "pages": map[string]any{"1": map[string]any{}, "2": map[string]any{}}, "texts": texts,
			},
			"html_content": "", "text_content": "",
		},
	}
	for _, option := range options {
		option(result)
	}
	return result
}

func writeJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(response).Encode(value))
}

func writeRecordedJSON(t *testing.T, response http.ResponseWriter, value []byte) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	_, err := response.Write(value)
	require.NoError(t, err)
}
