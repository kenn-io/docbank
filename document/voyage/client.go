package voyage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document/internal/manifestjson"
	"go.kenn.io/docbank/document/media"
)

const (
	// DefaultTimeout bounds one provider request attempt.
	DefaultTimeout = 45 * time.Second
	// MaxTimeout is the largest per-attempt timeout accepted by NewClient.
	MaxTimeout = 5 * time.Minute
	// DefaultMaxRetries is the default attempt count for retryable failures.
	DefaultMaxRetries = 3
	// MaxRetries is the largest attempt count accepted by NewClient.
	MaxRetries = 10

	defaultRetryBaseDelay = 100 * time.Millisecond
	maxBackoffShift       = 8
	maxErrorBodyBytes     = 4_096
	embeddingsPath        = "/multimodalembeddings"
)

// Media is one media part: detected metadata and the bytes it describes.
type Media struct {
	Metadata media.Metadata
	Bytes    []byte
}

// Part is one ordered element of an input: text or media, never both.
type Part struct {
	Text  string
	Media *Media
}

// Input is one document or query: ordered parts with at most one media part.
type Input struct {
	Parts []Part
}

// Usage reports provider token accounting when the provider returned it.
type Usage struct {
	TotalTokens int64
	Available   bool
}

// Result contains one vector per input, in input order, plus accounting.
type Result struct {
	Vectors [][]float32
	Usage   Usage
	Metrics RequestMetrics
}

// ClientConfig contains bounded operational settings outside policy identity.
type ClientConfig struct {
	APIKey         string
	Timeout        time.Duration
	MaxRetries     int
	RetryBaseDelay time.Duration
	HTTPClient     *http.Client
}

// Client calls the single endpoint derived from its policy.
type Client struct {
	policy         Policy
	apiKey         string
	timeout        time.Duration
	maxRetries     int
	retryBaseDelay time.Duration
	http           *http.Client
	now            func() time.Time
}

// NewClient validates operational bounds without making a network request.
func NewClient(policy Policy, config ClientConfig) (*Client, error) {
	if !policy.valid() {
		return nil, errors.New("voyage policy is invalid; use NewPolicy")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, errors.New("voyage client API key is required")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = DefaultMaxRetries
	}
	if config.RetryBaseDelay == 0 {
		config.RetryBaseDelay = defaultRetryBaseDelay
	}
	if config.Timeout < 0 || config.Timeout > MaxTimeout || config.MaxRetries < 1 ||
		config.MaxRetries > MaxRetries || config.RetryBaseDelay < 0 || config.RetryBaseDelay > maxRetryAfter {
		return nil, errors.New("voyage client operational bounds are invalid")
	}
	// Never follow redirects: a redirect could replay media bytes and the
	// bearer credential to a host outside the pinned endpoint.
	httpClient := &http.Client{Timeout: config.Timeout}
	if config.HTTPClient != nil {
		clone := *config.HTTPClient
		httpClient = &clone
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		policy: policy, apiKey: config.APIKey, timeout: config.Timeout, maxRetries: config.MaxRetries,
		retryBaseDelay: config.RetryBaseDelay, http: httpClient, now: time.Now,
	}, nil
}

// Policy returns the policy the client was built with.
func (c *Client) Policy() Policy { return c.policy }

// EmbedDocuments embeds a batch of documents. Every media part must be
// covered by an authorization from this client's policy; a batch of more than
// one input requires the batch capability and a text-then-media input
// requires the interleaved capability.
func (c *Client) EmbedDocuments(ctx context.Context, inputs []Input, authorizations []Authorization) (Result, error) {
	if len(inputs) == 0 {
		return Result{}, nil
	}
	authorized, err := c.authorizationSet(authorizations)
	if err != nil {
		return Result{}, err
	}
	if len(inputs) > c.policy.values.MaxBatchItems {
		return Result{}, &ProviderError{Kind: ErrBatchTooLarge}
	}
	if len(inputs) > 1 && !authorized[CapabilityBatchLimits] {
		return Result{}, fmt.Errorf("%w: batches of %d inputs require %s", ErrCapabilityContract, len(inputs), CapabilityBatchLimits)
	}
	if estimatedRequestBytes(inputs) > c.policy.values.MaxRequestBytes {
		return Result{}, &ProviderError{Kind: ErrBatchTooLarge}
	}
	wireInputs := make([]wireInput, len(inputs))
	for index, input := range inputs {
		content, err := c.documentContent(input, authorized, c.policy.values.Media)
		if err != nil {
			return Result{}, fmt.Errorf("voyage document %d: %w", index, err)
		}
		wireInputs[index] = wireInput{Content: content}
	}
	vectors, usage, metrics, err := c.embed(ctx, wireInputs, inputTypeDocument)
	if err != nil {
		return Result{}, err
	}
	return Result{Vectors: vectors, Usage: usage, Metrics: metrics}, nil
}

