package mistral

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	// DefaultTimeout is used when ClientConfig.Timeout is zero.
	DefaultTimeout = 5 * time.Minute
	// DefaultMaxRetries is used when ClientConfig.MaxRetries is zero.
	DefaultMaxRetries = 3
	// DefaultMaxRetryDelay is used when ClientConfig.MaxRetryDelay is zero.
	DefaultMaxRetryDelay = 30 * time.Second
	// MaxTimeout is the largest per-attempt timeout accepted by NewClient.
	MaxTimeout = 30 * time.Minute
	// MaxRetries is the largest retry count accepted by NewClient.
	MaxRetries = 10
	// MaxRetryDelay is the largest retry delay accepted by NewClient.
	MaxRetryDelay       = 60 * time.Second
	maxSourceUnitDPI    = 100_000
	maxSourceUnitHeight = 10_000_000
	maxSourceUnitWidth  = 10_000_000
)

var (
	// ErrPermanentResponse marks a provider 4xx other than rate limiting.
	ErrPermanentResponse = errors.New("mistral OCR permanent response")
	// ErrResponseTooLarge marks a response that exceeds the policy limit.
	ErrResponseTooLarge = errors.New("mistral OCR response too large")
	// ErrTransientResponse marks an exhausted retryable provider or transport failure.
	ErrTransientResponse = errors.New("mistral OCR transient response")
	// ErrCapabilityContract marks provider unit behavior that contradicts the
	// evidence used to authorize a format.
	ErrCapabilityContract = errors.New("mistral OCR capability contract changed")
)

// ClientConfig contains bounded operational settings outside policy identity.
type ClientConfig struct {
	APIKey        string
	Timeout       time.Duration
	MaxRetries    int
	MaxRetryDelay time.Duration
	HTTPClient    *http.Client
}

// Result contains provider-neutral document evidence and request accounting.
type Result struct {
	Document       document.SourceDocument
	ReturnedModel  string
	UnitsProcessed int
	ProviderBytes  *int64
	Metrics        RequestMetrics
}

// RequestMetrics describes actual provider HTTP work for one logical request.
type RequestMetrics struct {
	Requests int
	Retries  int
	Latency  time.Duration
}

type processError struct {
	err     error
	metrics RequestMetrics
}

func (e *processError) Error() string { return e.err.Error() }
func (e *processError) Unwrap() error { return e.err }

// MetricsFromError recovers provider request accounting from a Process error.
func MetricsFromError(err error) RequestMetrics {
	if processErr, ok := errors.AsType[*processError](err); ok {
		return processErr.metrics
	}
	return RequestMetrics{}
}

type wireResult struct {
	Model     string     `json:"model"`
	Pages     []wirePage `json:"pages"`
	UsageInfo *wireUsage `json:"usage_info"`
}

type wirePage struct {
	Index        int            `json:"index"`
	Markdown     string         `json:"markdown"`
	Header       string         `json:"header"`
	Footer       string         `json:"footer"`
	Dimensions   wireDimensions `json:"dimensions"`
	indexPresent bool
}

type wireDimensions struct {
	DPI    int `json:"dpi"`
	Height int `json:"height"`
	Width  int `json:"width"`
}

type wireUsage struct {
	PagesProcessed        int    `json:"pages_processed"`
	DocSizeBytes          *int64 `json:"doc_size_bytes"`
	pagesProcessedPresent bool
}

// Client calls the single endpoint derived from its policy.
type Client struct {
	policy        Policy
	apiKey        string
	maxRetries    int
	maxRetryDelay time.Duration
	http          *http.Client
}

