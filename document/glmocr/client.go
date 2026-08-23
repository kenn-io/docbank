package glmocr

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
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/ocr"
)

const (
	DefaultTimeout       = 10 * time.Minute
	DefaultMaxRetries    = 2
	DefaultMaxRetryDelay = 15 * time.Second
	MaxTimeout           = 30 * time.Minute
	MaxRetries           = 10
	MaxRetryDelay        = time.Minute
)

var acceptedMediaTypes = map[string]string{
	"application/pdf": "pdf",
	"image/jpeg":      "image",
	"image/png":       "image",
	"image/webp":      "image",
}

// SupportedMediaTypes returns the stable local input allowlist.
func SupportedMediaTypes() []string {
	return []string{"application/pdf", "image/jpeg", "image/png", "image/webp"}
}

// ClientConfig contains bounded transport settings outside policy identity.
type ClientConfig struct {
	Timeout       time.Duration
	MaxRetries    int
	MaxRetryDelay time.Duration
	HTTPClient    *http.Client
}

// Client calls the local GLM-OCR document service.
type Client struct {
	policy        Policy
	http          *http.Client
	maxRetries    int
	maxRetryDelay time.Duration
}

var _ ocr.Processor = (*Client)(nil)

// NewClient validates transport settings without making a request.
func NewClient(policy Policy, config ClientConfig) (*Client, error) {
	if policy.fingerprint == "" {
		return nil, errors.New("GLM-OCR policy is invalid; use NewPolicy")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Timeout < 0 || config.Timeout > MaxTimeout {
		return nil, errors.New("GLM-OCR timeout is outside package bounds")
	}
	if config.MaxRetries < 0 || config.MaxRetries > MaxRetries {
		return nil, errors.New("GLM-OCR max retries is outside package bounds")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = DefaultMaxRetries
	}
	if config.MaxRetryDelay == 0 {
		config.MaxRetryDelay = DefaultMaxRetryDelay
	}
	if config.MaxRetryDelay < 0 || config.MaxRetryDelay > MaxRetryDelay {
		return nil, errors.New("GLM-OCR retry delay is outside package bounds")
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
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{policy: policy, http: httpClient, maxRetries: config.MaxRetries, maxRetryDelay: config.MaxRetryDelay}, nil
}

// Identity returns the pinned local provider/model identity.
func (c *Client) Identity() ocr.Identity {
	if c == nil {
		return ocr.Identity{}
	}
	return c.policy.identity
}

// Process verifies a bounded source, calls the structured document endpoint,
// and normalizes ordered page evidence entirely in memory.
func (c *Client) Process(ctx context.Context, source ocr.Source) (ocr.Result, error) {
	if c == nil {
		if source.Content != nil {
			_ = source.Content.Close()
		}
		return ocr.Result{}, errors.New("GLM-OCR client is nil")
	}
	content, err := c.readSource(ctx, source)
	if err != nil {
		return ocr.Result{}, err
	}
	payload, err := json.Marshal(struct {
		Images []string `json:"images"`
	}{Images: []string{"data:" + source.MediaType + ";base64," + base64.StdEncoding.EncodeToString(content)}})
	if err != nil {
		return ocr.Result{}, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: fmt.Errorf("encode GLM-OCR request: %w", err)}
	}

	requests := 0
	var latency time.Duration
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		wire, retryAfter, requested, requestLatency, err := c.processOnce(ctx, payload)
		if requested {
			requests++
			latency += requestLatency
		}
		metrics := ocr.RequestMetrics{Requests: requests, Retries: max(requests-1, 0), Latency: latency}
		if err == nil {
			result, convertErr := c.convert(wire, source, metrics)
			if convertErr != nil {
				return ocr.Result{}, &ocr.ProviderError{Kind: ocr.ErrorMalformedOutput, Metrics: metrics, Cause: convertErr}
			}
			return result, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ocr.Result{}, err
		}
		if _, ok := errors.AsType[*retryableError](err); !ok {
			return ocr.Result{}, &ocr.ProviderError{Kind: classifyHTTPError(err), Metrics: metrics, Cause: err}
		}
		if attempt == c.maxRetries {
			return ocr.Result{}, &ocr.ProviderError{
				Kind: ocr.ErrorTransient, Metrics: metrics,
				Cause: fmt.Errorf("GLM-OCR transient failure after %d attempts: %w", attempt+1, err),
			}
		}
		if err := waitContext(ctx, retryDelay(retryAfter, attempt, c.maxRetryDelay)); err != nil {
			return ocr.Result{}, err
		}
	}
	panic("unreachable")
}

