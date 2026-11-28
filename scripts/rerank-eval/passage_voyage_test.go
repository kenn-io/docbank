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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestPassageVoyageAccountsAttemptAndRejectsMalformedScores(t *testing.T) {
	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	require.NoError(t, err)
	body := `{"data":[{"index":1,"relevance_score":0.8},{"index":0,"relevance_score":0.5}],"usage":{"total_tokens":7}}`
	var received int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Documents  []string `json:"documents"`
			Truncation *bool    `json:"truncation"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		assert.Equal(t, []string{"first", "second"}, request.Documents)
		if assert.NotNil(t, request.Truncation) {
			assert.False(t, *request.Truncation)
		}
		received++
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	runner := &passageVoyageRunner{endpoint: server.URL, client: server.Client(), secret: func() string { return "synthetic" }, codec: codec, budget: &passageBudget{started: time.Now()}}
	inputs := []embeddingeval.PassageInput{{DocumentID: "a", Text: "first"}, {DocumentID: "b", Passage: 1, Text: "second"}}
	got, err := runner.RerankPassages(t.Context(), "query", inputs)
	require.NoError(t, err)
	require.Len(t, got.Scores, 2)
	require.Equal(t, []string{"b", "a"}, []string{got.Scores[0].DocumentID, got.Scores[1].DocumentID})
	require.Equal(t, []int{1, 0}, []int{got.Scores[0].Passage, got.Scores[1].Passage})
	require.InDeltaSlice(t, []float64{.8, .5}, []float64{got.Scores[0].Score, got.Scores[1].Score}, 1e-12)
	require.InDelta(t, 7.0, *got.Usage.TokenUsage, 1e-12)
	require.Equal(t, 4, got.LocalTokens)
	require.Greater(t, got.RequestBytes, len("firstsecond"))
	body = `{"data":[{"index":0},{"index":1,"relevance_score":2}]}`
	got, err = runner.RerankPassages(t.Context(), "query", inputs)
	require.Error(t, err, "missing score cannot silently become zero")
	require.Positive(t, got.RequestBytes)
	require.Equal(t, 2, received)
	before := received
	runner.budget.EstimatedMicros = 25_000_000
	_, err = runner.RerankPassages(t.Context(), "query", inputs)
	require.Error(t, err)
	require.Equal(t, before, received, "budget rejects before sending")
}