// NewClient validates operational bounds without making a network request.
func NewClient(policy Policy, config ClientConfig) (*Client, error) {
	if policy.digest == "" {
		return nil, errors.New("mistral policy is invalid; use NewPolicy")
	}
	if config.APIKey == "" || config.APIKey != strings.TrimSpace(config.APIKey) {
		return nil, errors.New("mistral OCR API key is required and must not contain surrounding whitespace")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Timeout < 0 || config.Timeout > MaxTimeout {
		return nil, errors.New("mistral OCR timeout is outside package bounds")
	}
	if config.MaxRetries < 0 || config.MaxRetries > MaxRetries {
		return nil, errors.New("mistral OCR max retries is outside package bounds")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = DefaultMaxRetries
	}
	if config.MaxRetryDelay == 0 {
		config.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if config.MaxRetryDelay < 0 || config.MaxRetryDelay > MaxRetryDelay {
		return nil, errors.New("mistral OCR retry delay is outside package bounds")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: config.Timeout}
	} else {
		clone := *httpClient
		httpClient = &clone
		if httpClient.Timeout <= 0 || httpClient.Timeout > config.Timeout {
			httpClient.Timeout = config.Timeout
		}
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		policy: policy, apiKey: config.APIKey, maxRetries: config.MaxRetries,
		maxRetryDelay: config.MaxRetryDelay, http: httpClient,
	}, nil
}

// Process verifies an opaque staged document and its capability authorization
// before sending bytes. Each request attempt holds one immutable in-memory copy
// up to PolicyValues.MaxDocumentBytes, so applications must bound concurrent
// calls according to their memory budget.
func (c *Client) Process(
	ctx context.Context,
	prepared *PreparedDocument,
	authorization FormatAuthorization,
) (Result, error) {
	if c == nil {
		return Result{}, errors.New("mistral OCR client is nil")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if authorization.policyDigest == "" || authorization.policyDigest != c.policy.digest {
		return Result{}, errors.New("mistral OCR authorization belongs to a different policy")
	}
	if authorization.format.ID == "" {
		return Result{}, errors.New("mistral OCR authorization has no format")
	}
	options := probeRequestOptions(
		authorization.format, c.policy.values.MaxUnits,
		c.policy.values.ExtractHeader, c.policy.values.ExtractFooter,
	)
	snapshotForAttempt := func() (preparedSnapshot, error) {
		snapshot, err := prepared.snapshot()
		if err != nil {
			return preparedSnapshot{}, err
		}
		if err := c.validatePreparedSnapshot(snapshot, authorization); err != nil {
			return preparedSnapshot{}, err
		}
		return snapshot, nil
	}
	return c.process(
		ctx, snapshotForAttempt, options, authorization.method, c.policy.values.MaxUnits,
	)
}

func (c *Client) process(
	ctx context.Context,
	snapshotForAttempt func() (preparedSnapshot, error),
	options requestOptions,
	method UnitBoundMethod,
	maxUnits int,
) (Result, error) {
	requests := 0
	var providerLatency time.Duration
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{}, newProcessError(err, requests, providerLatency)
		}
		snapshot, err := snapshotForAttempt()
		if err != nil {
			return Result{}, newProcessError(err, requests, providerLatency)
		}
		prefix, suffix, encodedLength, err := requestEnvelope(
			c.policy.values.Model, snapshot.mediaType, snapshot.size, options,
		)
		if err != nil {
			return Result{}, newProcessError(err, requests, providerLatency)
		}
		result, retryHeader, requested, latency, processErr := c.processOnce(
			ctx, snapshot, prefix, suffix, encodedLength, method, maxUnits,
		)
		if requested {
			requests++
			providerLatency += latency
		}
		if processErr == nil {
			result.Metrics = requestMetrics(requests, providerLatency)
			return result, nil
		}
		var transient *transientError
		if !errors.As(processErr, &transient) {
			return Result{}, newProcessError(processErr, requests, providerLatency)
		}
		if attempt == c.maxRetries {
			exhausted := fmt.Errorf("%w after %d attempts: %w", ErrTransientResponse, attempt+1, transient)
			return Result{}, newProcessError(exhausted, requests, providerLatency)
		}
		if err := waitContext(ctx, retryAfter(retryHeader, attempt, c.maxRetryDelay)); err != nil {
			return Result{}, newProcessError(err, requests, providerLatency)
		}
	}
	return Result{}, ErrTransientResponse
}

func (c *Client) validatePreparedSnapshot(
	snapshot preparedSnapshot,
	authorization FormatAuthorization,
) error {
	if snapshot.format.ID != authorization.format.ID {
		return errors.New("mistral OCR detected format does not match authorization")
	}
	if snapshot.size > c.policy.values.MaxDocumentBytes {
		return fmt.Errorf("mistral OCR document is %d bytes, policy limit %d",
			snapshot.size, c.policy.values.MaxDocumentBytes)
	}
	if authorization.method == UnitBoundLocalExact &&
		(snapshot.localUnits <= 0 || snapshot.localUnits > c.policy.values.MaxUnits) {
		return fmt.Errorf("mistral OCR local unit count exceeds authorized limit: %w", ErrCapabilityContract)
	}
	return nil
}