func (c *Client) readSource(ctx context.Context, source ocr.Source) (_ []byte, err error) {
	if source.Content == nil {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: errors.New("OCR source content is required")}
	}
	contentOwner := &onceReadCloser{ReadCloser: source.Content}
	defer func() {
		if closeErr := contentOwner.Close(); err == nil && closeErr != nil {
			err = &ocr.ProviderError{Kind: ocr.ErrorTransient, Cause: fmt.Errorf("close GLM-OCR source: %w", closeErr)}
		}
	}()
	if err := source.Validate(); err != nil {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: err}
	}
	if source.Size > c.policy.values.MaxDocumentBytes {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: errors.New("GLM-OCR source exceeds policy byte limit")}
	}
	if _, ok := acceptedMediaTypes[source.MediaType]; !ok {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: fmt.Errorf("GLM-OCR media type %q is unsupported", source.MediaType)}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = contentOwner.Close() })
	defer stopCancellation()
	content, err := io.ReadAll(io.LimitReader(contentOwner, source.Size+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &ocr.ProviderError{Kind: ocr.ErrorTransient, Cause: fmt.Errorf("read GLM-OCR source: %w", err)}
	}
	if int64(len(content)) != source.Size {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: errors.New("GLM-OCR source size mismatch")}
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != source.SHA256 {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: errors.New("GLM-OCR source hash mismatch")}
	}
	if detected := detectMediaType(content); detected != source.MediaType {
		return nil, &ocr.ProviderError{Kind: ocr.ErrorInvalidInput, Cause: fmt.Errorf("GLM-OCR source bytes are %q, not %q", detected, source.MediaType)}
	}
	return content, nil
}

func detectMediaType(content []byte) string {
	if len(content) >= 12 && string(content[:4]) == "RIFF" && string(content[8:12]) == "WEBP" {
		return "image/webp"
	}
	return http.DetectContentType(content)
}

type onceReadCloser struct {
	io.ReadCloser

	once sync.Once
	err  error
}

func (c *onceReadCloser) Close() error {
	c.once.Do(func() { c.err = c.ReadCloser.Close() })
	return c.err
}

type wireResult struct {
	JSONResult            jsontext.Value `json:"json_result"`
	Model                 string         `json:"model"`
	DeploymentFingerprint string         `json:"deployment_fingerprint"`
}

type wireElement struct {
	Index   *int   `json:"index"`
	Label   string `json:"label"`
	Content string `json:"content"`
	Bounds  []int  `json:"bbox_2d"`
}

type retryableError struct {
	status int
	cause  error
}

func (e *retryableError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("GLM-OCR transient HTTP %d", e.status)
}

func (e *retryableError) Unwrap() error { return e.cause }

var (
	errRejected         = errors.New("GLM-OCR request rejected")
	errResponseTooLarge = errors.New("GLM-OCR response too large")
)

