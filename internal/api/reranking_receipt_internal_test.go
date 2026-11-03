package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/retrieval"
)

func TestDocumentSearchRerankingIdentityConversion(t *testing.T) {
	t.Parallel()

	report := fromDocumentSearchReport(processing.SearchReport{Receipts: []retrieval.ProviderReceipt{
		{Stage: retrieval.ProviderStageExpansion, Outcome: retrieval.ProviderOutcomeApplied},
		{Stage: retrieval.ProviderStageReranking, Outcome: retrieval.ProviderOutcomeApplied, CandidateCount: 2, ProfileID: "reranker/模型:v2"},
	}}, false)
	require.NotNil(t, report.Reranking)
	encoded, err := json.Marshal(report.Reranking)
	require.NoError(t, err)
	var receipt map[string]any
	require.NoError(t, json.Unmarshal(encoded, &receipt))
	assert.Equal(t, "reranker/模型:v2", receipt["profile_id"])
}
