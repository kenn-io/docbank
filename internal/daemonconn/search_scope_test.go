package daemonconn

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchScopeReportAuthority(t *testing.T) {
	request := api.DocumentSearchRequest{Query: "synthetic", Mode: "lexical", Profile: "private", Fence: api.DocumentSearchFence{VaultUID: "11111111-1111-4111-8111-111111111111"}, Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{MIMEType: "TEXT/PLAIN"}}}
	valid := func() api.DocumentSearchReport {
		return api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical", Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{MIMEType: "text/plain"}}, ObservedScopeCount: new(5000), Coverage: api.DocumentSearchCoverage{State: "unknown"}, Results: []api.DocumentSearchResult{{VaultUID: request.Fence.VaultUID, NodeID: 1, ContentVersionID: "22222222-2222-4222-8222-222222222222", Rank: 1, LexicalRank: 1, Score: 1.0 / 61, Path: "/synthetic.txt", Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}}}
	}
	require.NoError(t, validateDocumentSearchReport(request, valid()))
	for _, tc := range []struct {
		name string
		edit func(*api.DocumentSearchReport)
	}{
		{"missing scope", func(r *api.DocumentSearchReport) { r.Scope = nil }},
		{"kind changed", func(r *api.DocumentSearchReport) { r.Scope.Kind = "invalid" }},
		{"missing count", func(r *api.DocumentSearchReport) { r.ObservedScopeCount = nil }},
		{"negative count", func(r *api.DocumentSearchReport) { r.ObservedScopeCount = new(-1) }},
		{"count smaller than results", func(r *api.DocumentSearchReport) { r.ObservedScopeCount = new(0) }},
		{"missing filters", func(r *api.DocumentSearchReport) { r.Scope.Filters = nil }},
		{"changed filters", func(r *api.DocumentSearchReport) { r.Scope.Filters.MIMEType = "application/pdf" }},
		{"foreign vault", func(r *api.DocumentSearchReport) { r.Results[0].VaultUID = "33333333-3333-4333-8333-333333333333" }},
		{"bad version", func(r *api.DocumentSearchReport) { r.Results[0].ContentVersionID = "invalid" }},
		{"unknown coverage exceeds population", func(r *api.DocumentSearchReport) { r.Coverage.ScopedDocuments = 5001 }},
		{"count differs from coverage", func(r *api.DocumentSearchReport) {
			r.Coverage = api.DocumentSearchCoverage{ScopedDocuments: 4999, CompleteDocuments: 4999, State: "complete"}
		}},
		{"coverage overfull", func(r *api.DocumentSearchReport) {
			r.Coverage = api.DocumentSearchCoverage{ScopedDocuments: 5000, CompleteDocuments: 5001, State: "incomplete"}
		}},
		{"false complete coverage", func(r *api.DocumentSearchReport) {
			r.Coverage = api.DocumentSearchCoverage{ScopedDocuments: 5000, CompleteDocuments: 4999, State: "complete"}
		}},
		{"noncanonical rank", func(r *api.DocumentSearchReport) { r.Results[0].Rank = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := valid()
			tc.edit(&report)
			require.Error(t, validateDocumentSearchReport(request, report))
		})
	}
	request.Scope = nil
	request.Fence.ContentVersionIDs = []string{"22222222-2222-4222-8222-222222222222"}
	require.Error(t, validateDocumentSearchReport(request, valid()), "an explicit-fence request cannot receive scope authority")
}
