package providerutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

const testProvider = Provider("Synthetic")

func TestParseRetryAfterAcceptsSecondsAndHTTPDate(t *testing.T) {
	header := func(value string) http.Header { return http.Header{"Retry-After": []string{value}} }
	assert.Equal(t, 120*time.Second, ParseRetryAfter(header("120")))
	assert.Equal(t, maxRetryAfter, ParseRetryAfter(header("999999999")))

	future := ParseRetryAfter(header(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)))
	assert.Greater(t, future, 80*time.Second)
	assert.LessOrEqual(t, future, 90*time.Second)

	assert.Zero(t, ParseRetryAfter(header(time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))))
	assert.Zero(t, ParseRetryAfter(header("soon")))
	assert.Zero(t, ParseRetryAfter(header("-5")))
	assert.Zero(t, ParseRetryAfter(http.Header{}))
}

func TestRequireMediaTypeAcceptsOnlyTheRequestedWildcardFamily(t *testing.T) {
	require.NoError(t, testProvider.RequireMediaType("image/png", "image/*"))
	assertCode(t, testProvider.RequireMediaType("application/json", "image/*"),
		document.RenditionErrorMalformedEvidence)
}

func TestStatusErrorUsesOneTableForEveryStage(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		stage     Stage
		status    int
		want      document.RenditionErrorCode
		wantCause document.RenditionErrorCode
		retry     time.Duration
	}{
		{name: "submission 503", stage: StageSubmission, status: http.StatusServiceUnavailable,
			want: document.RenditionErrorAmbiguousSubmission, wantCause: document.RenditionErrorCapacity, retry: 3 * time.Second},
		{name: "job 503", stage: StageJob, status: http.StatusServiceUnavailable, want: document.RenditionErrorCapacity, retry: 3 * time.Second},
		{name: "submission 429", stage: StageSubmission, status: http.StatusTooManyRequests, want: document.RenditionErrorRateLimited, retry: time.Second},
		{name: "submission 415", stage: StageSubmission, status: http.StatusUnsupportedMediaType, want: document.RenditionErrorUnsupportedInput},
		{name: "job 415", stage: StageJob, status: http.StatusUnsupportedMediaType, want: document.RenditionErrorMalformedEvidence},
		{name: "submission 500", stage: StageSubmission, status: http.StatusInternalServerError,
			want: document.RenditionErrorAmbiguousSubmission, wantCause: document.RenditionErrorTransient},
		{name: "job 404", stage: StageJob, status: http.StatusNotFound, want: document.RenditionErrorUnknownJob},
		{name: "submission 404", stage: StageSubmission, status: http.StatusNotFound, want: document.RenditionErrorMalformedEvidence},
		{name: "submission 401", stage: StageSubmission, status: http.StatusUnauthorized, want: document.RenditionErrorAuthentication, retry: time.Second},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testProvider.StatusError(testCase.stage, testCase.status, testCase.retry, nil)
			providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
			require.True(t, ok)
			assert.Equal(t, testCase.want, providerErr.Code())
			if testCase.wantCause != "" {
				cause, ok := errors.AsType[*document.RenditionProviderError](errors.Unwrap(providerErr))
				require.True(t, ok)
				assert.Equal(t, testCase.wantCause, cause.Code())
				providerErr = cause
			}
			if retryableCode(providerErr.Code()) {
				assert.Equal(t, testCase.retry, providerErr.RetryAfter())
			} else {
				assert.Zero(t, providerErr.RetryAfter())
			}
		})
	}
}