// EmbedQuery embeds one query shaped [text], [image], or [text, image]. Text
// needs the text-query capability, an image needs the query capability for
// its own format, and the combined shape also needs the text-and-image
// capability.
func (c *Client) EmbedQuery(ctx context.Context, input Input, authorizations ...Authorization) ([]float32, Usage, error) {
	authorized, err := c.authorizationSet(authorizations)
	if err != nil {
		return nil, Usage{}, err
	}
	if estimatedRequestBytes([]Input{input}) > c.policy.values.MaxRequestBytes {
		return nil, Usage{}, &ProviderError{Kind: ErrBatchTooLarge}
	}
	content, err := c.queryContent(input, authorized, c.policy.values.Media)
	if err != nil {
		return nil, Usage{}, err
	}
	vectors, usage, _, err := c.embed(ctx, []wireInput{{Content: content}}, inputTypeQuery)
	if err != nil {
		return nil, Usage{}, err
	}
	return vectors[0], usage, nil
}

func (c *Client) authorizationSet(authorizations []Authorization) (map[string]bool, error) {
	authorized := make(map[string]bool, len(authorizations))
	for _, authorization := range authorizations {
		if authorization.policyDigest == "" || authorization.policyDigest != c.policy.digest {
			return nil, fmt.Errorf("%w: authorization was not derived from this client's policy", ErrCapabilityContract)
		}
		authorized[authorization.capability.ID] = true
	}
	return authorized, nil
}

func (c *Client) documentContent(input Input, authorized map[string]bool, mediaPolicy media.Policy) ([]wireContentPart, error) {
	// Only the probed shapes are accepted: [media] or [text, media].
	var textPart, mediaPart *Part
	switch len(input.Parts) {
	case 1:
		mediaPart = &input.Parts[0]
	case 2:
		textPart, mediaPart = &input.Parts[0], &input.Parts[1]
	default:
		return nil, fmt.Errorf("%w: document must be [media] or [text, media]", ErrInvalidInput)
	}
	if mediaPart.Media == nil || mediaPart.Text != "" {
		return nil, fmt.Errorf("%w: document must end with exactly one media part", ErrInvalidInput)
	}
	content := make([]wireContentPart, 0, 2)
	if textPart != nil {
		if textPart.Media != nil || strings.TrimSpace(textPart.Text) == "" {
			return nil, fmt.Errorf("%w: text part must precede the media part and be non-empty", ErrInvalidInput)
		}
		if !authorized[CapabilityInterleaved] {
			return nil, fmt.Errorf("%w: text with media requires %s", ErrCapabilityContract, CapabilityInterleaved)
		}
		content = append(content, wireContentPart{Type: "text", Text: textPart.Text})
	}
	detected, capability, err := c.verifyMedia(mediaPart.Media, mediaPolicy)
	if err != nil {
		return nil, err
	}
	if !authorized[capability.ID] {
		return nil, fmt.Errorf("%w: media requires %s", ErrCapabilityContract, capability.ID)
	}
	return append(content, wireMediaPart(detected, mediaPart.Media.Bytes)), nil
}