func (c *Client) processOnce(ctx context.Context, payload []byte) (wireResult, string, bool, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.policy.values.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return wireResult{}, "", false, 0, fmt.Errorf("create GLM-OCR request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	started := time.Now()
	response, err := c.http.Do(request)
	latency := time.Since(started)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return wireResult{}, "", true, latency, ctxErr
		}
		return wireResult{}, "", true, latency, &retryableError{cause: fmt.Errorf("send GLM-OCR request: %w", err)}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError {
		return wireResult{}, response.Header.Get("Retry-After"), true, latency, &retryableError{status: response.StatusCode}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return wireResult{}, "", true, latency, fmt.Errorf("GLM-OCR HTTP %d: %w", response.StatusCode, errRejected)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return wireResult{}, "", true, latency, errors.New("GLM-OCR returned non-JSON content type")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.policy.values.MaxResponseBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return wireResult{}, "", true, latency, ctxErr
		}
		return wireResult{}, "", true, latency, &retryableError{cause: fmt.Errorf("read GLM-OCR response: %w", err)}
	}
	if int64(len(body)) > c.policy.values.MaxResponseBytes {
		return wireResult{}, "", true, latency, errResponseTooLarge
	}
	if !utf8.Valid(body) {
		return wireResult{}, "", true, latency, errors.New("GLM-OCR response contains invalid UTF-8")
	}
	var result wireResult
	if err := json.Unmarshal(body, &result); err != nil {
		return wireResult{}, "", true, latency, fmt.Errorf("decode GLM-OCR response: %w", err)
	}
	return result, "", true, latency, nil
}

func classifyHTTPError(err error) ocr.ErrorKind {
	switch {
	case errors.Is(err, errRejected):
		return ocr.ErrorRejected
	case errors.Is(err, errResponseTooLarge):
		return ocr.ErrorResponseTooLarge
	default:
		return ocr.ErrorMalformedOutput
	}
}

func (c *Client) convert(wire wireResult, source ocr.Source, metrics ocr.RequestMetrics) (ocr.Result, error) {
	if wire.DeploymentFingerprint != c.policy.deployment {
		return ocr.Result{}, fmt.Errorf(
			"GLM-OCR returned deployment fingerprint %q, want %q",
			wire.DeploymentFingerprint, c.policy.deployment,
		)
	}
	if wire.Model != c.policy.values.ServedModel {
		return ocr.Result{}, fmt.Errorf("GLM-OCR returned model %q, want %q", wire.Model, c.policy.values.ServedModel)
	}
	if len(wire.JSONResult) == 0 || bytes.Equal(wire.JSONResult, []byte("null")) {
		return ocr.Result{}, errors.New("GLM-OCR response has no structured pages")
	}
	var rawPages []jsontext.Value
	if err := json.Unmarshal(wire.JSONResult, &rawPages); err != nil {
		return ocr.Result{}, fmt.Errorf("decode GLM-OCR page structure: %w", err)
	}
	if len(rawPages) == 0 || len(rawPages) > c.policy.values.MaxUnits {
		return ocr.Result{}, errors.New("GLM-OCR response page count is outside policy bounds")
	}
	pages := make([][]wireElement, len(rawPages))
	for pageIndex, rawPage := range rawPages {
		if len(rawPage) == 0 || rawPage.Kind() != '[' {
			return ocr.Result{}, fmt.Errorf("GLM-OCR page %d is not an array", pageIndex)
		}
		if err := json.Unmarshal(rawPage, &pages[pageIndex]); err != nil {
			return ocr.Result{}, fmt.Errorf("decode GLM-OCR page %d: %w", pageIndex, err)
		}
	}
	family := acceptedMediaTypes[source.MediaType]
	sourceDocument := document.SourceDocument{Family: family, UnitKind: "page", Units: make([]document.SourceUnit, len(pages))}
	structure := make([]ocr.UnitStructure, len(pages))
	hasEvidence := false
	for pageIndex, page := range pages {
		markdown := make([]string, 0, len(page))
		elements := make([]ocr.Element, 0, len(page))
		for elementIndex, element := range page {
			if element.Index == nil || *element.Index != elementIndex || element.Label == "" || len(element.Label) > 64 ||
				element.Label != strings.ToLower(element.Label) || strings.ContainsAny(element.Label, "\r\n\x00") ||
				!utf8.ValidString(element.Content) {
				return ocr.Result{}, fmt.Errorf("GLM-OCR page %d element %d is invalid", pageIndex, elementIndex)
			}
			bounds, hasBounds, err := convertBounds(element.Bounds)
			if err != nil {
				return ocr.Result{}, fmt.Errorf("GLM-OCR page %d element %d: %w", pageIndex, elementIndex, err)
			}
			var boundsPointer *ocr.ElementBounds
			if hasBounds {
				boundsPointer = &bounds
			}
			if element.Content != "" {
				markdown = append(markdown, element.Content)
			}
			elements = append(elements, ocr.Element{
				Index: *element.Index, Kind: element.Label, Markdown: element.Content, Bounds: boundsPointer,
			})
		}
		pageMarkdown := strings.Join(markdown, "\n\n")
		hasEvidence = hasEvidence || strings.TrimSpace(pageMarkdown) != ""
		sourceDocument.Units[pageIndex] = document.SourceUnit{Index: pageIndex, Markdown: pageMarkdown}
		structure[pageIndex] = ocr.UnitStructure{UnitIndex: pageIndex, Elements: elements}
	}
	if !hasEvidence {
		return ocr.Result{}, errors.New("GLM-OCR document has no textual evidence")
	}
	normalized, err := document.NormalizeDocument(sourceDocument, c.policy.normalizePolicy)
	if err != nil {
		return ocr.Result{}, fmt.Errorf("normalize GLM-OCR document: %w", err)
	}
	providerBytes := source.Size
	return ocr.Result{
		Source: sourceDocument, Document: normalized, Structure: structure,
		Identity: c.policy.identity, PolicyFingerprint: c.policy.fingerprint,
		UnitsProcessed: len(pages), ProviderBytes: &providerBytes, Metrics: metrics,
	}, nil
}

