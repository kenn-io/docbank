package rerankeval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/tiktoken-go/tokenizer"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
)

// This offline adapter has no retries; identities never depend on response order.
type passageVoyageRunner struct {
	endpoint string
	client   *http.Client
	secret   func() string
	codec    tokenizer.Codec
	budget   *passageBudget
}

func (r *passageVoyageRunner) RerankPassages(ctx context.Context, query string, inputs []embeddingeval.PassageInput) (result embeddingeval.PassageScores, err error) {
	texts := make([]string, len(inputs))
	for i, input := range inputs {
		n, e := r.codec.Count(query)
		if e != nil {
			return result, fmt.Errorf("count rerank query tokens: %w", e)
		}
		m, e := r.codec.Count(input.Text)
		if e != nil {
			return result, fmt.Errorf("count rerank passage tokens: %w", e)
		}
		result.LocalTokens += n + m
		texts[i] = input.Text
	}
	body, err := json.Marshal(struct {
		Model      string   `json:"model"`
		Query      string   `json:"query"`
		Documents  []string `json:"documents"`
		TopK       int      `json:"top_k"`
		Truncation bool     `json:"truncation"`
	}{"rerank-2.5", query, texts, len(texts), false})
	if err != nil {
		return result, err
	}
	result.RequestBytes = len(body)
	if err = r.budget.reserve(result.LocalTokens, 0.05); err != nil {
		return result, err
	}
	result.Usage.ProviderCalls = 1
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+r.secret())
	req.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(req)
	if err != nil {
		return result, errors.New("Voyage rerank transport failed")
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	if response.StatusCode != http.StatusOK {
		return result, errors.New("Voyage rerank returned non-200 status")
	}
	var wire struct {
		Data []struct {
			Index *int     `json:"index"`
			Score *float64 `json:"relevance_score"`
		} `json:"data"`
		Usage struct {
			TotalTokens *float64 `json:"total_tokens"`
		} `json:"usage"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return result, err
	}
	if len(data) > 1<<20 {
		return result, errors.New("Voyage response exceeds bound")
	}
	if err = json.Unmarshal(data, &wire); err != nil {
		return result, errors.New("malformed Voyage response")
	}
	for _, item := range wire.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(inputs) || item.Score == nil {
			return result, errors.New("invalid Voyage score identity")
		}
		i := *item.Index
		result.Scores = append(result.Scores, embeddingeval.PassageScore{DocumentID: inputs[i].DocumentID, Passage: inputs[i].Passage, Score: *item.Score})
	}
	result.Usage.TokenUsage = wire.Usage.TotalTokens
	chargedTokens := float64(result.LocalTokens)
	basis := "2026-10-30 list price; cl100k_base estimate; $0.05/million"
	if observed := wire.Usage.TotalTokens; observed != nil && *observed >= 0 {
		chargedTokens = *observed
		r.budget.ObservedRerankTokens += *observed
		r.budget.EstimatedMicros += max(0, *observed-float64(result.LocalTokens)) * .05
		basis = "2026-10-30 list price; observed Voyage tokens; $0.05/million"
	}
	result.Usage.Cost = &embeddingeval.CostObservation{Micros: int64(math.Ceil(chargedTokens * .05)), Basis: basis}
	return result, nil
}

type passageBudget struct {
	started                 time.Time
	EstimatedMicros         float64       `json:"EstimatedMicros"`
	EmbeddingLocalTokens    int           `json:"EmbeddingLocalTokens"`
	EmbeddingReservedMicros float64       `json:"EmbeddingReservedMicros"`
	ObservedRerankTokens    float64       `json:"ObservedRerankTokens"`
	Elapsed                 time.Duration `json:"Elapsed"`
}

func (b *passageBudget) reserve(tokens int, rate float64) error {
	cost := float64(tokens) * rate
	if time.Since(b.started) >= 2*time.Hour || b.EstimatedMicros+cost > 25_000_000 {
		return errors.New("fixed provider budget or two-hour limit reached")
	}
	b.EstimatedMicros += cost
	return nil
}
