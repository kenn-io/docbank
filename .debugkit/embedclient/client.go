package embedclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	jsonv2 "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

// Options configures a text embedding client.
type Options struct {
	Model      embedconfig.Model
	Roles      embedconfig.Roles
	Deployment embedconfig.Deployment
	Batch      embedconfig.Batch
	Transport  embedconfig.Transport
	// APIKey is the resolved bearer token. It is not part of any identity.
	// Empty means the request does not add an Authorization header.
	APIKey string
	// HTTP is an optional caller-owned client. New clones it and leaves the
	// original unchanged. Nil builds a client from Transport.Timeout.
	HTTP *http.Client
	// OllamaMetalRecovery recovers a response that contains an unusable
	// vector when the endpoint is an Ollama /v1 URL. Valid vectors are kept.
	// The bad inputs are sent to Ollama's native embed route, which unloads
	// the current runner, retries that runner once, and then tries once with
	// the GPU disabled. Those recovery requests use a 30 minute timeout in
	// place of Transport.Timeout and the caller's client timeout; the Embed
	// context still bounds them.
	// The switch does not change the vector identity.
	OllamaMetalRecovery bool
	// Retry retries requests that fail in a way that may succeed later. The
	// zero value does not retry.
	Retry Retry
}

// Client sends text embedding requests.
type Client struct {
	http           *http.Client
	recoveryHTTP   *http.Client // ollamaRecoveryTimeout instead of Transport.Timeout
	endpoint       string
	model          embedconfig.Model
	roles          embedconfig.Roles
	batchItems     int // same-role cap after applying a token budget
	maxResponse    int64
	serviceURL     string
	ollamaRecovery bool
	retry          Retry
}

// New validates opts and returns a client pinned to the deployment origin.
// The metric must be cosine, and the encoding format must be empty, float,
// or base64. A token budget lowers the same-role item cap.
func New(opts Options) (*Client, error) {
	model, err := opts.Model.Prepared()
	if err != nil {
		return nil, err
	}
	if err := validateEncodedModel(model); err != nil {
		return nil, err
	}
	roles, err := opts.Roles.Prepared()
	if err != nil {
		return nil, err
	}
	deployment, err := opts.Deployment.Prepared()
	if err != nil {
		return nil, err
	}
	batch, err := opts.Batch.Prepared()
	if err != nil {
		return nil, err
	}
	items, err := batchItemCap(batch)
	if err != nil {
		return nil, err
	}
	transport, err := opts.Transport.Prepared()
	if err != nil {
		return nil, err
	}
	canonical, err := deployment.Canonical()
	if err != nil {
		return nil, err
	}
	if opts.OllamaMetalRecovery {
		if err := validateOllamaService(canonical); err != nil {
			return nil, err
		}
	}
	parsed, err := url.Parse(canonical)
	if err != nil {
		return nil, fmt.Errorf("embed endpoint is invalid: %w", err)
	}
	origin, err := embedconfig.Origin(parsed)
	if err != nil {
		return nil, err
	}
	pinned := pinClient(opts.HTTP, origin, opts.APIKey, transport.Timeout)
	// A CPU re-encode or a model reload can take minutes, far past a
	// per-request timeout, so recovery has its own, much longer bound.
	recovery := *pinned
	recovery.Timeout = ollamaRecoveryTimeout
	return &Client{
		http:           pinned,
		recoveryHTTP:   &recovery,
		endpoint:       embeddingsURL(canonical),
		serviceURL:     canonical,
		ollamaRecovery: opts.OllamaMetalRecovery,
		retry:          opts.Retry,
		model:          model,
		roles:          roles,
		batchItems:     items,
		maxResponse:    int64(transport.MaxResponseBytes),
	}, nil
}

// Embed encodes inputs in request order. An empty list returns nil.
// Inputs that do not share a role are sent in separate requests because
// input_type is one field per request.
func (c *Client) Embed(ctx context.Context, inputs []embedmodel.Content) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	return c.embed(ctx, inputs, true)
}

func (c *Client) embed(ctx context.Context, inputs []embedmodel.Content, format bool) ([][]float32, error) {
	prepared, err := c.prepare(inputs, format)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(prepared))
	for start := 0; start < len(prepared); {
		end := start + 1
		for end < len(prepared) && prepared[end].role == prepared[start].role && end-start < c.batchItems {
			end++
		}
		texts := make([]string, end-start)
		for i := start; i < end; i++ {
			texts[i-start] = prepared[i].text
		}
		role := prepared[start].role
		vectors, err := c.retry.do(ctx, func() ([][]float32, error) {
			return c.post(ctx, role, texts)
		})
		if err != nil {
			first := start
			return nil, remapVectorIndex(err, func(i int) int { return prepared[first+i].index })
		}
		for i, vec := range vectors {
			out[prepared[start+i].index] = vec
		}
		start = end
	}
	return out, nil
}

