package eval_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
)

func TestEvaluateReportsRetrievalQualityAndProviderUsage(t *testing.T) {
	corpus := embeddingeval.Corpus{
		ID: "synthetic-documents", Version: "1",
		Documents: []embeddingeval.Document{
			{ID: "revenue", Text: "Synthetic quarterly revenue increased."},
			{ID: "risk", Text: "Synthetic supply risk disclosure."},
			{ID: "other", Text: "Unrelated synthetic material."},
		},
		Queries: []embeddingeval.Query{
			{ID: "q-revenue", Text: "sales growth", Judgments: []embeddingeval.Judgment{{DocumentID: "revenue", Grade: 3, Critical: true}}},
			{ID: "q-risk", Text: "supply problem", Judgments: []embeddingeval.Judgment{{DocumentID: "risk", Grade: 2}}},
		},
	}
	runner := staticRunner{rankings: map[string][]string{
		"q-revenue": {"other", "revenue", "risk"},
		"q-risk":    {"risk", "other", "revenue"},
	}}
	report, err := embeddingeval.Evaluate(context.Background(), corpus, []embeddingeval.System{{
		ID: "raw", RecipeFingerprint: "recipe-fingerprint", VectorSpaceFingerprint: "space-fingerprint",
	}}, 2, runner)
	require.NoError(t, err)
	require.Len(t, report.Systems, 1)
	require.Len(t, report.Systems[0].Trials, 2)
	assert.InDelta(t, 0.75, report.Systems[0].Trials[0].Metrics.MRR, 0.0001)
	assert.InDelta(t, 1.0, report.Systems[0].Trials[0].Metrics.RecallAt5, 0.0001)
	assert.Zero(t, report.Systems[0].Trials[0].Metrics.CriticalMisses)
	assert.Equal(t, 2, report.Systems[0].Trials[0].Usage.ProviderCalls)
	assert.Equal(t, 22, report.Systems[0].Trials[0].Usage.ProviderInputRunes)
	assert.Equal(t, 4*time.Millisecond, report.Systems[0].Trials[0].Usage.Latency)
	assert.InDelta(t, report.Systems[0].Aggregate.MRR.Min, report.Systems[0].Aggregate.MRR.Max, 0.0001)
}

type staticRunner struct {
	rankings map[string][]string
}

func (runner staticRunner) Search(_ context.Context, _ embeddingeval.System, _ embeddingeval.Corpus, query embeddingeval.Query) (embeddingeval.SearchResult, error) {
	return embeddingeval.SearchResult{
		DocumentIDs: runner.rankings[query.ID],
		Usage: embeddingeval.Usage{
			ProviderCalls: 1, ProviderInputRunes: 11, ProviderOutputUnits: 3,
			EstimatedCostMicros: 7, Latency: 2 * time.Millisecond,
		},
	}, nil
}
