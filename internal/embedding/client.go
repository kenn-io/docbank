// Package embedding calls an OpenAI-compatible embeddings endpoint. It owns
// no storage; vector generations and persistence live in internal/vector.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	kitvec "go.kenn.io/kit/vector"
)

// Config is the resolved endpoint and vector-space identity.
type Config struct {
	BaseURL         string
	Model           string
	APIKey          string
	FingerprintSalt string
	Dimensions      int
	BatchSize       int
	Timeout         time.Duration
}

// Client calls one origin-pinned /embeddings endpoint.
type Client struct {
	http       *http.Client
	baseURL    string
	model      string
	apiKey     string
	salt       string
	dimensions int
	batchSize  int
}

// New validates cfg and constructs a client whose redirects cannot carry
// document text or bearer credentials to another origin.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("embedding: base URL and model are required")
	}
	if cfg.Dimensions < 1 || cfg.BatchSize < 1 || cfg.Timeout <= 0 {
		return nil, errors.New("embedding: positive dimensions, batch size, and timeout are required")
	}
	originURL, err := url.Parse(cfg.BaseURL)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" {
		return nil, fmt.Errorf("embedding: invalid base URL %q", cfg.BaseURL)
	}
	origin := originURL.Scheme + "://" + originURL.Host
	hc := &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme+"://"+req.URL.Host != origin {
				return errors.New("embedding: refusing redirect away from configured origin")
			}
			return nil
		},
	}
	return &Client{
		http: hc, baseURL: strings.TrimRight(cfg.BaseURL, "/"), model: cfg.Model,
		apiKey: cfg.APIKey,
		salt:   cfg.FingerprintSalt, dimensions: cfg.Dimensions, batchSize: cfg.BatchSize,
	}, nil
}

// Generation identifies the exact vector space and text recipe. Endpoint
// location is transport, not identity; FingerprintSalt distinguishes changed
// weights published under the same model name.
func (c *Client) Generation() kitvec.Generation {
	params := map[string]string{}
	if c.salt != "" {
		params["salt"] = c.salt
	}
	return kitvec.Generation{Model: c.model, Dimensions: c.dimensions, Params: params}
}

// EncodeFunc adapts the client to Kit's fill and query flows.
func (c *Client) EncodeFunc() kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) (vectors [][]float32, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("embedding: encoder panic: %v", recovered)
			}
		}()
		return c.Embed(ctx, texts)
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int             `json:"index"`
		Embedding embeddingVector `json:"embedding"`
	} `json:"data"`
}

type embeddingVector []float32

func (v *embeddingVector) UnmarshalJSON(data []byte) error {
	var values []*float32
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	out := make([]float32, len(values))
	for i, value := range values {
		if value == nil {
			return fmt.Errorf("component %d is null", i)
		}
		out[i] = *value
	}
	*v = out
	return nil
}

// Embed returns one normalized, finite vector per input in input order.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := min(start+c.batchSize, len(texts))
		vectors, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embedding: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	maxBytes := int64(c.dimensions)*int64(len(texts))*16 + (1 << 20)
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("embedding: read response: %w", readErr)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("embedding: response exceeds %d bytes", maxBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding: endpoint returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var decoded embedResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("embedding: decode response: %w", err)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: endpoint returned %d vectors for %d inputs",
			len(decoded.Data), len(texts))
	}
	vectors := make([][]float32, len(texts))
	seen := make([]bool, len(texts))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(texts) || seen[item.Index] {
			return nil, fmt.Errorf("embedding: invalid or duplicate response index %d", item.Index)
		}
		seen[item.Index] = true
		vector := []float32(item.Embedding)
		if len(vector) != c.dimensions {
			return nil, fmt.Errorf("embedding: response vector has %d dimensions, want %d",
				len(vector), c.dimensions)
		}
		if err := normalize(vector); err != nil {
			return nil, err
		}
		vectors[item.Index] = vector
	}
	return vectors, nil
}

func normalize(vector []float32) error {
	var sum float64
	for i, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("embedding: component %d is not finite", i)
		}
		sum += float64(value) * float64(value)
	}
	if sum == 0 {
		return errors.New("embedding: vector has zero norm")
	}
	scale := float32(1 / math.Sqrt(sum))
	for i := range vector {
		vector[i] *= scale
	}
	return nil
}