func convertBounds(values []int) (ocr.ElementBounds, bool, error) {
	if values == nil {
		return ocr.ElementBounds{}, false, nil
	}
	if len(values) != 4 {
		return ocr.ElementBounds{}, false, errors.New("element bounds must contain four coordinates")
	}
	for _, value := range values {
		if value < 0 || value > 1000 {
			return ocr.ElementBounds{}, false, errors.New("element bounds are outside normalized coordinates")
		}
	}
	if values[0] > values[2] || values[1] > values[3] {
		return ocr.ElementBounds{}, false, errors.New("element bounds are inverted")
	}
	return ocr.ElementBounds{Left: values[0], Top: values[1], Right: values[2], Bottom: values[3]}, true, nil
}

func retryDelay(header string, attempt int, maximum time.Duration) time.Duration {
	if seconds, err := time.ParseDuration(strings.TrimSpace(header) + "s"); err == nil && seconds >= 0 {
		return min(seconds, maximum)
	}
	if when, err := http.ParseTime(header); err == nil {
		return min(max(time.Until(when), 0), maximum)
	}
	delay := 250 * time.Millisecond * time.Duration(1<<min(attempt, 8))
	return min(delay, maximum)
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

// Health verifies that the configured loopback service is accepting requests.
func (c *Client) Health(ctx context.Context) error {
	if c == nil {
		return errors.New("GLM-OCR client is nil")
	}
	endpoint, err := url.Parse(c.policy.values.Endpoint)
	if err != nil {
		return fmt.Errorf("parse GLM-OCR health endpoint: %w", err)
	}
	endpoint.Path = "/health"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("create GLM-OCR health request: %w", err)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call GLM-OCR health endpoint: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GLM-OCR health HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return errors.New("GLM-OCR health response is invalid")
	}
	var health struct {
		Status                string `json:"status"`
		DeploymentFingerprint string `json:"deployment_fingerprint"`
	}
	if err := json.Unmarshal(body, &health); err != nil || health.Status != "ok" {
		return errors.New("GLM-OCR health response is not ok")
	}
	if health.DeploymentFingerprint != c.policy.deployment {
		return errors.New("GLM-OCR health deployment fingerprint does not match policy")
	}
	return nil
}