func (c *Client) queryContent(input Input, authorized map[string]bool, mediaPolicy media.Policy) ([]wireContentPart, error) {
	// Only the probed shapes are accepted: [text], [image], or [text, image].
	var textPart, imagePart *Part
	switch len(input.Parts) {
	case 1:
		if input.Parts[0].Media != nil {
			imagePart = &input.Parts[0]
		} else {
			textPart = &input.Parts[0]
		}
	case 2:
		textPart, imagePart = &input.Parts[0], &input.Parts[1]
	default:
		return nil, fmt.Errorf("%w: query must be [text], [image], or [text, image]", ErrInvalidInput)
	}
	content := make([]wireContentPart, 0, 2)
	if textPart != nil {
		if textPart.Media != nil || strings.TrimSpace(textPart.Text) == "" {
			return nil, fmt.Errorf("%w: query text part must be non-empty text", ErrInvalidInput)
		}
		if !authorized[CapabilityQueryText] {
			return nil, fmt.Errorf("%w: text queries require %s", ErrCapabilityContract, CapabilityQueryText)
		}
		content = append(content, wireContentPart{Type: "text", Text: textPart.Text})
	}
	if imagePart != nil {
		if imagePart.Media == nil || imagePart.Text != "" {
			return nil, fmt.Errorf("%w: query image part must be media only", ErrInvalidInput)
		}
		detected, _, err := c.verifyMedia(imagePart.Media, mediaPolicy)
		if err != nil {
			return nil, err
		}
		if detected.Kind != media.KindImage || detected.Animated {
			return nil, fmt.Errorf("%w: query media must be a still image", ErrInvalidInput)
		}
		capability, ok := queryCapabilityFor(detected.Format)
		if !ok {
			return nil, fmt.Errorf("%w: query image format %s has no capability", ErrInvalidInput, detected.Format)
		}
		if !authorized[capability.ID] {
			return nil, fmt.Errorf("%w: %s queries require %s", ErrCapabilityContract, detected.Format, capability.ID)
		}
		content = append(content, wireMediaPart(detected, imagePart.Media.Bytes))
	}
	if textPart != nil && imagePart != nil && !authorized[CapabilityQueryTextImage] {
		return nil, fmt.Errorf("%w: text-and-image queries require %s", ErrCapabilityContract, CapabilityQueryTextImage)
	}
	return content, nil
}

// verifyMedia re-detects the bytes so callers cannot pass metadata that does
// not describe them, applies the policy media bounds, and returns the detected
// metadata that every request is serialized from.
func (c *Client) verifyMedia(part *Media, mediaPolicy media.Policy) (media.Metadata, Capability, error) {
	if len(part.Bytes) == 0 {
		return media.Metadata{}, Capability{}, fmt.Errorf("%w: media bytes are required", ErrInvalidInput)
	}
	detected, reason := media.InspectBytes(part.Bytes, part.Metadata.DeclaredMediaType, mediaPolicy)
	if reason != media.ReasonEligible {
		return media.Metadata{}, Capability{}, fmt.Errorf("%w: media is %s under the policy", ErrInvalidInput, reason)
	}
	if detected.Format != part.Metadata.Format || detected.Kind != part.Metadata.Kind ||
		detected.MediaType != part.Metadata.MediaType || detected.Animated != part.Metadata.Animated ||
		detected.Width != part.Metadata.Width || detected.Height != part.Metadata.Height {
		return media.Metadata{}, Capability{}, fmt.Errorf("%w: media metadata does not describe its bytes", ErrInvalidInput)
	}
	capability, ok := documentCapabilityFor(detected)
	if !ok {
		return media.Metadata{}, Capability{}, fmt.Errorf("%w: media format %s (animated=%t) has no capability", ErrInvalidInput, detected.Format, detected.Animated)
	}
	return detected, capability, nil
}

type wireRequest struct {
	Inputs          []wireInput `json:"inputs"`
	Model           string      `json:"model"`
	InputType       string      `json:"input_type"`
	Truncation      bool        `json:"truncation"`
	OutputDimension int         `json:"output_dimension"`
}

type wireInput struct {
	Content []wireContentPart `json:"content"`
}

type wireContentPart struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	ImageBase64 string `json:"image_base64,omitempty"`
	VideoBase64 string `json:"video_base64,omitempty"`
}

type wireResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Embedding []strictFloat `json:"embedding"`
		Index     *int          `json:"index"`
	} `json:"data"`
	Usage struct {
		TotalTokens *int64 `json:"total_tokens"`
	} `json:"usage"`
}

// strictFloat accepts only finite JSON numbers; null and non-numeric
// elements are malformed rather than silently zero.
type strictFloat float32

