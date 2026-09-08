package geminiembed

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
	"strings"
	"time"

	"go.kenn.io/docbank/document/internal/providerutil"
)

const maxUsageValue = int64(1 << 50)

type wireRequest struct {
	Model                string      `json:"model"`
	Content              wireContent `json:"content"`
	OutputDimensionality int         `json:"outputDimensionality"`
}

type wireContent struct {
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text       string          `json:"text,omitzero"`
	InlineData *wireInlineData `json:"inlineData,omitempty"`
	FileData   *wireFileData   `json:"fileData,omitempty"`
}

type wireInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFileData struct {
	MIMEType string `json:"mimeType"`
	FileURI  string `json:"fileUri"`
}

type wireResponse struct {
	Embedding     wireEmbedding `json:"embedding"`
	UsageMetadata wireUsage     `json:"usageMetadata,omitzero"`
}

type wireEmbedding struct {
	Values []float32 `json:"values"`
	Shape  wireShape `json:"shape,omitzero"`
}

type wireShape struct {
	values []int64
	set    bool
}

func (shape *wireShape) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("embedding shape must be an array")
	}
	var values []int64
	if err := json.Unmarshal(data, &values); err != nil {
		return errors.New("embedding shape must be an integer array")
	}
	shape.values = values
	shape.set = true
	return nil
}

type tokenCount struct {
	value int64
	set   bool
}

func (count *tokenCount) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("token count must be an integer")
	}
	var value int64
	if err := json.Unmarshal(data, &value); err != nil || value < 0 || value > maxUsageValue {
		return errors.New("token count must be a bounded nonnegative integer")
	}
	count.value = value
	count.set = true
	return nil
}

type wireModalityTokenCount struct {
	Modality   string     `json:"modality"`
	TokenCount tokenCount `json:"tokenCount,omitzero"`
}

type wirePromptTokenDetails struct {
	values []wireModalityTokenCount
	set    bool
}

func (details *wirePromptTokenDetails) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("prompt token details must be an array")
	}
	var values []wireModalityTokenCount
	if err := json.Unmarshal(data, &values, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	details.values = values
	details.set = true
	return nil
}

type wireUsage struct {
	PromptTokenCount   tokenCount             `json:"promptTokenCount,omitzero"`
	PromptTokenDetails wirePromptTokenDetails `json:"promptTokenDetails,omitzero"`
	set                bool
}

func (usage *wireUsage) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("usage metadata must be an object")
	}
	type usageFields struct {
		PromptTokenCount   tokenCount             `json:"promptTokenCount,omitzero"`
		PromptTokenDetails wirePromptTokenDetails `json:"promptTokenDetails,omitzero"`
	}
	var decoded usageFields
	if err := json.Unmarshal(data, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	usage.PromptTokenCount = decoded.PromptTokenCount
	usage.PromptTokenDetails = decoded.PromptTokenDetails
	usage.set = true
	return nil
}

func (client *Client) marshalRequest(part wirePart) ([]byte, error) {
	payload, err := json.Marshal(wireRequest{Model: "models/" + Model,
		Content: wireContent{Parts: []wirePart{part}}, OutputDimensionality: client.descriptor.Dimension})
	if err != nil {
		return nil, errors.New("gemini embed: request encoding failed")
	}
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		clear(payload)
		return nil, errors.New("gemini embed: embedding request exceeds profile byte capacity")
	}
	return payload, nil
}

func (client *Client) execute(ctx context.Context, payload []byte, secret string, receipt *Receipt) ([]float32, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+embedPath, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("gemini embed: request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Goog-Api-Key", secret)
	if !beginReceiptRequest(receipt) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("gemini embed: request canceled: %w", contextErr)
		}
		return nil, errors.New("gemini embed: provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if response.StatusCode != http.StatusOK {
		return nil, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, err := readBounded(ctx, response.Body, client.profile.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true),
		json.WithUnmarshalers(json.UnmarshalFunc(providerutil.UnmarshalEmbeddingFloat32))); err != nil {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	var usage *wireUsage
	if decoded.UsageMetadata.set {
		usage = &decoded.UsageMetadata
	}
	if len(decoded.Embedding.Values) != client.descriptor.Dimension || len(decoded.Embedding.Shape.values) != 0 || !validUsage(usage) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if !normalizeUnitVector(decoded.Embedding.Values) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if !recordReceiptResponse(receipt, usage, response.Header.Get("X-Goog-Request-Id")) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	return decoded.Embedding.Values, nil
}

func normalizeUnitVector(vector []float32) bool {
	var squaredNorm float64
	for _, value := range vector {
		floatValue := float64(value)
		if math.IsNaN(floatValue) || math.IsInf(floatValue, 0) {
			return false
		}
		squaredNorm += floatValue * floatValue
	}
	if squaredNorm == 0 || math.IsInf(squaredNorm, 0) {
		return false
	}
	norm := math.Sqrt(squaredNorm)
	for index, value := range vector {
		vector[index] = float32(float64(value) / norm)
	}
	return true
}

func isJSONContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return false
	}
	if len(parameters) == 0 {
		return true
	}
	charset, ok := parameters["charset"]
	return len(parameters) == 1 && ok && strings.EqualFold(charset, "utf-8")
}

func readBounded(ctx context.Context, body io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maximum+1))
	if contextErr := ctx.Err(); contextErr != nil {
		clear(data)
		return nil, fmt.Errorf("gemini embed: response read canceled: %w", contextErr)
	}
	if err != nil {
		clear(data)
		return nil, fmt.Errorf("gemini embed: provider response read failed: %w", err)
	}
	if int64(len(data)) > maximum {
		clear(data)
		return nil, errors.New("gemini embed: provider response exceeds byte capacity")
	}
	return data, nil
}

func validUsage(usage *wireUsage) bool {
	if usage == nil {
		return true
	}
	if usage.PromptTokenCount.set && (usage.PromptTokenCount.value < 0 || usage.PromptTokenCount.value > maxUsageValue) {
		return false
	}
	if len(usage.PromptTokenDetails.values) > 6 {
		return false
	}
	seen := make(map[string]struct{}, len(usage.PromptTokenDetails.values))
	for _, detail := range usage.PromptTokenDetails.values {
		if !detail.TokenCount.set || detail.TokenCount.value < 0 || detail.TokenCount.value > maxUsageValue {
			return false
		}
		switch detail.Modality {
		case "MODALITY_UNSPECIFIED", "TEXT", "IMAGE", "VIDEO", "AUDIO", "DOCUMENT":
		default:
			return false
		}
		if _, duplicate := seen[detail.Modality]; duplicate {
			return false
		}
		seen[detail.Modality] = struct{}{}
	}
	return true
}