func (c *Client) processOnce(
	ctx context.Context,
	snapshot preparedSnapshot,
	prefix, suffix []byte,
	encodedLength int64,
	method UnitBoundMethod,
	maxUnits int,
) (Result, string, bool, time.Duration, error) {
	documentBytes, err := readVerifiedDocument(ctx, snapshot)
	if err != nil {
		return Result{}, "", false, 0, err
	}

	reader, streamDone := streamRequest(documentBytes, prefix, suffix)
	var streamErr error
	streamFinished := false
	waitForStream := func() error {
		if !streamFinished {
			streamErr = <-streamDone
			streamFinished = true
		}
		return streamErr
	}
	defer func() {
		_ = reader.Close()
		_ = waitForStream()
		clear(documentBytes)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.policy.values.Endpoint, reader)
	if err != nil {
		return Result{}, "", false, 0, fmt.Errorf("build Mistral OCR request: %w", err)
	}
	request.ContentLength = encodedLength
	request.Header.Set("Content-Type", mediaTypeJSON)
	request.Header.Set("Authorization", "Bearer "+c.apiKey)

	started := time.Now()
	response, err := c.http.Do(request)
	latency := time.Since(started)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, "", true, latency, ctxErr
		}
		return Result{}, "", true, latency, &transientError{cause: fmt.Errorf("send Mistral OCR request: %w", err)}
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		return Result{}, response.Header.Get("Retry-After"), true, latency, &transientError{status: response.StatusCode}
	}
	if response.StatusCode >= http.StatusBadRequest {
		return Result{}, "", true, latency, fmt.Errorf("mistral OCR HTTP %d: %w", response.StatusCode, ErrPermanentResponse)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Result{}, "", true, latency, fmt.Errorf("mistral OCR unexpected HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != mediaTypeJSON {
		return Result{}, "", true, latency, errors.New("mistral OCR returned non-JSON content type")
	}
	type bodyResult struct {
		body []byte
		err  error
	}
	// A provider can send response headers before the transport finishes the
	// request body. Read both sides concurrently so neither blocks the other.
	bodyDone := make(chan bodyResult, 1)
	go func() {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, c.policy.values.MaxResponseBytes+1))
		bodyDone <- bodyResult{body: body, err: readErr}
	}()
	streamResults := streamDone
	bodyResults := bodyDone
	var body []byte
	bodyFinished := false
	for !streamFinished || !bodyFinished {
		select {
		case streamErr = <-streamResults:
			streamFinished = true
			streamResults = nil
			if streamErr != nil {
				latency = time.Since(started)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return Result{}, "", true, latency, ctxErr
				}
				return Result{}, "", true, latency, &transientError{
					cause: fmt.Errorf("stream Mistral OCR request: %w", streamErr),
				}
			}
		case result := <-bodyResults:
			bodyFinished = true
			bodyResults = nil
			body = result.body
			if result.err != nil {
				latency = time.Since(started)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return Result{}, "", true, latency, ctxErr
				}
				return Result{}, "", true, latency, &transientError{
					cause: fmt.Errorf("read Mistral OCR response: %w", result.err),
				}
			}
			if int64(len(body)) > c.policy.values.MaxResponseBytes {
				latency = time.Since(started)
				return Result{}, "", true, latency, ErrResponseTooLarge
			}
		case <-ctx.Done():
			ctxErr := ctx.Err()
			_ = reader.CloseWithError(ctxErr)
			_ = response.Body.Close()
			latency = time.Since(started)
			return Result{}, "", true, latency, ctxErr
		}
	}
	latency = time.Since(started)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, "", true, latency, ctxErr
	}
	if !utf8.Valid(body) {
		return Result{}, "", true, latency, errors.New("mistral OCR response contains invalid UTF-8")
	}
	wire, err := decodeWireResult(body, method, maxUnits)
	if err != nil {
		return Result{}, "", true, latency, fmt.Errorf("decode Mistral OCR response: %w", err)
	}
	if err := validateWireResult(wire, c.policy.values.Model, snapshot, method, maxUnits); err != nil {
		return Result{}, "", true, latency, err
	}
	return providerNeutralResult(wire, snapshot.format), "", true, latency, nil
}

