package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/retrieval"
)

func TestSearchValidityHTTP(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	ctx := t.Context()
	old := createFileWithContent(t, ts, catalog, "/old.txt", "synthetic ancient lease\n")
	next := createFileWithContent(t, ts, catalog, "/new.txt", "synthetic replacement lease\n")
	_, err := catalog.PutSupersession(ctx, next.ID, old.ID, old.Revision, new(""))
	require.NoError(t, err)
	_, err = catalog.PutEffectiveWindow(ctx, next.ID, next.Revision, "2025-01-01", "")
	require.NoError(t, err)
	c := daemonconn.New(ts.URL, testAPIKey)
	request := api.DocumentSearchRequest{Query: "old", Mode: "lexical", Profile: "private", AsOf: "2025-06-01", Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{old.CurrentVersionID, next.CurrentVersionID}}}
	report, err := c.SearchDocuments(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "2025-06-01", report.AsOf)
	require.Len(t, report.Results, 1)
	require.Equal(t, "superseded", report.Results[0].Validity.State)
	require.Equal(t, []api.DocumentValiditySuccessor{{NodeID: next.ID, Path: "/new.txt", EffectiveFrom: "2025-01-01"}}, report.Results[0].Validity.SupersededBy)
	request.AsOf = "2024-06-01"
	report, err = c.SearchDocuments(ctx, request)
	require.NoError(t, err)
	require.Equal(t, "current", report.Results[0].Validity.State)
	request.AsOf = "2025-02-30"
	response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
}

func TestSearchValidityReranksReturnedPage(t *testing.T) {
	t.Parallel()
	provider := &routeRerankingProvider{}
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithReranker(t, provider, retrieval.ProviderFailureFailClosed))
	ctx := t.Context()
	old := createFileWithContent(t, ts, catalog, "/renewal-a.txt", "synthetic old lease\n")
	next := createFileWithContent(t, ts, catalog, "/renewal-b.txt", "synthetic new lease\n")
	c := daemonconn.New(ts.URL, testAPIKey)
	request := api.DocumentSearchRequest{Query: "renewal", Mode: "lexical", Profile: "private", Limit: 1, AsOf: "2025-06-01", Fence: api.DocumentSourceFence{VaultUID: catalog.VaultID(), ContentVersionIDs: []string{old.CurrentVersionID, next.CurrentVersionID}}}
	baseline, err := c.SearchDocuments(ctx, request)
	require.NoError(t, err)
	require.Equal(t, old.ID, baseline.Results[0].NodeID)
	_, err = catalog.PutSupersession(ctx, next.ID, old.ID, old.Revision, new(""))
	require.NoError(t, err)
	grantProcessingTestConsent(t, ts, old)
	request.Rerank = true
	report, err := c.SearchDocuments(ctx, request)
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, next.ID, report.Results[0].NodeID)
	require.Len(t, provider.candidates, 1)
	require.Equal(t, next.ID, provider.candidates[0].Document.NodeID)
	require.Equal(t, 1, report.Reranking.CandidateCount)
	request.Limit = 2
	report, err = c.SearchDocuments(ctx, request)
	require.NoError(t, err)
	require.Equal(t, next.ID, report.Results[0].NodeID, "in-force priority survives reranking")
	require.Equal(t, old.ID, report.Results[1].NodeID)
	require.InDelta(t, 2, report.Results[1].Score, 0.000001, "keep the provider score despite demotion")
}
