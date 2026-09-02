package providerutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	// JSONMediaType is the response media type most adapters require.
	JSONMediaType = "application/json"
	// maxRetryAfter is the largest retry hint a classified error may carry.
	maxRetryAfter = 24 * time.Hour
)

// ErrResponseTooLarge marks a response body that exceeded its byte bound.
var ErrResponseTooLarge = errors.New("response exceeds byte limit")

// Executor issues bounded, authorized HTTP requests against one fixed origin.
type Executor struct {
	Provider         Provider
	HTTP             *http.Client
	Origin           string
	RequestTimeout   time.Duration
	MaxResponseBytes int64
	Credential       Credential
	// ResponseMediaType is required of every 2xx body unless the request
	// overrides it; JSONMediaType when empty.
	ResponseMediaType string
}

// Request describes one HTTP exchange.
type Request struct {
	Stage       Stage
	Method      string
	Path        string
	ContentType string
	Body        []byte
	// Upload streams a multipart body instead of Body and ContentType.
	Upload  *MultipartUpload
	Headers map[string]string
	// MaxResponseBytes tightens the executor bound when positive.
	MaxResponseBytes  int64
	ResponseMediaType string
}

// Response is one fully read, bounded provider response.
type Response struct {
	Status int
	Header http.Header
	// ContentLength is the declared body length, or -1 when unknown.
	ContentLength int64
	Body          []byte
	RetryAfter    time.Duration
}

// Success reports whether the status is 2xx.
func (response Response) Success() bool {
	return response.Status >= http.StatusOK && response.Status < http.StatusMultipleChoices
}

// Do performs one request. It returns an error only when the exchange could
// not be completed or a 2xx body is unusable; a non-2xx response with a
// complete body returns nil so the caller can classify it, usually through
// Provider.StatusError. Submission-stage failures other than caller
// cancellation are wrapped as ambiguous submissions.
func (executor *Executor) Do(operation *Operation, usage *Usage, request Request) (Response, error) {
	if err := operation.Check(); err != nil {
		return Response{}, err
	}
	requestCtx, cancel := context.WithTimeout(operation.Context(), executor.RequestTimeout)
	defer cancel()
	httpRequest, completion, err := executor.build(requestCtx, request)
	if err != nil {
		return Response{}, err
	}
	if completion != nil {
		stopClose := context.AfterFunc(requestCtx, func() { completion.close(requestCtx.Err()) })
		defer stopClose()
	}
	if err := executor.prepare(operation, httpRequest); err != nil {
		abortUpload(completion, err)
		return Response{}, err
	}
	usage.Requests++
	httpResponse, err := executor.HTTP.Do(httpRequest)
	if err != nil {
		abortUpload(completion, err)
		return Response{}, executor.failure(operation, request.Stage, "request failed", err)
	}
	defer func() { _ = httpResponse.Body.Close() }()
	response := Response{
		Status: httpResponse.StatusCode, Header: httpResponse.Header, ContentLength: httpResponse.ContentLength,
		RetryAfter: ParseRetryAfter(httpResponse.Header),
	}
	limit := executor.MaxResponseBytes
	if request.MaxResponseBytes > 0 {
		limit = min(limit, request.MaxResponseBytes)
	}
	response.Body, err = ReadBounded(httpResponse.Body, limit)
	usage.OutputBytes += int64(len(response.Body))
	if err != nil {
		abortUpload(completion, err)
		return executor.readFailure(operation, request.Stage, response, err)
	}
	if err := operation.Check(); err != nil {
		abortUpload(completion, err)
		return response, executor.submissionFailure(operation, request.Stage, err)
	}
	if !response.Success() {
		abortUpload(completion, errors.New("provider rejected the submission"))
		return response, nil
	}
	if completion != nil {
		if uploadErr := completion.wait(requestCtx); uploadErr != nil {
			return response, executor.failure(operation, request.Stage, "upload did not complete", uploadErr)
		}
	}
	if err := executor.validateBody(request, response); err != nil {
		return response, executor.submissionFailure(operation, request.Stage, err)
	}
	return response, nil
}

func (executor *Executor) build(
	ctx context.Context, request Request,
) (*http.Request, *multipartCompletion, error) {
	var body io.Reader = bytes.NewReader(request.Body)
	var completion *multipartCompletion
	contentType := request.ContentType
	if request.Upload != nil {
		completion, contentType = request.Upload.start()
		body = completion.reader
	}
	httpRequest, err := http.NewRequestWithContext(ctx, request.Method, executor.Origin+request.Path, body)
	if err != nil {
		abortUpload(completion, err)
		return nil, nil, executor.Provider.Classified(document.RenditionErrorTransient,
			"could not create "+string(executor.Provider)+" request", err)
	}
	httpRequest.Header.Set("Accept", executor.responseMediaType(request))
	if contentType != "" {
		httpRequest.Header.Set("Content-Type", contentType)
	}
	for name, value := range request.Headers {
		httpRequest.Header.Set(name, value)
	}
	return httpRequest, completion, nil
}