func newProcessError(err error, requests int, providerLatency time.Duration) error {
	if requests == 0 {
		return err
	}
	return &processError{err: err, metrics: requestMetrics(requests, providerLatency)}
}

func requestMetrics(requests int, providerLatency time.Duration) RequestMetrics {
	return RequestMetrics{Requests: requests, Retries: max(requests-1, 0), Latency: providerLatency}
}

type transientError struct {
	status int
	cause  error
}

func (e *transientError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("Mistral OCR transient HTTP %d", e.status)
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func streamRequest(documentBytes, prefix, suffix []byte) (*io.PipeReader, <-chan error) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var streamErr error
		defer func() {
			_ = writer.CloseWithError(streamErr)
			done <- streamErr
			close(done)
		}()
		if _, streamErr = writer.Write(prefix); streamErr != nil {
			return
		}
		encoder := base64.NewEncoder(base64.StdEncoding, writer)
		_, streamErr = io.Copy(encoder, bytes.NewReader(documentBytes))
		if closeErr := encoder.Close(); streamErr == nil {
			streamErr = closeErr
		}
		if streamErr == nil {
			_, streamErr = writer.Write(suffix)
		}
	}()
	return reader, done
}

func decodeWireResult(data []byte, method UnitBoundMethod, maxUnits int) (wireResult, error) {
	decoder := jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowDuplicateNames(true))
	opening, err := decoder.ReadToken()
	if err != nil {
		return wireResult{}, err
	}
	if opening.Kind() != jsontext.KindBeginObject {
		return wireResult{}, errors.New("mistral OCR response must be a JSON object")
	}

	var result wireResult
	keys := make(map[string]struct{})
	for decoder.PeekKind() != jsontext.KindEndObject {
		keyToken, err := decoder.ReadToken()
		if err != nil {
			return wireResult{}, err
		}
		if keyToken.Kind() != jsontext.KindString {
			return wireResult{}, errors.New("mistral OCR response has a non-string JSON object key")
		}
		key := keyToken.String()
		if !canonicalJSONKey(key) {
			return wireResult{}, fmt.Errorf(
				"mistral OCR response JSON object key %q must use lowercase ASCII", key,
			)
		}
		if _, exists := keys[key]; exists {
			return wireResult{}, fmt.Errorf("mistral OCR response has duplicate JSON object key %q", key)
		}
		keys[key] = struct{}{}
		switch key {
		case "model":
			err = json.UnmarshalDecode(decoder, &result.Model)
		case "pages":
			result.Pages, err = decodeWirePages(decoder, method, maxUnits)
		case "usage_info":
			err = json.UnmarshalDecode(decoder, &result.UsageInfo)
		default:
			err = scanJSONValue(decoder, 1, "mistral OCR response")
		}
		if err != nil {
			return wireResult{}, err
		}
	}
	if _, err := decoder.ReadToken(); err != nil {
		return wireResult{}, err
	}
	if _, err := decoder.ReadToken(); !errors.Is(err, io.EOF) {
		return wireResult{}, errors.New("mistral OCR response has trailing JSON")
	}
	return result, nil
}

