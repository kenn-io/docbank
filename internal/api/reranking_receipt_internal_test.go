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

	for _, outcome := range []retrieval.ProviderOutcome{
		retrieval.ProviderOutcomeApplied, retrieval.ProviderOutcomeSkipped,
		retrieval.ProviderOutcomeAuthorizationDenied, retrieval.ProviderOutcomeTimedOut,
		retrieval.ProviderOutcomeMalformed, retrieval.ProviderOutcomeUnavailable,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			report := fromDocumentSearchReport(processing.SearchReport{Receipts: []retrieval.ProviderReceipt{
				{Stage: retrieval.ProviderStageExpansion, Outcome: retrieval.ProviderOutcomeApplied, ProfileID: "expansion"},
				{Stage: retrieval.ProviderStageReranking, Outcome: outcome, CandidateCount: 2, ProfileID: "reranker/模型:v2"},
			}}, false)
			require.NotNil(t, report.Reranking)
			encoded, err := json.Marshal(report.Reranking)
			require.NoError(t, err)
			var receipt map[string]any
			require.NoError(t, json.Unmarshal(encoded, &receipt))
			if outcome == retrieval.ProviderOutcomeApplied {
				assert.Equal(t, "reranker/模型:v2", receipt["profile_id"])
			} else {
				assert.NotContains(t, receipt, "profile_id")
			}
		})
	}
}