func (f *strictFloat) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || data[0] == 'n' || data[0] == '"' || data[0] == '[' || data[0] == '{' ||
		data[0] == 't' || data[0] == 'f' {
		return errors.New("embedding element must be a number")
	}
	value, err := strconv.ParseFloat(string(data), 32)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("embedding element must be a finite number")
	}
	*f = strictFloat(value)
	return nil
}

func wireMediaPart(detected media.Metadata, data []byte) wireContentPart {
	dataURL := "data:" + detected.MediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
	if detected.Kind == media.KindVideo {
		return wireContentPart{Type: "video_base64", VideoBase64: dataURL}
	}
	return wireContentPart{Type: "image_base64", ImageBase64: dataURL}
}

const (
	// requestOverheadBytes covers the request envelope: model, input type,
	// truncation, and dimension keys.
	requestOverheadBytes = 512
	// partOverheadBytes covers one content part's keys, quotes, separators,
	// and data-URL prefix.
	partOverheadBytes = 96
	// textEscapeFactor is the worst-case JSON expansion of one text byte
	// (\uXXXX).
	textEscapeFactor = 6
)

// estimatedRequestBytes is an upper bound on the encoded request, computed
// before any base64 or JSON allocation so oversized batches are refused
// without allocating them. Text is charged at its worst-case escaped size and
// media at its exact base64 size.
func estimatedRequestBytes(inputs []Input) int64 {
	total := int64(requestOverheadBytes)
	for _, input := range inputs {
		total += partOverheadBytes
		for _, part := range input.Parts {
			total += partOverheadBytes + int64(len(part.Text))*textEscapeFactor
			if part.Media != nil {
				total += int64(base64.StdEncoding.EncodedLen(len(part.Media.Bytes)))
			}
		}
	}
	return total
}

func (c *Client) embed(ctx context.Context, inputs []wireInput, inputType string) ([][]float32, Usage, RequestMetrics, error) {
	body, err := json.Marshal(wireRequest{
		Inputs: inputs, Model: c.policy.values.Model, InputType: inputType,
		Truncation: false, OutputDimension: c.policy.values.Dimension,
	})
	if err != nil {
		return nil, Usage{}, RequestMetrics{}, fmt.Errorf("marshal Voyage request: %w", err)
	}
	if int64(len(body)) > c.policy.values.MaxRequestBytes {
		return nil, Usage{}, RequestMetrics{}, &ProviderError{Kind: ErrBatchTooLarge}
	}
	metrics := RequestMetrics{}
	malformedRetried := false
	for attempt := 1; ; attempt++ {
		metrics.Requests++
		attemptStarted := c.now()
		vectors, usage, requestErr := c.doOnce(ctx, body, len(inputs))
		if elapsed := c.now().Sub(attemptStarted); elapsed > 0 {
			metrics.Latency += elapsed
		}
		if requestErr == nil {
			return vectors, usage, metrics, nil
		}
		var providerErr *ProviderError
		if !errors.As(requestErr, &providerErr) {
			return nil, Usage{}, metrics, &ProviderError{Kind: requestErr, Metrics: metrics}
		}
		providerErr.Metrics = metrics
		retryable := errors.Is(providerErr.Kind, ErrTransientResponse)
		if errors.Is(providerErr.Kind, ErrMalformedResponse) && !malformedRetried {
			retryable, malformedRetried = true, true
		}
		if !retryable || attempt >= c.maxRetries {
			return nil, Usage{}, metrics, providerErr
		}
		delay := retryBackoffDelay(c.retryBaseDelay, attempt)
		if providerErr.RetrySet {
			delay = providerErr.RetryAfter
		}
		if err := sleepContext(ctx, delay); err != nil {
			return nil, Usage{}, metrics, &ProviderError{Kind: err, Metrics: metrics}
		}
		metrics.Retries++
	}
}

