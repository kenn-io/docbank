package zeroentropyembed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/docbank/document"
)

const (
	maximumSecretBytes = 64 << 10
	maximumUsage       = int64(1 << 50)
	providerPayloadMax = int64(5_000_000)
)

var _ document.EmbeddingProvider = (*Client)(nil)

type wireRequest struct {
	Model          string         `json:"model"`
	InputType      string         `json:"input_type"`
	Input          []string       `json:"input"`
	Dimensions     int            `json:"dimensions"`
	EncodingFormat EncodingFormat `json:"encoding_format"`
	Latency        Latency        `json:"latency,omitempty"`
}

type wireResponse struct {
	Results []wireResult `json:"results"`
	Usage   *wireUsage   `json:"usage"`
}

type wireResult struct {
	Embedding jsonv1.RawMessage `json:"embedding"`
}

type wireUsage struct {
	TotalBytes  *int64 `json:"total_bytes"`
	TotalTokens *int64 `json:"total_tokens"`
}

type preparedRequest struct {
	positions []int
	payload   []byte
}

type Receipt struct {
	ProviderID            string
	DescriptorFingerprint string
	PolicyFingerprint     string
	Model                 string
	ModelRevision         string
	EncodingFormat        EncodingFormat
	RequestedLatency      Latency
	RequestCount          int
	TotalBytes            int64
	TotalTokens           int64
}

type Execution struct {
	Result  document.EmbeddingResult
	Receipt Receipt
}

func (client *Client) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	execution, err := client.embed(ctx, inputs, authorization, false)
	return execution.Result, err
}