// EmbedTexts encodes plain texts for one role, like Embed with a text Content
// per string. The role prefix and suffix are applied.
func (c *Client) EmbedTexts(ctx context.Context, role embedconfig.Role, texts []string) ([][]float32, error) {
	inputs := make([]embedmodel.Content, len(texts))
	for i, text := range texts {
		inputs[i] = embedmodel.Content{Role: role, Text: text}
	}
	return c.Embed(ctx, inputs)
}

// EncodeFunc adapts one role to vector.EncodeFunc.
// The texts are sent as given. A fitter's prepared text already includes
// its role prefix and suffix, so this path does not add them again.
// New rejects every metric other than cosine, so this cannot feed the
// cosine pipeline from a different distance.
func (c *Client) EncodeFunc(role embedconfig.Role) vector.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		if len(texts) == 0 {
			return nil, nil
		}
		switch role {
		case embedconfig.RoleDocument, embedconfig.RoleQuery:
		default:
			return nil, errors.New("embed role must be document or query")
		}
		inputs := make([]embedmodel.Content, len(texts))
		for i, text := range texts {
			if embedmodel.BlankText(text) {
				return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
			}
			inputs[i] = embedmodel.Content{Role: role, Kind: embedmodel.KindText, Text: text}
		}
		return c.embed(ctx, inputs, false)
	}
}

type encoded struct {
	index int
	role  embedconfig.Role
	text  string
}

func (c *Client) prepare(inputs []embedmodel.Content, format bool) ([]encoded, error) {
	out := make([]encoded, len(inputs))
	for i, input := range inputs {
		text, err := input.EmbedText()
		if err != nil {
			return nil, fmt.Errorf("embed input %d: %w", i, err)
		}
		if embedmodel.BlankText(text) {
			return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
		}
		if format {
			text, err = embedmodel.Format(input.Role, text, c.roles)
			if err != nil {
				return nil, fmt.Errorf("embed input %d: %w", i, err)
			}
			if embedmodel.BlankText(text) {
				return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
			}
		}
		out[i] = encoded{index: i, role: input.Role, text: text}
	}
	return out, nil
}

func (c *Client) post(ctx context.Context, role embedconfig.Role, texts []string) ([][]float32, error) {
	body, err := c.requestBody(role, texts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("embed request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(resp)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	if int64(len(payload)) > c.maxResponse {
		return nil, ErrResponseTooLarge
	}
	vectors, problems, err := c.classify(payload, len(texts))
	if err != nil {
		return nil, err
	}
	if len(problems) == 0 {
		return vectors, nil
	}
	if !c.ollamaRecovery {
		return nil, problems[0]
	}
	return c.recoverOllama(ctx, texts, vectors, problems)
}

func embeddingsURL(canonical string) string {
	if strings.HasSuffix(canonical, "/embeddings") {
		return canonical
	}
	return canonical + "/embeddings"
}

// requestBody is split so the wire struct stays next to the encoder.
func (c *Client) requestBody(role embedconfig.Role, texts []string) ([]byte, error) {
	payload := wireRequest{Model: c.model.Name, Input: texts}
	if c.roles.InputType == embedconfig.InputTypeRetrieval {
		payload.InputType = string(role)
	}
	if c.model.RequestDimensions {
		payload.Dimensions = c.model.Dimensions
	}
	if c.model.EncodingFormat != "" {
		payload.EncodingFormat = c.model.EncodingFormat
	}
	body, err := jsonv2.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed request encoding failed: %w", err)
	}
	return body, nil
}

func validateEncodedModel(model embedconfig.Model) error {
	if model.Metric != embedconfig.MetricCosine {
		return errors.New("embed model metric must be cosine")
	}
	switch model.EncodingFormat {
	case "", "float", "base64":
		return nil
	default:
		return errors.New("embed encoding format must be empty, float, or base64")
	}
}

// batchItemCap is how many same-role inputs fit in one request.
// With a token budget the cap is min(Items, MaxTokens/InputTokenUpperBound).
// An input that cannot fit in MaxTokens is an error, not a one-item request.
func batchItemCap(batch embedconfig.Batch) (int, error) {
	if batch.MaxTokens == 0 && batch.InputTokenUpperBound == 0 {
		return batch.Items, nil
	}
	if batch.MaxTokens <= 0 || batch.InputTokenUpperBound <= 0 || batch.InputTokenUpperBound > batch.MaxTokens {
		return 0, errors.New("embed batch per-input token bound must fit in the aggregate cap")
	}
	byTokens := batch.MaxTokens / batch.InputTokenUpperBound
	if byTokens < 1 {
		return 0, errors.New("embed batch per-input token bound must fit in the aggregate cap")
	}
	items := min(batch.Items, byTokens)
	if items < 1 {
		return 0, errors.New("embed batch items must be positive")
	}
	return items, nil
}