func retryBackoffDelay(base time.Duration, attempt int) time.Duration {
	multiplier := int64(1 << min(attempt-1, maxBackoffShift))
	if base >= time.Duration(int64(maxRetryAfter)/multiplier) {
		return maxRetryAfter
	}
	return time.Duration(int64(base) * multiplier)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) doOnce(ctx context.Context, body []byte, want int) ([][]float32, Usage, error) {
	attemptCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.policy.values.Endpoint+embeddingsPath, bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("build Voyage request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Usage{}, ctx.Err()
		}
		return nil, Usage{}, &ProviderError{Kind: ErrTransientResponse, cause: err}
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, Usage{}, &ProviderError{Kind: ErrUnauthorized, StatusCode: response.StatusCode}
	case response.StatusCode == http.StatusRequestEntityTooLarge:
		return nil, Usage{}, &ProviderError{Kind: ErrBatchTooLarge, StatusCode: response.StatusCode}
	case response.StatusCode == http.StatusTooManyRequests:
		retryAfter, retrySet := parseRetryAfter(response.Header.Get("Retry-After"), c.now())
		return nil, Usage{}, &ProviderError{Kind: ErrTransientResponse, StatusCode: response.StatusCode, RetryAfter: retryAfter, RetrySet: retrySet}
	case response.StatusCode >= http.StatusInternalServerError:
		return nil, Usage{}, &ProviderError{Kind: ErrTransientResponse, StatusCode: response.StatusCode}
	case response.StatusCode == http.StatusBadRequest:
		limited, readErr := readBounded(response.Body, maxErrorBodyBytes)
		if readErr == nil && sizeRejection(limited) {
			return nil, Usage{}, &ProviderError{Kind: ErrBatchTooLarge, StatusCode: response.StatusCode}
		}
		return nil, Usage{}, &ProviderError{Kind: ErrPermanentResponse, StatusCode: response.StatusCode}
	case response.StatusCode >= http.StatusBadRequest:
		return nil, Usage{}, &ProviderError{Kind: ErrPermanentResponse, StatusCode: response.StatusCode}
	case response.StatusCode != http.StatusOK:
		return nil, Usage{}, &ProviderError{Kind: ErrMalformedResponse, StatusCode: response.StatusCode}
	}

	payload, err := readBounded(response.Body, c.policy.values.MaxResponseBytes)
	if err != nil {
		return nil, Usage{}, &ProviderError{Kind: ErrMalformedResponse, StatusCode: response.StatusCode, cause: err}
	}
	if err := manifestjson.RejectDuplicateKeys(payload, "voyage response"); err != nil {
		return nil, Usage{}, &ProviderError{Kind: ErrMalformedResponse, StatusCode: response.StatusCode}
	}
	var decoded wireResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, Usage{}, &ProviderError{Kind: ErrMalformedResponse, StatusCode: response.StatusCode}
	}
	vectors, usage, ok := c.decodeResponse(decoded, want)
	if !ok {
		return nil, Usage{}, &ProviderError{Kind: ErrMalformedResponse, StatusCode: response.StatusCode}
	}
	return vectors, usage, nil
}

func (c *Client) decodeResponse(response wireResponse, want int) ([][]float32, Usage, bool) {
	if response.Model != c.policy.values.Model || len(response.Data) != want {
		return nil, Usage{}, false
	}
	vectors := make([][]float32, want)
	seen := make([]bool, want)
	for _, item := range response.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= want || seen[*item.Index] ||
			len(item.Embedding) != c.policy.values.Dimension {
			return nil, Usage{}, false
		}
		vector := make([]float32, len(item.Embedding))
		var norm float64
		for index, value := range item.Embedding {
			vector[index] = float32(value)
			norm += float64(value) * float64(value)
		}
		// A zero vector is not an embedding; it would compare as equidistant
		// to everything and cannot be evidence of anything.
		if norm == 0 {
			return nil, Usage{}, false
		}
		seen[*item.Index] = true
		vectors[*item.Index] = vector
	}
	usage := Usage{}
	if response.Usage.TotalTokens != nil && *response.Usage.TotalTokens >= 0 {
		usage = Usage{TotalTokens: *response.Usage.TotalTokens, Available: true}
	}
	return vectors, usage, true
}

var errResponseTooLarge = errors.New("voyage response exceeds configured limit")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read Voyage response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func sizeRejection(body []byte) bool {
	message := strings.ToLower(string(body))
	for _, marker := range []string{
		"too large", "too many", "exceed", "maximum", "context length", "total number of tokens",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
