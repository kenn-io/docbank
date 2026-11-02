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
	"sync"
	"time"

	jsonv2 "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
)

// ollamaUnloadTimeout bounds the wait for Ollama to unload the runner after
// a keep_alive of 0. A large model may take longer; recovery then skips the
// GPU retry and goes straight to the CPU pass.
const ollamaUnloadTimeout = 10 * time.Second

// ollamaRecoveryTimeout bounds each recovery embed request in place of the
// per-request timeout. A model reload or a CPU re-encode can take many
// minutes; this only stops a server that never answers.
const ollamaRecoveryTimeout = 30 * time.Minute

var ollamaRecoveryGates sync.Map

type ollamaEmbedRequest struct {
	Model      string              `json:"model"`
	Input      []string            `json:"input"`
	Truncate   bool                `json:"truncate"`
	Dimensions int                 `json:"dimensions,omitzero"`
	Options    *ollamaEmbedOptions `json:"options,omitzero"`
	KeepAlive  string              `json:"keep_alive,omitzero"`
}

type ollamaEmbedOptions struct {
	NumGPU int `json:"num_gpu"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]*float64 `json:"embeddings"`
}

type ollamaProcessResponse struct {
	Models []struct {
		Model string `json:"model"`
		Name  string `json:"name"`
	} `json:"models"`
}

func validateOllamaService(canonical string) error {
	if _, _, err := ollamaNativeURLs(canonical); err != nil {
		return err
	}
	return nil
}

func ollamaNativeURLs(canonical string) (embedURL, psURL string, err error) {
	embedURL, err = ollamaAPIURL(canonical, "/api/embed")
	if err != nil {
		return "", "", err
	}
	psURL, err = ollamaAPIURL(canonical, "/api/ps")
	if err != nil {
		return "", "", err
	}
	return embedURL, psURL, nil
}

func ollamaAPIURL(endpoint, apiPath string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("ollama recovery endpoint is invalid: %w", err)
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		return "", fmt.Errorf("ollama recovery requires an endpoint path ending in /v1, got %q", parsed.Path)
	}
	parsed.Path = strings.TrimSuffix(path, "/v1") + apiPath
	parsed.RawPath = ""
	return parsed.String(), nil
}

// recoverOllama keeps the usable vectors and replaces the unusable inputs.
// The first native request unloads the current runner. If that request does
// not itself return usable vectors, the runner is retried once, and then the
// inputs are encoded once with the GPU disabled. Recovery for one native URL
// runs one at a time.
func (c *Client) recoverOllama(ctx context.Context, texts []string, vectors [][]float32, problems []error) ([][]float32, error) {
	embedURL, psURL, err := ollamaNativeURLs(c.serviceURL)
	if err != nil {
		return nil, err
	}
	gate := ollamaGate(embedURL)
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, &TransportError{Err: ctx.Err()}
	}
	defer func() { <-gate }()

	indexes := make([]int, len(problems))
	inputs := make([]string, len(problems))
	for i, problem := range problems {
		vectorErr, ok := errors.AsType[*VectorError](problem)
		if !ok {
			return nil, problem
		}
		indexes[i] = vectorErr.Index
		inputs[i] = texts[vectorErr.Index]
	}
	// Recovery requests number only the bad inputs; report batch positions.
	batchIndex := func(i int) int { return indexes[i] }

	recovered, unloadErr := c.ollamaNativeEmbed(ctx, embedURL, inputs, nil, "0s")
	waitErr := c.waitForOllamaUnload(ctx, psURL)
	if unloadErr == nil && waitErr == nil {
		return mergeVectors(vectors, indexes, recovered), nil
	}
	unloadErr = remapVectorIndex(unloadErr, batchIndex)
	var reloadErr error
	if waitErr == nil {
		recovered, reloadErr = c.ollamaNativeEmbed(ctx, embedURL, inputs, nil, "")
		if reloadErr == nil {
			return mergeVectors(vectors, indexes, recovered), nil
		}
		reloadErr = remapVectorIndex(reloadErr, batchIndex)
	}
	recovered, cpuErr := c.ollamaNativeEmbed(ctx, embedURL, inputs, &ollamaEmbedOptions{NumGPU: 0}, "0s")
	if cpuErr != nil {
		cpuErr = remapVectorIndex(cpuErr, batchIndex)
		return nil, errors.Join(unloadErr, waitErr, reloadErr, fmt.Errorf("ollama CPU recovery: %w", cpuErr))
	}
	return mergeVectors(vectors, indexes, recovered), nil
}

func (c *Client) ollamaNativeEmbed(ctx context.Context, embedURL string, inputs []string, options *ollamaEmbedOptions, keepAlive string) ([][]float32, error) {
	body := ollamaEmbedRequest{
		Model: c.model.Name, Input: inputs, Truncate: false,
		Options: options, KeepAlive: keepAlive,
	}
	if c.model.RequestDimensions {
		body.Dimensions = c.model.Dimensions
	}
	payload, err := jsonv2.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, embedURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.recoveryHTTP.Do(req)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	if int64(len(raw)) > c.maxResponse {
		return nil, ErrResponseTooLarge
	}
	var decoded ollamaEmbedResponse
	if err := jsonv2.Unmarshal(raw, &decoded); err != nil {
		return nil, errors.New("embed response is invalid")
	}
	if len(decoded.Embeddings) != len(inputs) {
		return nil, fmt.Errorf("ollama embed returned %d vectors for %d inputs", len(decoded.Embeddings), len(inputs))
	}
	out := make([][]float32, len(decoded.Embeddings))
	for i, row := range decoded.Embeddings {
		vector, err := normalizeNative(row, c.model.Dimensions, c.model.Normalization)
		if err != nil {
			return nil, &VectorError{Index: i, Err: err}
		}
		out[i] = vector
	}
	return out, nil
}

func normalizeNative(row []*float64, dims int, normalization embedconfig.Normalization) ([]float32, error) {
	values, err := finiteFloat32s(row)
	if err != nil {
		return nil, err
	}
	return decodeVector(wireEmbedding{values: values}, dims, normalization)
}

func (c *Client) waitForOllamaUnload(ctx context.Context, psURL string) error {
	ctx, cancel := context.WithTimeout(ctx, ollamaUnloadTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		loaded, err := c.ollamaModelLoaded(ctx, psURL)
		if err != nil {
			return err
		}
		if !loaded {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) ollamaModelLoaded(ctx context.Context, psURL string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, psURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return false, classifyTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, newAPIError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return false, &TransportError{Err: err}
	}
	var decoded ollamaProcessResponse
	if err := jsonv2.Unmarshal(raw, &decoded); err != nil {
		return false, errors.New("embed response is invalid")
	}
	for _, model := range decoded.Models {
		if ollamaNamesMatch(c.model.Name, model.Model) || ollamaNamesMatch(c.model.Name, model.Name) {
			return true, nil
		}
	}
	return false, nil
}

func ollamaNamesMatch(configured, loaded string) bool {
	return ollamaDisplayName(configured) == ollamaDisplayName(loaded)
}

func ollamaDisplayName(name string) string {
	if _, rest, ok := strings.Cut(name, "://"); ok {
		name = rest
	}
	parts := strings.Split(name, "/")
	if len(parts) >= 3 && strings.EqualFold(parts[0], "registry.ollama.ai") {
		parts = parts[1:]
		if len(parts) == 2 && strings.EqualFold(parts[0], "library") {
			parts = parts[1:]
		}
	}
	last := len(parts) - 1
	if last >= 0 && !strings.Contains(parts[last], ":") {
		parts[last] += ":latest"
	}
	return strings.Join(parts, "/")
}

func mergeVectors(primary [][]float32, indexes []int, recovered [][]float32) [][]float32 {
	merged := make([][]float32, len(primary))
	copy(merged, primary)
	for i, index := range indexes {
		merged[index] = recovered[i]
	}
	return merged
}

// ollamaGate returns a one-slot semaphore for embedURL. A channel lets a
// waiting request give up when its context ends.
func ollamaGate(embedURL string) chan struct{} {
	gate, _ := ollamaRecoveryGates.LoadOrStore(embedURL, make(chan struct{}, 1))
	return gate.(chan struct{})
}
