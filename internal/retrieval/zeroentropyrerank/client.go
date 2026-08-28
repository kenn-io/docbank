package zeroentropyrerank

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
	"unicode/utf8"

	"go.kenn.io/docbank/internal/retrieval"
)

const (
	maximumSecretBytes    = 64 << 10
	maximumUsage          = int64(1 << 50)
	maximumLatencySeconds = float64(24 * 60 * 60)
	providerPayloadMax    = int64(5_000_000)
)

var _ retrieval.RerankingProvider = (*Client)(nil)

type wireRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
	Latency   Latency  `json:"latency,omitempty"`
}

type wireResponse struct {
	Results           []wireResult `json:"results"`
	TotalBytes        *int64       `json:"total_bytes"`
	TotalTokens       *int64       `json:"total_tokens"`
	ActualLatencyMode Latency      `json:"actual_latency_mode"`
	E2ELatency        *float64     `json:"e2e_latency"`
	InferenceLatency  *float64     `json:"inference_latency"`
}

type wireResult struct {
	Index          *int     `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
}

type Receipt struct {
	PolicyFingerprint       string
	Model                   string
	ModelRevision           string
	RequestedLatency        Latency
	ActualLatency           Latency
	CandidateCount          int
	TotalBytes              int64
	TotalTokens             int64
	E2ELatencySeconds       float64
	InferenceLatencySeconds float64
}

type Execution struct {
	Scores  []retrieval.RerankScore
	Receipt Receipt
}

func (client *Client) Rerank(ctx context.Context, request retrieval.RerankingRequest) ([]retrieval.RerankScore, error) {
	execution, err := client.rerank(ctx, request, false)
	return execution.Scores, err
}

func (client *Client) RerankWithReceipt(ctx context.Context, request retrieval.RerankingRequest) (Execution, error) {
	return client.rerank(ctx, request, true)
}

func (client *Client) rerank(ctx context.Context, request retrieval.RerankingRequest, includeReceipt bool) (Execution, error) {
	if client == nil || ctx == nil {
		return Execution{}, errors.New("zeroentropy rerank: client and context are required")
	}
	if err := client.validateRequest(request); err != nil {
		return Execution{}, err
	}
	documents := make([]string, len(request.Candidates))
	for index, candidate := range request.Candidates {
		documents[index] = candidate.Excerpt
	}
	latency := client.profile.Latency
	if latency == LatencyAuto {
		latency = ""
	}
	payload, err := json.Marshal(wireRequest{Model: Model, Query: request.Query, Documents: documents,
		TopN: len(documents), Latency: latency})
	if err != nil {
		return Execution{}, errors.New("zeroentropy rerank: request encoding failed")
	}
	defer clear(payload)
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		return Execution{}, &ProviderError{Kind: ErrCapacityResponse}
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !validSecret(secret) {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return Execution{}, fmt.Errorf("zeroentropy rerank: credential resolution canceled: %w", contextErr)
		}
		return Execution{}, errors.New("zeroentropy rerank: API-key resolution failed")
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, origin+rerankPath, bytes.NewReader(payload))
	if err != nil {
		return Execution{}, errors.New("zeroentropy rerank: request construction failed")
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(httpRequest)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return Execution{}, fmt.Errorf("zeroentropy rerank: request canceled: %w", contextErr)
		}
		return Execution{}, &ProviderError{Kind: ErrTransientResponse}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Execution{}, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, readErr := readBounded(response.Body, client.profile.MaxResponseBytes)
	defer clear(body)
	if contextErr := requestCtx.Err(); contextErr != nil {
		return Execution{}, fmt.Errorf("zeroentropy rerank: response read canceled: %w", contextErr)
	}
	if readErr != nil {
		if errors.Is(readErr, errCapacity) {
			return Execution{}, &ProviderError{Kind: ErrCapacityResponse}
		}
		return Execution{}, &ProviderError{Kind: ErrTransientResponse}
	}
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil || !client.validResponse(decoded, len(request.Candidates)) {
		return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	scores := make([]retrieval.RerankScore, len(request.Candidates))
	seen := make([]bool, len(request.Candidates))
	for _, result := range decoded.Results {
		if result.Index == nil || result.RelevanceScore == nil || *result.Index < 0 || *result.Index >= len(request.Candidates) ||
			seen[*result.Index] || math.IsNaN(*result.RelevanceScore) || math.IsInf(*result.RelevanceScore, 0) ||
			*result.RelevanceScore < 0 || *result.RelevanceScore > 1 {
			return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		seen[*result.Index] = true
		scores[*result.Index] = retrieval.RerankScore{Document: request.Candidates[*result.Index].Document, Score: *result.RelevanceScore}
	}
	if slices.Contains(seen, false) {
		return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	execution := Execution{Scores: scores}
	if includeReceipt {
		execution.Receipt = Receipt{PolicyFingerprint: client.fingerprint, Model: Model,
			ModelRevision: client.profile.ModelRevision, RequestedLatency: client.profile.Latency,
			ActualLatency: decoded.ActualLatencyMode, CandidateCount: len(request.Candidates),
			TotalBytes: *decoded.TotalBytes, TotalTokens: *decoded.TotalTokens,
			E2ELatencySeconds: *decoded.E2ELatency, InferenceLatencySeconds: *decoded.InferenceLatency}
	}
	return execution, nil
}

func (client *Client) validateRequest(request retrieval.RerankingRequest) error {
	if strings.TrimSpace(request.Query) == "" || !utf8.ValidString(request.Query) {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	if len(request.Query) > client.profile.MaxQueryBytes || len(request.Candidates) == 0 || len(request.Candidates) > client.profile.MaxCandidates {
		return &ProviderError{Kind: ErrCapacityResponse}
	}
	seen := make(map[retrieval.DocumentIdentity]struct{}, len(request.Candidates))
	var excerptBytes, providerBytes int64
	for _, candidate := range request.Candidates {
		if candidate.Document.VaultID == "" || candidate.Document.NodeID <= 0 || candidate.Document.ContentVersionID == "" {
			return &ProviderError{Kind: ErrPermanentResponse}
		}
		if !utf8.ValidString(candidate.Excerpt) || len(candidate.Excerpt) > client.profile.MaxExcerptBytes ||
			int64(len(candidate.Excerpt)) > client.profile.MaxTotalExcerptBytes-excerptBytes {
			return &ProviderError{Kind: ErrCapacityResponse}
		}
		excerptBytes += int64(len(candidate.Excerpt))
		increment := int64(150 + len(request.Query) + len(candidate.Excerpt))
		if increment > providerPayloadMax-providerBytes {
			return &ProviderError{Kind: ErrCapacityResponse}
		}
		providerBytes += increment
		if _, duplicate := seen[candidate.Document]; duplicate {
			return &ProviderError{Kind: ErrPermanentResponse}
		}
		seen[candidate.Document] = struct{}{}
	}
	return nil
}

func (client *Client) validResponse(response wireResponse, candidates int) bool {
	if len(response.Results) != candidates || response.TotalBytes == nil || response.TotalTokens == nil ||
		*response.TotalBytes < 0 || *response.TotalBytes > maximumUsage || *response.TotalTokens < 0 || *response.TotalTokens > maximumUsage ||
		(response.ActualLatencyMode != LatencyFast && response.ActualLatencyMode != LatencySlow) ||
		response.E2ELatency == nil || response.InferenceLatency == nil {
		return false
	}
	if client.profile.Latency != LatencyAuto && response.ActualLatencyMode != client.profile.Latency {
		return false
	}
	for _, value := range []float64{*response.E2ELatency, *response.InferenceLatency} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maximumLatencySeconds {
			return false
		}
	}
	return *response.InferenceLatency <= *response.E2ELatency
}

var errCapacity = errors.New("response capacity exceeded")

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
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

func validSecret(value string) bool { return len(value) <= maximumSecretBytes && validToken(value) }
