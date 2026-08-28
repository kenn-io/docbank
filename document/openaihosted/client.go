package openaihosted

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const (
	maxSecretBytes      = 64 << 10
	unitLengthTolerance = 1e-4
)

var _ document.EmbeddingProvider = (*Client)(nil)

type wireRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

type wireResponse struct {
	Object string          `json:"object"`
	Data   []wireEmbedding `json:"data"`
	Model  string          `json:"model"`
	Usage  *wireUsage      `json:"usage"`
}

type wireEmbedding struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     *int      `json:"index"`
}

type wireUsage struct {
	PromptTokens *int64 `json:"prompt_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
}

// Embed sends one bounded text-only request to the fixed hosted endpoint.
func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if client == nil || ctx == nil {
		return document.EmbeddingResult{}, errors.New("openaihosted: client and context are required")
	}
	if err := document.ValidateEmbeddingProviderRequest(client, inputs, authorization); err != nil {
		return document.EmbeddingResult{}, err
	}
	if authorization.MaxBatchItems > client.profile.MaxBatchItems || authorization.MaxInputBytes > client.profile.MaxInputBytes ||
		authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrCapacityResponse}
	}
	rendered := make([]string, len(inputs))
	var total int64
	for index, input := range inputs {
		if input.Role == document.EmbeddingRoleDocument {
			rendered[index] = client.descriptor.ModelInput.EncodeDocument(input.Text)
		} else {
			rendered[index] = client.descriptor.ModelInput.EncodeQuery(input.Text)
		}
		length := int64(len(rendered[index]))
		if length > client.profile.MaxInputItemBytes || length > client.profile.MaxInputBytes-total {
			return document.EmbeddingResult{}, &ProviderError{Kind: ErrCapacityResponse}
		}
		total += length
	}
	payload, err := json.Marshal(wireRequest{Input: rendered, Model: Model, Dimensions: client.descriptor.Dimension, EncodingFormat: "float"})
	if err != nil {
		return document.EmbeddingResult{}, errors.New("openaihosted: request encoding failed")
	}
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrCapacityResponse}
	}

	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !validSecret(secret) {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, fmt.Errorf("openaihosted: credential resolution canceled: %w", contextErr)
		}
		return document.EmbeddingResult{}, errors.New("openaihosted: API-key resolution failed")
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, origin+embeddingsPath, bytes.NewReader(payload))
	if err != nil {
		return document.EmbeddingResult{}, errors.New("openaihosted: request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return document.EmbeddingResult{}, fmt.Errorf("openaihosted: request canceled: %w", contextErr)
		}
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrTransientResponse}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return document.EmbeddingResult{}, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if err := requireJSONContentType(response.Header.Get("Content-Type")); err != nil {
		return document.EmbeddingResult{}, err
	}
	body, err := readBounded(requestCtx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	result, err := client.validateAndOrder(decoded, inputs)
	if err != nil {
		return document.EmbeddingResult{}, err
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	return result, nil
}

func (client *Client) validateAndOrder(response wireResponse, inputs []document.EmbeddingInput) (document.EmbeddingResult, error) {
	if response.Object != "list" || response.Model != Model || response.Usage == nil ||
		response.Usage.PromptTokens == nil || response.Usage.TotalTokens == nil ||
		*response.Usage.PromptTokens < 0 || *response.Usage.TotalTokens < *response.Usage.PromptTokens || len(response.Data) != len(inputs) {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range response.Data {
		if item.Object != "embedding" || item.Index == nil || *item.Index < 0 || *item.Index >= len(inputs) || seen[*item.Index] {
			return document.EmbeddingResult{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		if err := client.validateVector(item.Embedding); err != nil {
			return document.EmbeddingResult{}, err
		}
		seen[*item.Index] = true
		vectors[*item.Index] = document.EmbeddingVector{Key: inputs[*item.Index].Key, Values: slices.Clone(item.Embedding)}
	}
	if slices.Contains(seen, false) {
		return document.EmbeddingResult{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func (client *Client) validateVector(vector []float32) error {
	if len(vector) != client.descriptor.Dimension {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	var norm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return &ProviderError{Kind: ErrPermanentResponse}
		}
		norm += float64(value) * float64(value)
	}
	if math.Abs(norm-1) > unitLengthTolerance {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	return nil
}

func validSecret(value string) bool {
	return value != "" && len(value) <= maxSecretBytes && utf8.ValidString(value) && strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || unicode.IsSpace(character)
	}) < 0
}

func requireJSONContentType(value string) error {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	if len(parameters) == 0 {
		return nil
	}
	charset, ok := parameters["charset"]
	if len(parameters) != 1 || !ok || !strings.EqualFold(charset, "utf-8") {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	return nil
}

func readBounded(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("openaihosted: response read canceled: %w", contextErr)
		}
		return nil, &ProviderError{Kind: ErrTransientResponse}
	}
	if int64(len(body)) > maximum {
		return nil, &ProviderError{Kind: ErrCapacityResponse}
	}
	return body, nil
}