func (executor *Executor) prepare(operation *Operation, httpRequest *http.Request) error {
	if err := executor.Credential.Authorize(executor.Provider, httpRequest); err != nil {
		return err
	}
	return operation.Check()
}

func abortUpload(completion *multipartCompletion, cause error) {
	if completion != nil {
		_ = completion.abort(cause)
	}
}

func (executor *Executor) responseMediaType(request Request) string {
	if request.ResponseMediaType != "" {
		return request.ResponseMediaType
	}
	if executor.ResponseMediaType != "" {
		return executor.ResponseMediaType
	}
	return JSONMediaType
}

// failure classifies a request that produced no usable response: the
// operation's own end wins, then a transient transport failure.
func (executor *Executor) failure(operation *Operation, stage Stage, message string, cause error) error {
	err := operation.Check()
	if err == nil {
		err = executor.Provider.Classified(document.RenditionErrorTransient,
			string(executor.Provider)+" "+message, cause)
	}
	return executor.submissionFailure(operation, stage, err)
}

// submissionFailure wraps a failure as ambiguous when provider work may have
// started without a usable answer. Caller cancellation and a terminal
// malformed synchronous result are returned as they are.
func (executor *Executor) submissionFailure(operation *Operation, stage Stage, err error) error {
	if stage == StageJob || operation.CallerDone() {
		return err
	}
	if providerErr, ok := errors.AsType[*document.RenditionProviderError](err); ok &&
		stage == StageResult && providerErr.Code() == document.RenditionErrorMalformedEvidence {
		return err
	}
	return executor.Provider.AmbiguousSubmission(err)
}

func (executor *Executor) readFailure(
	operation *Operation, stage Stage, response Response, cause error,
) (Response, error) {
	response.Body = nil
	err := executor.Provider.Classified(document.RenditionErrorTransient,
		"could not read "+string(executor.Provider)+" response", cause)
	if errors.Is(cause, ErrResponseTooLarge) {
		err = executor.Provider.Malformed(string(executor.Provider)+" response exceeds byte limit", cause)
	} else if operationErr := operation.Check(); operationErr != nil {
		err = operationErr
	}
	if !response.Success() {
		return response, executor.Provider.StatusError(stage, response.Status, response.RetryAfter, err)
	}
	return response, executor.submissionFailure(operation, stage, err)
}

func (executor *Executor) validateBody(request Request, response Response) error {
	want := executor.responseMediaType(request)
	if err := executor.Provider.RequireMediaType(response.Header.Get("Content-Type"), want); err != nil {
		return err
	}
	if isJSONMediaType(want) && !utf8.Valid(response.Body) {
		return executor.Provider.Malformed(string(executor.Provider)+" response JSON is not valid UTF-8", nil)
	}
	return nil
}

func isJSONMediaType(mediaType string) bool {
	mediaType, _, _ = strings.Cut(mediaType, ";")
	return mediaType == JSONMediaType || strings.HasSuffix(mediaType, "+json")
}

// RequireMediaType checks a Content-Type against the protocol media type.
// Parameters are compared only when the wanted type declares them.
func (provider Provider) RequireMediaType(got, want string) error {
	gotType, gotParams, err := mime.ParseMediaType(got)
	if err != nil {
		return provider.Malformed(string(provider)+" response content type is invalid", err)
	}
	wantType, wantParams, err := mime.ParseMediaType(want)
	if err != nil || gotType != wantType || len(wantParams) != 0 && !reflect.DeepEqual(gotParams, wantParams) {
		return provider.Malformed(string(provider)+" response content type does not match protocol", err)
	}
	return nil
}

// ReadBounded reads at most maximum bytes and reports ErrResponseTooLarge
// when the body continues past the bound.
func ReadBounded(reader io.Reader, maximum int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return value, err
	}
	if int64(len(value)) > maximum {
		return value[:maximum], ErrResponseTooLarge
	}
	return value, nil
}

// ParseRetryAfter reads a Retry-After header given either as delay seconds
// or as an HTTP-date. A missing, malformed, or past value yields zero; the
// result never exceeds the 24-hour bound of a classified error.
func ParseRetryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return min(max(time.Until(when), 0), maxRetryAfter)
}