func decodeWirePages(
	decoder *jsontext.Decoder,
	method UnitBoundMethod,
	maxUnits int,
) ([]wirePage, error) {
	opening, err := decoder.ReadToken()
	if err != nil {
		return nil, err
	}
	if opening.Kind() == jsontext.KindNull {
		return nil, nil
	}
	if opening.Kind() != jsontext.KindBeginArray {
		return nil, errors.New("mistral OCR response pages must be an array")
	}
	pages := make([]wirePage, 0, min(maxUnits, 16))
	for decoder.PeekKind() != jsontext.KindEndArray {
		if len(pages) == maxUnits {
			if method == UnitBoundProviderRequest || method == UnitBoundLocalExact {
				return nil, fmt.Errorf("provider returned more than %d authorized units: %w",
					maxUnits, ErrCapabilityContract)
			}
			return nil, fmt.Errorf("mistral OCR response exceeds unit limit %d", maxUnits)
		}
		var page wirePage
		if err := json.UnmarshalDecode(decoder, &page); err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	return pages, nil
}

func readVerifiedDocument(ctx context.Context, snapshot preparedSnapshot) ([]byte, error) {
	if snapshot.path == "" || snapshot.size < 0 || snapshot.format.ID == "" ||
		snapshot.mediaType != snapshot.format.MediaType || len(snapshot.sha256) != sha256.Size*2 ||
		strings.ToLower(snapshot.sha256) != snapshot.sha256 {
		return nil, errors.New("invalid Mistral OCR prepared document metadata")
	}
	if _, err := hex.DecodeString(snapshot.sha256); err != nil {
		return nil, errors.New("invalid Mistral OCR prepared document SHA-256")
	}
	file, err := openPrivateFile(snapshot.path)
	if err != nil {
		return nil, fmt.Errorf("open Mistral OCR spool: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened Mistral OCR spool: %w", err)
	}
	if openedInfo.Size() != snapshot.size {
		return nil, errors.New("mistral OCR spool size changed")
	}
	contents := make([]byte, int(snapshot.size))
	contextFile := &contextReader{ctx: ctx, reader: file}
	if _, err := io.ReadFull(contextFile, contents); err != nil {
		return nil, fmt.Errorf("verify Mistral OCR spool: %w", err)
	}
	var extra [1]byte
	extraBytes, extraErr := io.ReadFull(contextFile, extra[:])
	if extraErr != nil && !errors.Is(extraErr, io.EOF) {
		return nil, fmt.Errorf("verify Mistral OCR spool length: %w", extraErr)
	}
	if extraBytes != 0 {
		return nil, errors.New("mistral OCR spool size changed")
	}
	digest := sha256.Sum256(contents)
	if hex.EncodeToString(digest[:]) != snapshot.sha256 {
		return nil, errors.New("mistral OCR spool hash mismatch")
	}
	return contents, nil
}

func requestEnvelope(
	model, mediaType string,
	size int64,
	options requestOptions,
) ([]byte, []byte, int64, error) {
	const maxBase64Input = (math.MaxInt64 / 4 * 3) - 2
	if size < 0 || size > maxBase64Input {
		return nil, nil, 0, errors.New("mistral OCR document size is not representable")
	}
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encode Mistral OCR model: %w", err)
	}
	dataPrefixJSON, err := json.Marshal("data:" + mediaType + ";base64,")
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encode Mistral OCR media type: %w", err)
	}
	prefix := append([]byte(`{"model":`), modelJSON...)
	prefix = append(prefix, []byte(`,"document":{"type":"document_url","document_url":`)...)
	prefix = append(prefix, dataPrefixJSON[:len(dataPrefixJSON)-1]...)
	tail := struct {
		IncludeImageBase64 bool   `json:"include_image_base64"`
		IncludeBlocks      bool   `json:"include_blocks"`
		ExtractHeader      bool   `json:"extract_header"`
		ExtractFooter      bool   `json:"extract_footer"`
		Pages              string `json:"pages,omitzero"`
	}{
		ExtractHeader: options.ExtractHeader,
		ExtractFooter: options.ExtractFooter,
		Pages:         options.Pages,
	}
	tailJSON, err := json.Marshal(tail)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encode Mistral OCR options: %w", err)
	}
	suffix := append([]byte(`"},`), tailJSON[1:]...)
	base64Length := ((size + 2) / 3) * 4
	if base64Length > math.MaxInt64-int64(len(prefix))-int64(len(suffix)) {
		return nil, nil, 0, errors.New("mistral OCR request length overflow")
	}
	return prefix, suffix, int64(len(prefix)) + base64Length + int64(len(suffix)), nil
}