func TestOperationCheckOrdersCallerExpiryAndTotalTimeout(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Minute).Format(TimestampForm)

	t.Run("caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		operation, err := NewOperation(ctx, testProvider, expiry, time.Minute)
		require.NoError(t, err)
		defer operation.Cancel()
		cancel()
		err = operation.Check()
		assertCode(t, err, document.RenditionErrorCanceled)
		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("total timeout", func(t *testing.T) {
		operation, err := NewOperation(t.Context(), testProvider, expiry, time.Millisecond)
		require.NoError(t, err)
		defer operation.Cancel()
		<-operation.Context().Done()
		err = operation.Check()
		assertCode(t, err, document.RenditionErrorCapacity)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("expiry", func(t *testing.T) {
		operation, err := NewOperation(t.Context(), testProvider,
			time.Now().UTC().Add(-time.Second).Format(TimestampForm), time.Minute)
		require.NoError(t, err)
		defer operation.Cancel()
		assertCode(t, operation.Check(), document.RenditionErrorPolicyRejected)
	})

	t.Run("resumed operation uses only a new total timeout", func(t *testing.T) {
		operation := NewResumedOperation(t.Context(), testProvider, time.Millisecond)
		defer operation.Cancel()
		<-operation.Context().Done()
		err := operation.Check()
		assertCode(t, err, document.RenditionErrorCapacity)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestExecutorStreamsUploadAndStopsOnEarlyRejection(t *testing.T) {
	sourceReader, sourceWriter := io.Pipe()
	t.Cleanup(func() { _ = sourceWriter.Close() })
	var interrupted atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		controller := http.NewResponseController(response)
		assert.NoError(t, controller.EnableFullDuplex())
		response.Header().Set("Retry-After", "7")
		response.WriteHeader(http.StatusTooManyRequests)
		assert.NoError(t, controller.Flush())
	}))
	t.Cleanup(server.Close)
	executor := newTestExecutor(server)
	operation, err := NewOperation(t.Context(), testProvider,
		time.Now().UTC().Add(time.Minute).Format(TimestampForm), time.Minute)
	require.NoError(t, err)
	defer operation.Cancel()

	done := make(chan Response, 1)
	go func() {
		response, doErr := executor.Do(operation, &Usage{}, Request{
			Stage: StageSubmission, Method: http.MethodPost, Path: "/submit",
			Upload: &MultipartUpload{
				FieldName: "file", Filename: "blocked.pdf", MediaType: "application/pdf",
				Source: sourceReader, Length: 1 << 20,
				Interrupt: func() error { interrupted.Add(1); return sourceReader.Close() },
			},
		})
		assert.NoError(t, doErr)
		done <- response
	}()
	select {
	case response := <-done:
		assert.Equal(t, http.StatusTooManyRequests, response.Status)
		assert.Equal(t, 7*time.Second, response.RetryAfter)
		assert.Equal(t, int64(1), interrupted.Load())
	case <-time.After(2 * time.Second):
		t.Fatal("early rejection did not release the blocked upload")
	}
}

func TestExecutorDeliversMultipartFieldsAndCountsResponseBytes(t *testing.T) {
	source := []byte("synthetic source bytes")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if !assert.NoError(t, err) {
			return
		}
		reader := multipart.NewReader(request.Body, params["boundary"])
		names := make([]string, 0, 3)
		for part, partErr := reader.NextPart(); partErr == nil; part, partErr = reader.NextPart() {
			names = append(names, part.FormName())
			if part.FileName() != "" {
				got, readErr := io.ReadAll(part)
				assert.NoError(t, readErr)
				assert.Equal(t, source, got)
			}
		}
		assert.Equal(t, []string{"manifest", "file", "mode"}, names)
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		response.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, err = io.WriteString(response, `{"ok":true}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	executor := newTestExecutor(server)
	executor.Credential = BearerCredential("api", resolverFunc(func(context.Context, string) (string, error) {
		return "synthetic-secret", nil
	}))
	operation, err := NewOperation(t.Context(), testProvider,
		time.Now().UTC().Add(time.Minute).Format(TimestampForm), time.Minute)
	require.NoError(t, err)
	defer operation.Cancel()
	upload := &MultipartUpload{
		Prologue:  func(writer *multipart.Writer) error { return writer.WriteField("manifest", "{}") },
		FieldName: "file", Filename: "document.pdf", MediaType: "application/pdf",
		Source: bytes.NewReader(source), Length: int64(len(source)), Fields: [][2]string{{"mode", "fast"}},
	}
	encodedLength, err := upload.EncodedLength()
	require.NoError(t, err)
	var usage Usage

	response, err := executor.Do(operation, &usage, Request{
		Stage: StageSubmission, Method: http.MethodPost, Path: "/submit", Upload: upload,
	})
	require.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(response.Body))
	assert.Equal(t, Usage{Requests: 1, OutputBytes: int64(len(response.Body))}, usage)

	reference := *upload
	reference.Source = bytes.NewReader(source)
	var encoded bytes.Buffer
	writer := multipart.NewWriter(&encoded)
	require.NoError(t, reference.write(writer))
	require.NoError(t, writer.Close())
	assert.Equal(t, int64(encoded.Len()), encodedLength)
}

func TestMultipartUploadRejectsFilenameLineBreaks(t *testing.T) {
	for _, filename := range []string{"report\r.pdf", "report\n.pdf"} {
		upload := MultipartUpload{FieldName: "file", Filename: filename, MediaType: "application/pdf"}
		_, err := upload.EncodedLength()
		require.ErrorContains(t, err, "line break")
	}
}

func TestExecutorCountsPartialBodyAndClassifiesReadFailure(t *testing.T) {
	payload := []byte("partial provider response")
	executor := &Executor{
		Provider: testProvider, Origin: "http://127.0.0.1", RequestTimeout: time.Second, MaxResponseBytes: 1 << 20,
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{JSONMediaType}},
				Body: io.NopCloser(readerFunc(func(buffer []byte) (int, error) {
					return copy(buffer, payload), errors.New("synthetic response read failure")
				})),
			}, nil
		})},
	}
	operation, err := NewOperation(t.Context(), testProvider,
		time.Now().UTC().Add(time.Minute).Format(TimestampForm), time.Minute)
	require.NoError(t, err)
	defer operation.Cancel()
	var usage Usage

	_, err = executor.Do(operation, &usage, Request{Stage: StageJob, Method: http.MethodGet, Path: "/result"})
	assertCode(t, err, document.RenditionErrorTransient)
	assert.Equal(t, Usage{Requests: 1, OutputBytes: int64(len(payload))}, usage)

	_, err = executor.Do(operation, &usage, Request{Stage: StageSubmission, Method: http.MethodPost, Path: "/submit"})
	assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
}

func TestExecutorReportsTruncatedErrorBodiesThroughStatusClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
		_, err := io.WriteString(response, strings.Repeat("x", 64))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	executor := newTestExecutor(server)
	executor.MaxResponseBytes = 8
	operation, err := NewOperation(t.Context(), testProvider,
		time.Now().UTC().Add(time.Minute).Format(TimestampForm), time.Minute)
	require.NoError(t, err)
	defer operation.Cancel()

	response, err := executor.Do(operation, &Usage{}, Request{Stage: StageJob, Method: http.MethodGet, Path: "/job"})
	assertCode(t, err, document.RenditionErrorAuthentication)
	require.ErrorIs(t, err, ErrResponseTooLarge)
	assert.Empty(t, response.Body)
}

func TestValidateJobIDFitsReceiptTokens(t *testing.T) {
	for _, value := range []string{"UPPER", ".", "..", "a.b", strings.Repeat("a", 121), "with/slash", ""} {
		require.Error(t, ValidateJobID(value), value)
	}
	require.NoError(t, ValidateJobID(strings.Repeat("a", 120)))
	require.NoError(t, testProvider.ValidatePathIdentifier("Job.ID-1", "job ID"))
	require.Error(t, testProvider.ValidatePathIdentifier("..", "job ID"))
}

func newTestExecutor(server *httptest.Server) *Executor {
	return &Executor{
		Provider: testProvider, HTTP: server.Client(), Origin: server.URL,
		RequestTimeout: time.Second, MaxResponseBytes: 1 << 20,
	}
}

type resolverFunc func(context.Context, string) (string, error)

func (resolver resolverFunc) ResolveSecret(ctx context.Context, name string) (string, error) {
	return resolver(ctx, name)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type readerFunc func([]byte) (int, error)

func (function readerFunc) Read(buffer []byte) (int, error) { return function(buffer) }

func assertCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerErr, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "%T: %v", err, err)
	assert.Equal(t, want, providerErr.Code())
}
