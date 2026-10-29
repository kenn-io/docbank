package api_test

import (
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/retrieval"
)

func TestDocumentSearchEmptyScopeValidation(t *testing.T) {
	t.Parallel()
	provider := &routeRerankingProvider{}
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithReranker(t, provider, retrieval.ProviderFailureDegrade))
	request := api.DocumentSearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", Rerank: true, Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()}, Scope: &api.DocumentSearchScope{Kind: "all_live"}}
	report, err := daemonconn.New(ts.URL, testAPIKey).SearchDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.NotNil(t, report.ObservedScopeCount)
	require.Zero(t, *report.ObservedScopeCount)
	require.NotNil(t, report.Reranking)
	require.Equal(t, "skipped", report.Reranking.Outcome)
	require.Zero(t, provider.calls)
	neither := request
	neither.Scope = nil
	response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, neither)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	var failure api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &failure))
	require.Equal(t, "validation", failure.Code)
	for _, tc := range []struct {
		name string
		edit func(*api.DocumentSearchRequest)
	}{
		{"query", func(r *api.DocumentSearchRequest) { r.Query = " " }},
		{"mode", func(r *api.DocumentSearchRequest) { r.Mode = "unknown" }},
		{"profile", func(r *api.DocumentSearchRequest) { r.Profile = "missing" }},
		{"foreign vault", func(r *api.DocumentSearchRequest) { r.Fence.VaultUID = "11111111-1111-4111-8111-111111111111" }},
		{"both populations", func(r *api.DocumentSearchRequest) {
			r.Fence.ContentVersionIDs = []string{"11111111-1111-4111-8111-111111111111"}
		}},
		{"media scope", func(r *api.DocumentSearchRequest) {
			r.MediaSources = []api.DocumentMediaSourceSelector{{SourceID: "source", SourceVersionID: "version", ContentVersionID: "11111111-1111-4111-8111-111111111111"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := request
			tc.edit(&invalid)
			response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, invalid)
			require.GreaterOrEqual(t, response.StatusCode, 400, body)
		})
	}
}

func TestDocumentSearchScopeFiltersErrors(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	file, err := catalog.CreateFile(t.Context(), catalog.RootID(), "synthetic-scope.txt", testHash("scope-directory"), 1, "text/plain")
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		filters api.DocumentSourceFenceFilters
		code    string
	}{
		{"mime", api.DocumentSourceFenceFilters{MIMEType: "not a mime"}, "validation"},
		{"time", api.DocumentSourceFenceFilters{ModifiedSince: "invalid"}, "validation"},
		{"reversed time", api.DocumentSourceFenceFilters{ModifiedSince: "2026-10-02T00:00:00Z", ModifiedBefore: "2026-10-01T00:00:00Z"}, "validation"},
		{"negative directory", api.DocumentSourceFenceFilters{UnderNodeID: -1}, "validation"},
		{"missing directory", api.DocumentSourceFenceFilters{UnderNodeID: 9223372036854775807}, "not_found"},
		{"file as directory", api.DocumentSourceFenceFilters{UnderNodeID: file.ID}, "not_dir"},
		{"missing tag", api.DocumentSourceFenceFilters{TagID: "11111111-1111-4111-8111-111111111111"}, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, api.DocumentSearchRequest{Query: "synthetic", Mode: "lexical", Profile: "private", Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()}, Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &tc.filters}})
			require.GreaterOrEqual(t, response.StatusCode, 400, body)
			var failure api.Error
			require.NoError(t, json.Unmarshal([]byte(body), &failure))
			require.Equal(t, tc.code, failure.Code, body)
		})
	}
}

func TestDocumentSearchScopeBeyondExplicitWireLimit(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	seed, err := catalog.CreateFile(t.Context(), catalog.RootID(), "synthetic-seed.txt", testHash("synthetic-large-scope"), 1, "text/plain")
	require.NoError(t, err)
	db, err := store.DefaultSQLiteDriver().Open(catalog.DBPath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ids := make([]string, 4097)
	for i := range ids {
		ids[i] = uuid.New().String()
		result, err := tx.ExecContext(t.Context(), `INSERT INTO nodes(parent_id,name,kind,current_version_id,revision,created_at,modified_at) SELECT parent_id,?,'file',?,1,created_at,modified_at FROM nodes WHERE id=?`, fmt.Sprintf("synthetic-%04d.txt", i), ids[i], seed.ID)
		require.NoError(t, err)
		nodeID, err := result.LastInsertId()
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind,source_version_id) SELECT ?,?,blob_hash,size,mime_type,recorded_at,1,?,'content_create',NULL FROM content_versions WHERE version_id=?`, ids[i], nodeID, uuid.New().String(), seed.CurrentVersionID)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	request := api.DocumentSearchRequest{Query: "synthetic", Mode: "lexical", Profile: "private", Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()}, Scope: &api.DocumentSearchScope{Kind: "all_live"}}
	report, err := daemonconn.New(ts.URL, testAPIKey).SearchDocuments(t.Context(), request)
	require.NoError(t, err)
	require.NotEmpty(t, report.Results)
	require.Equal(t, 4098, *report.ObservedScopeCount)
	request.Scope = nil
	request.Fence.ContentVersionIDs = ids
	response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	var failure api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &failure))
	require.Equal(t, "validation", failure.Code)
}

func TestDocumentSearchEmptyScopeWithEmbeddingBinding(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))
	request := api.DocumentSearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()}, Scope: &api.DocumentSearchScope{Kind: "all_live"}}
	// No index or query-text consent exists. Empty populations need neither.
	report, err := daemonconn.New(ts.URL, testAPIKey).SearchDocuments(t.Context(), request)
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.Equal(t, 0, *report.ObservedScopeCount)
	request.BindingID = "missing"
	response, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestDocumentSearchEmptyScopePreservesRequiredBinding(t *testing.T) {
	t.Parallel()
	rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithProviders(t, rendition, newProcessingTestEmbeddingProvider(t), true, document.EmbeddingRequired))
	report, err := daemonconn.New(ts.URL, testAPIKey).SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "synthetic", Mode: "semantic", Profile: "private", Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()}, Scope: &api.DocumentSearchScope{Kind: "all_live"}})
	require.NoError(t, err)
	require.Empty(t, report.Results)
	require.True(t, report.Coverage.BindingRequired)
	require.Equal(t, "complete", report.Coverage.State)
}