func validateWireResult(
	result wireResult,
	expectedModel string,
	snapshot preparedSnapshot,
	method UnitBoundMethod,
	maxUnits int,
) error {
	if result.Model == "" {
		return errors.New("mistral OCR response omitted model")
	}
	if result.Model != expectedModel {
		return fmt.Errorf("mistral OCR response model %q does not match requested model", result.Model)
	}
	if result.Pages == nil {
		return errors.New("mistral OCR response omitted pages")
	}
	if len(result.Pages) == 0 {
		return errors.New("mistral OCR response returned no pages")
	}
	if len(result.Pages) > maxUnits {
		if method == UnitBoundProviderRequest || method == UnitBoundLocalExact {
			return fmt.Errorf("provider returned %d units above authorized limit %d: %w",
				len(result.Pages), maxUnits, ErrCapabilityContract)
		}
		return fmt.Errorf("mistral OCR response has %d units, limit %d", len(result.Pages), maxUnits)
	}
	for index, page := range result.Pages {
		if !page.indexPresent || page.Index != index {
			return fmt.Errorf("mistral OCR response unit %d has invalid index %d", index, page.Index)
		}
		if page.Dimensions.DPI < 0 || page.Dimensions.DPI > maxSourceUnitDPI ||
			page.Dimensions.Height < 0 || page.Dimensions.Height > maxSourceUnitHeight ||
			page.Dimensions.Width < 0 || page.Dimensions.Width > maxSourceUnitWidth {
			return fmt.Errorf("mistral OCR response unit %d has invalid dimensions", index)
		}
	}
	if result.UsageInfo == nil || !result.UsageInfo.pagesProcessedPresent {
		return errors.New("mistral OCR response omitted usage")
	}
	if result.UsageInfo.PagesProcessed < 0 ||
		(result.UsageInfo.DocSizeBytes != nil && *result.UsageInfo.DocSizeBytes < 0) {
		return errors.New("mistral OCR response has invalid usage")
	}
	if result.UsageInfo.PagesProcessed > maxUnits &&
		(method == UnitBoundProviderRequest || method == UnitBoundLocalExact) {
		return fmt.Errorf("provider processed %d units above authorized limit %d: %w",
			result.UsageInfo.PagesProcessed, maxUnits, ErrCapabilityContract)
	}
	if method == UnitBoundLocalExact && result.UsageInfo.PagesProcessed != snapshot.localUnits {
		return fmt.Errorf("provider processed %d units, local exact count %d: %w",
			result.UsageInfo.PagesProcessed, snapshot.localUnits, ErrCapabilityContract)
	}
	if result.UsageInfo.PagesProcessed != len(result.Pages) {
		return fmt.Errorf("mistral OCR response processed %d units but returned %d",
			result.UsageInfo.PagesProcessed, len(result.Pages))
	}
	if result.UsageInfo.DocSizeBytes != nil && *result.UsageInfo.DocSizeBytes != snapshot.size {
		return fmt.Errorf("mistral OCR response accounted for %d document bytes, expected %d",
			*result.UsageInfo.DocSizeBytes, snapshot.size)
	}
	return nil
}

func providerNeutralResult(result wireResult, format CandidateFormat) Result {
	units := make([]document.SourceUnit, len(result.Pages))
	for index, page := range result.Pages {
		units[index] = document.SourceUnit{
			Index: page.Index, Markdown: page.Markdown, Header: page.Header, Footer: page.Footer,
			Dimensions: document.UnitDimensions{
				DPI: page.Dimensions.DPI, Height: page.Dimensions.Height, Width: page.Dimensions.Width,
			},
		}
	}
	return Result{
		Document:      document.SourceDocument{Family: format.Family, UnitKind: format.UnitKind, Units: units},
		ReturnedModel: result.Model, UnitsProcessed: result.UsageInfo.PagesProcessed,
		ProviderBytes: result.UsageInfo.DocSizeBytes,
	}
}

func (p *wirePage) UnmarshalJSON(data []byte) error {
	*p = wirePage{}
	if err := rejectDuplicateJSONKeys(data, "mistral OCR response"); err != nil {
		return err
	}
	type pageJSON struct {
		Index      *int           `json:"index"`
		Markdown   string         `json:"markdown"`
		Header     string         `json:"header"`
		Footer     string         `json:"footer"`
		Dimensions wireDimensions `json:"dimensions"`
	}
	var decoded pageJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	p.Markdown = decoded.Markdown
	p.Header = decoded.Header
	p.Footer = decoded.Footer
	p.Dimensions = decoded.Dimensions
	if decoded.Index != nil {
		p.Index = *decoded.Index
		p.indexPresent = true
	}
	return nil
}

func (u *wireUsage) UnmarshalJSON(data []byte) error {
	*u = wireUsage{}
	if err := rejectDuplicateJSONKeys(data, "mistral OCR response"); err != nil {
		return err
	}
	type usageJSON struct {
		PagesProcessed *int   `json:"pages_processed"`
		DocSizeBytes   *int64 `json:"doc_size_bytes"`
	}
	var decoded usageJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	u.DocSizeBytes = decoded.DocSizeBytes
	if decoded.PagesProcessed != nil {
		u.PagesProcessed = *decoded.PagesProcessed
		u.pagesProcessedPresent = true
	}
	return nil
}
