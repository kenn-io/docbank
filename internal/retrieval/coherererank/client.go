package coherererank

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/cohereapi"
	"go.kenn.io/docbank/internal/retrieval"
)

const (
	maxSecretBytes = 64 << 10
	maxUsageValue  = float64(1 << 50)
)

var _ retrieval.RerankingProvider = (*Client)(nil)

type wireRequest struct {
	Model           Model    `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n"`
	MaxTokensPerDoc int      `json:"max_tokens_per_doc"`
}

type wireResponse struct {
	ID      string       `json:"id"`
	Results []wireResult `json:"results"`
	Meta    *wireMeta    `json:"meta,omitempty"`
}

type wireResult struct {
	Index          *int     `json:"index"`
	RelevanceScore *float64 `json:"relevance_score"`
}

type wireMeta struct {
	APIVersion *struct {
		Version        string `json:"version"`
		IsDeprecated   *bool  `json:"is_deprecated,omitempty"`
		IsExperimental *bool  `json:"is_experimental,omitempty"`
	} `json:"api_version,omitempty"`
	BilledUnits *struct {
		Images          *float64 `json:"images,omitempty"`
		InputTokens     *float64 `json:"input_tokens,omitempty"`
		ImageTokens     *float64 `json:"image_tokens,omitempty"`
		OutputTokens    *float64 `json:"output_tokens,omitempty"`
		SearchUnits     *float64 `json:"search_units,omitempty"`
		Classifications *float64 `json:"classifications,omitempty"`
		Pages           *float64 `json:"pages,omitempty"`
	} `json:"billed_units,omitempty"`
	Tokens *struct {
		InputTokens  *float64 `json:"input_tokens,omitempty"`
		OutputTokens *float64 `json:"output_tokens,omitempty"`
	} `json:"tokens,omitempty"`
	CachedTokens *float64 `json:"cached_tokens,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

type Receipt struct {
	PolicyFingerprint  string
	Model              Model
	ModelRevision      string
	CandidateCount     int
	BilledImages       float64
	InputTokens        float64
	ImageTokens        float64
	OutputTokens       float64
	SearchUnits        float64
	Classifications    float64
	Pages              float64
	CachedTokens       float64
	ProviderResponseID string
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
		return Execution{}, errors.New("cohere rerank: client and context are required")
	}
	if err := client.validateRequest(request); err != nil {
		return Execution{}, err
	}
	documents := make([]string, len(request.Candidates))
	for index, candidate := range request.Candidates {
		documents[index] = candidate.Excerpt
	}
	payload, err := json.Marshal(wireRequest{Model: client.profile.Model, Query: request.Query,
		Documents: documents, TopN: len(documents), MaxTokensPerDoc: client.profile.MaxTokensPerDocument})
	if err != nil {
		return Execution{}, errors.New("cohere rerank: request encoding failed")
	}
	defer clear(payload)
	if int64(len(payload)) > client.profile.MaxRequestBytes {
		return Execution{}, &ProviderError{Kind: ErrCapacityResponse}
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !cohereapi.ValidToken(secret, maxSecretBytes) {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return Execution{}, fmt.Errorf("cohere rerank: credential resolution canceled: %w", contextErr)
		}
		return Execution{}, errors.New("cohere rerank: API-key resolution failed")
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, origin+rerankPath, bytes.NewReader(payload))
	if err != nil {
		return Execution{}, errors.New("cohere rerank: request construction failed")
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(httpRequest)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return Execution{}, fmt.Errorf("cohere rerank: request canceled: %w", contextErr)
		}
		return Execution{}, &ProviderError{Kind: ErrTransientResponse}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Execution{}, statusError(response.StatusCode, response.Header.Get("Retry-After"), time.Now().UTC())
	}
	if !cohereapi.IsJSONContentType(response.Header.Get("Content-Type")) {
		return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	body, outcome, readErr := cohereapi.ReadBounded(requestCtx, response.Body, client.profile.MaxResponseBytes)
	switch outcome {
	case cohereapi.ReadOK:
	case cohereapi.ReadCanceled:
		return Execution{}, fmt.Errorf("cohere rerank: response read canceled: %w", readErr)
	case cohereapi.ReadTransient:
		return Execution{}, &ProviderError{Kind: ErrTransientResponse}
	case cohereapi.ReadCapacity:
		return Execution{}, &ProviderError{Kind: ErrCapacityResponse}
	}
	defer clear(body)
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	scores, err := validateResults(decoded, request.Candidates)
	if err != nil {
		return Execution{}, err
	}
	execution := Execution{Scores: scores}
	if includeReceipt {
		receipt, ok := client.receipt(decoded, len(request.Candidates))
		if !ok {
			return Execution{}, &ProviderError{Kind: ErrPermanentResponse}
		}
		execution.Receipt = receipt
	}
	return execution, nil
}

func (client *Client) validateRequest(request retrieval.RerankingRequest) error {
	if strings.TrimSpace(request.Query) == "" || !utf8.ValidString(request.Query) {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	if len(request.Query) > client.profile.MaxQueryBytes ||
		len(request.Candidates) == 0 || len(request.Candidates) > client.profile.MaxCandidates {
		return &ProviderError{Kind: ErrCapacityResponse}
	}
	seen := make(map[retrieval.DocumentIdentity]struct{}, len(request.Candidates))
	var total int64
	for _, candidate := range request.Candidates {
		if !utf8.ValidString(candidate.Excerpt) || len(candidate.Excerpt) > client.profile.MaxExcerptBytes ||
			int64(len(candidate.Excerpt)) > client.profile.MaxTotalExcerptBytes-total {
			return &ProviderError{Kind: ErrCapacityResponse}
		}
		total += int64(len(candidate.Excerpt))
		if _, duplicate := seen[candidate.Document]; duplicate {
			return &ProviderError{Kind: ErrPermanentResponse}
		}
		seen[candidate.Document] = struct{}{}
	}
	return nil
}

func validateResults(response wireResponse, candidates []retrieval.RerankingCandidate) ([]retrieval.RerankScore, error) {
	if response.ID != "" && !cohereapi.ValidToken(response.ID, 128) || len(response.Results) != len(candidates) || !validMeta(response.Meta) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	scores := make([]retrieval.RerankScore, len(candidates))
	seen := make([]bool, len(candidates))
	for _, result := range response.Results {
		if result.Index == nil || result.RelevanceScore == nil || *result.Index < 0 || *result.Index >= len(candidates) ||
			seen[*result.Index] || math.IsNaN(*result.RelevanceScore) || math.IsInf(*result.RelevanceScore, 0) ||
			*result.RelevanceScore < 0 || *result.RelevanceScore > 1 {
			return nil, &ProviderError{Kind: ErrPermanentResponse}
		}
		seen[*result.Index] = true
		scores[*result.Index] = retrieval.RerankScore{Document: candidates[*result.Index].Document, Score: *result.RelevanceScore}
	}
	if slices.Contains(seen, false) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	return scores, nil
}

func (client *Client) receipt(response wireResponse, candidates int) (Receipt, bool) {
	receipt := Receipt{PolicyFingerprint: client.fingerprint,
		Model: client.profile.Model, ModelRevision: client.profile.ModelRevision,
		CandidateCount: candidates, ProviderResponseID: response.ID}
	if response.Meta != nil && response.Meta.BilledUnits != nil {
		copyUsage(&receipt.BilledImages, response.Meta.BilledUnits.Images)
		copyUsage(&receipt.InputTokens, response.Meta.BilledUnits.InputTokens)
		copyUsage(&receipt.ImageTokens, response.Meta.BilledUnits.ImageTokens)
		copyUsage(&receipt.OutputTokens, response.Meta.BilledUnits.OutputTokens)
		copyUsage(&receipt.SearchUnits, response.Meta.BilledUnits.SearchUnits)
		copyUsage(&receipt.Classifications, response.Meta.BilledUnits.Classifications)
		copyUsage(&receipt.Pages, response.Meta.BilledUnits.Pages)
	}
	if response.Meta != nil {
		copyUsage(&receipt.CachedTokens, response.Meta.CachedTokens)
		if response.Meta.Tokens != nil {
			if response.Meta.BilledUnits == nil || response.Meta.BilledUnits.InputTokens == nil {
				copyUsage(&receipt.InputTokens, response.Meta.Tokens.InputTokens)
			}
			if response.Meta.BilledUnits == nil || response.Meta.BilledUnits.OutputTokens == nil {
				copyUsage(&receipt.OutputTokens, response.Meta.Tokens.OutputTokens)
			}
		}
	}
	return receipt, validReceipt(receipt)
}

func copyUsage(target *float64, value *float64) {
	if value != nil {
		*target = *value
	}
}

func validReceipt(receipt Receipt) bool {
	for _, value := range []float64{receipt.BilledImages, receipt.InputTokens, receipt.ImageTokens,
		receipt.OutputTokens, receipt.SearchUnits, receipt.Classifications, receipt.Pages, receipt.CachedTokens} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxUsageValue {
			return false
		}
	}
	return true
}

func validMeta(metadata *wireMeta) bool {
	if metadata == nil {
		return true
	}
	if metadata.APIVersion != nil && metadata.APIVersion.Version == "" {
		return false
	}
	if metadata.BilledUnits != nil && metadata.Tokens != nil &&
		(!matchingUsage(metadata.BilledUnits.InputTokens, metadata.Tokens.InputTokens) ||
			!matchingUsage(metadata.BilledUnits.OutputTokens, metadata.Tokens.OutputTokens)) {
		return false
	}
	if metadata.BilledUnits != nil && metadata.BilledUnits.Images != nil && *metadata.BilledUnits.Images != 0 {
		return false
	}
	if metadata.BilledUnits != nil && metadata.BilledUnits.ImageTokens != nil && *metadata.BilledUnits.ImageTokens != 0 {
		return false
	}
	values := []*float64{}
	if metadata.BilledUnits != nil {
		values = append(values, metadata.BilledUnits.Images, metadata.BilledUnits.InputTokens,
			metadata.BilledUnits.ImageTokens, metadata.BilledUnits.OutputTokens,
			metadata.BilledUnits.SearchUnits, metadata.BilledUnits.Classifications, metadata.BilledUnits.Pages)
	}
	if metadata.Tokens != nil {
		values = append(values, metadata.Tokens.InputTokens, metadata.Tokens.OutputTokens)
	}
	values = append(values, metadata.CachedTokens)
	for _, value := range values {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > maxUsageValue) {
			return false
		}
	}
	return true
}

func matchingUsage(left, right *float64) bool {
	return left == nil || right == nil || *left == *right
}
