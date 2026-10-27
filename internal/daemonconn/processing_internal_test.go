package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestDocumentSearchReportValidatesPresentRerankRelevance(t *testing.T) {
	const vaultID = "11111111-1111-4111-8111-111111111111"
	const firstVersion = "22222222-2222-4222-8222-222222222222"
	const secondVersion = "33333333-3333-4333-8333-333333333333"
	for _, test := range []struct {
		name    string
		mutate  func(*api.DocumentSearchRequest, *api.DocumentSearchReport)
		wantErr bool
	}{
		{name: "valid zero"},
		{name: "valid nonzero", mutate: func(_ *api.DocumentSearchRequest, report *api.DocumentSearchReport) {
			report.Results[0].Score = 0.75
			report.Results[0].RerankRelevance = new(0.75)
		}},
		{name: "omitted for older daemon", mutate: func(_ *api.DocumentSearchRequest, report *api.DocumentSearchReport) {
			report.Results[0].RerankRelevance = nil
		}},
		{name: "unrequested", wantErr: true, mutate: func(request *api.DocumentSearchRequest, report *api.DocumentSearchReport) {
			request.Rerank = false
			report.Reranking = nil
		}},
		{name: "degraded", wantErr: true, mutate: func(_ *api.DocumentSearchRequest, report *api.DocumentSearchReport) {
			report.Reranking.Outcome = "degraded"
			report.Reranking.Cause = "authorization_denied"
			report.Degradations = []string{"reranking_degraded"}
		}},
		{name: "outside candidate prefix", wantErr: true, mutate: func(_ *api.DocumentSearchRequest, report *api.DocumentSearchReport) {
			report.Results[1].RerankRelevance = new(report.Results[1].Score)
		}},
		{name: "score mismatch", wantErr: true, mutate: func(_ *api.DocumentSearchRequest, report *api.DocumentSearchReport) {
			report.Results[0].RerankRelevance = new(0.75)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := api.DocumentSearchRequest{Query: "synthetic", Mode: "lexical", Rerank: true,
				Fence: api.DocumentSourceFence{VaultUID: vaultID, ContentVersionIDs: []string{firstVersion, secondVersion}}}
			report := api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical",
				Coverage:  api.DocumentSearchCoverage{ScopedDocuments: 2, CompleteDocuments: 2, State: "complete"},
				Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", CandidateCount: 1},
				Results: []api.DocumentSearchResult{
					{VaultUID: vaultID, NodeID: 1, ContentVersionID: firstVersion, Rank: 1, LexicalRank: 1,
						Score: 0, RerankRelevance: new(0.0), Path: "/synthetic-a.txt", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}},
					{VaultUID: vaultID, NodeID: 2, ContentVersionID: secondVersion, Rank: 2, LexicalRank: 2,
						Score: 0.25, Path: "/synthetic-b.txt", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}},
				}}
			if test.mutate != nil {
				test.mutate(&request, &report)
			}
			err := validateDocumentSearchReport(request, report)
			if test.wantErr {
				require.ErrorContains(t, err, "rerank relevance")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