func (client *Client) EmbedWithReceipt(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (Execution, error) {
	return client.embed(ctx, inputs, authorization, true)
}

func (client *Client) embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization, includeReceipt bool) (Execution, error) {
	if client == nil || ctx == nil {
		return Execution{}, errors.New("zeroentropy embed: client and context are required")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	if err := document.ValidateEmbeddingProviderRequest(client, inputs, authorization); err != nil {
		return Execution{}, err
	}
	if authorization.MaxBatchItems > client.profile.MaxBatchItems || authorization.MaxInputBytes > client.profile.MaxInputBytes ||
		authorization.MaxResponseBytes > client.profile.MaxResponseBytes {
		return Execution{}, &ProviderError{Kind: ErrCapacityResponse}
	}
	prepared, err := client.prepareRequests(inputs)
	if err != nil {
		return Execution{}, err
	}
	defer func() {
		for index := range prepared {
			clear(prepared[index].payload)
		}
	}()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !validSecret(secret) {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return Execution{}, fmt.Errorf("zeroentropy embed: credential resolution canceled: %w", contextErr)
		}
		return Execution{}, errors.New("zeroentropy embed: API-key resolution failed")
	}
	result := document.EmbeddingResult{Vectors: make([]document.EmbeddingVector, len(inputs))}
	receipt := Receipt{ProviderID: ProviderID, DescriptorFingerprint: client.descriptor.Fingerprint,
		PolicyFingerprint: client.descriptor.PolicyFingerprint, Model: Model, ModelRevision: client.descriptor.ModelRevision,
		EncodingFormat: client.profile.EncodingFormat, RequestedLatency: client.profile.Latency}
	for _, request := range prepared {
		vectors, usage, executeErr := client.execute(requestCtx, request.payload, len(request.positions), secret)
		if executeErr != nil {
			return Execution{}, executeErr
		}
		for local, global := range request.positions {
			result.Vectors[global] = document.EmbeddingVector{Key: inputs[global].Key, Values: vectors[local]}
		}
		receipt.RequestCount++
		if receipt.TotalBytes > maximumUsage-*usage.TotalBytes || receipt.TotalTokens > maximumUsage-*usage.TotalTokens {
			return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		receipt.TotalBytes += *usage.TotalBytes
		receipt.TotalTokens += *usage.TotalTokens
	}
	if err := document.ValidateEmbeddingProviderResult(client.descriptor, inputs, authorization, result); err != nil {
		return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	execution := Execution{Result: result}
	if includeReceipt {
		execution.Receipt = receipt
	}
	return execution, nil
}

func (client *Client) prepareRequests(inputs []document.EmbeddingInput) ([]preparedRequest, error) {
	documentPositions, queryPositions := []int{}, []int{}
	documents, queries := []string{}, []string{}
	for index, input := range inputs {
		var rendered string
		var target *[]string
		var positions *[]int
		switch input.Role {
		case document.EmbeddingRoleDocument:
			rendered, target, positions = client.descriptor.ModelInput.EncodeDocument(input.Text), &documents, &documentPositions
		case document.EmbeddingRoleQuery:
			rendered, target, positions = client.descriptor.ModelInput.EncodeQuery(input.Text), &queries, &queryPositions
		default:
			return nil, &ProviderError{Kind: ErrPermanentResponse}
		}
		if int64(len(rendered)) > client.profile.MaxInputItemBytes {
			return nil, &ProviderError{Kind: ErrCapacityResponse}
		}
		*target = append(*target, rendered)
		*positions = append(*positions, index)
	}
	requests := make([]preparedRequest, 0, 2)
	for _, group := range []struct {
		positions []int
		values    []string
		inputType string
	}{{documentPositions, documents, "document"}, {queryPositions, queries, "query"}} {
		if len(group.positions) == 0 {
			continue
		}
		var providerBytes int64
		for _, value := range group.values {
			if int64(len(value))+150 > providerPayloadMax-providerBytes {
				return nil, &ProviderError{Kind: ErrCapacityResponse}
			}
			providerBytes += int64(len(value)) + 150
		}
		latency := client.profile.Latency
		if latency == LatencyAuto {
			latency = ""
		}
		payload, err := json.Marshal(wireRequest{Model: Model, InputType: group.inputType, Input: group.values,
			Dimensions: client.descriptor.Dimension, EncodingFormat: client.profile.EncodingFormat, Latency: latency})
		if err != nil {
			return nil, errors.New("zeroentropy embed: request encoding failed")
		}
		if int64(len(payload)) > client.profile.MaxRequestBytes {
			clear(payload)
			return nil, &ProviderError{Kind: ErrCapacityResponse}
		}
		requests = append(requests, preparedRequest{positions: slices.Clone(group.positions), payload: payload})
	}
	return requests, nil
}

func (client *Client) execute(ctx context.Context, payload []byte, expected int, secret string) ([][]float32, wireUsage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+embedPath, bytes.NewReader(payload))
	if err != nil {
		return nil, wireUsage{}, errors.New("zeroentropy embed: request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, wireUsage{}, fmt.Errorf("zeroentropy embed: request canceled: %w", contextErr)
		}
		return nil, wireUsage{}, &ProviderError{Kind: ErrTransientResponse}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, wireUsage{}, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return nil, wireUsage{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, readErr := readBounded(response.Body, client.profile.MaxResponseBytes)
	defer clear(body)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, wireUsage{}, fmt.Errorf("zeroentropy embed: response read canceled: %w", contextErr)
	}
	if readErr != nil {
		if errors.Is(readErr, errCapacity) {
			return nil, wireUsage{}, &ProviderError{Kind: ErrCapacityResponse}
		}
		return nil, wireUsage{}, &ProviderError{Kind: ErrTransientResponse}
	}
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil ||
		len(decoded.Results) != expected || !validUsage(decoded.Usage) {
		return nil, wireUsage{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	vectors := make([][]float32, expected)
	for index, result := range decoded.Results {
		vector, decodeErr := client.decodeVector(result.Embedding)
		if decodeErr != nil {
			return nil, wireUsage{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		vectors[index] = vector
	}
	return vectors, *decoded.Usage, nil
}

func (client *Client) decodeVector(raw jsonv1.RawMessage) ([]float32, error) {
	var values []float32
	if client.profile.EncodingFormat == EncodingFloat {
		if err := json.Unmarshal(raw, &values, json.RejectUnknownMembers(true)); err != nil {
			return nil, err
		}
	} else {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil || base64.StdEncoding.DecodedLen(len(encoded)) < client.descriptor.Dimension*4 {
			return nil, errors.New("invalid base64 vector")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) != client.descriptor.Dimension*4 {
			clear(decoded)
			return nil, errors.New("invalid base64 vector")
		}
		defer clear(decoded)
		values = make([]float32, client.descriptor.Dimension)
		for index := range values {
			values[index] = math.Float32frombits(binary.LittleEndian.Uint32(decoded[index*4:]))
		}
	}
	if len(values) != client.descriptor.Dimension {
		return nil, errors.New("invalid vector dimension")
	}
	for _, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, errors.New("non-finite vector")
		}
	}
	return values, nil
}

func validUsage(usage *wireUsage) bool {
	return usage != nil && usage.TotalBytes != nil && usage.TotalTokens != nil &&
		*usage.TotalBytes >= 0 && *usage.TotalBytes <= maximumUsage &&
		*usage.TotalTokens >= 0 && *usage.TotalTokens <= maximumUsage
}

var errCapacity = errors.New("response capacity exceeded")

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		clear(body)
		return nil, errCapacity
	}
	return body, nil
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (mediaType == "application/json" || len(mediaType) > 5 && mediaType[len(mediaType)-5:] == "+json")
}

func validSecret(value string) bool {
	return len(value) <= maximumSecretBytes && validToken(value)
}
