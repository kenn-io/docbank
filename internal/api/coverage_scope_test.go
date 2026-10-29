package api_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

// Removing the scoped route, dropping its scope conversion, or changing the
// legacy report counts must break these HTTP-level compatibility checks.
func TestProcessingCoverageScopeMatchesExplicit(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))
	complete := createFileWithContent(t, ts, catalog, "/complete.txt", "synthetic complete coverage\n")
	runProcessingForCoverage(t, ts, complete)
	pending := createFileWithContent(t, ts, catalog, "/pending.txt", "synthetic pending coverage\n")
	trashed := createFileWithContent(t, ts, catalog, "/trashed.txt", "synthetic excluded coverage\n")
	_, _, err := catalog.Trash(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	response, legacyBody := get(t, ts, "/api/v1/coverage?profile=private&vault_uid="+catalog.VaultID()+"&content_version_id="+complete.CurrentVersionID+"&content_version_id="+pending.CurrentVersionID, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, legacyBody)
	var explicit api.CoverageReport
	require.NoError(t, json.Unmarshal([]byte(legacyBody), &explicit))
	require.NotContains(t, legacyBody, `"scope"`)
	require.NotContains(t, legacyBody, `"observed_scope_count"`)
	response, body := do(t, ts, http.MethodPost, "/api/v1/coverage", nil, api.DocumentCoverageRequest{
		Profile: "private", Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()},
		Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{MIMEType: "TEXT/PLAIN"}},
	})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var scoped api.CoverageReport
	require.NoError(t, json.Unmarshal([]byte(body), &scoped))
	require.Equal(t, &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{MIMEType: "text/plain"}}, scoped.Scope)
	require.NotNil(t, scoped.ObservedScopeCount)
	require.Equal(t, 2, *scoped.ObservedScopeCount)
	classes := scoped
	classes.Scope, classes.ObservedScopeCount = nil, nil
	require.Equal(t, explicit, classes)
	require.Equal(t, 1, scoped.Renditions.Complete)
	require.Equal(t, 1, scoped.Renditions.Unavailable)
	require.Zero(t, scoped.Renditions.Stale)
	require.NotContains(t, body, "content_version_ids")
	// POST retains the explicit population alternative with the same response.
	response, body = do(t, ts, http.MethodPost, "/api/v1/coverage", nil, map[string]any{
		"profile": "private", "fence": map[string]any{"vault_uid": catalog.VaultID(), "content_version_ids": []string{complete.CurrentVersionID, pending.CurrentVersionID}},
	})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var posted api.CoverageReport
	require.NoError(t, json.Unmarshal([]byte(body), &posted))
	require.Equal(t, explicit, posted)
	require.NotContains(t, body, `"observed_scope_count"`)
}

func TestProcessingCoverageEmptyScope(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestServiceWithEmbedding(t))

	for _, filters := range []*api.DocumentSourceFenceFilters{nil, {MIMEType: "IMAGE/PNG"}} {
		if filters != nil {
			// The filtered case also proves that a nonempty vault can select no files.
			createFileWithContent(t, ts, catalog, "/unmatched.txt", "synthetic unmatched coverage\n")
		}
		response, body := do(t, ts, http.MethodPost, "/api/v1/coverage", nil, api.DocumentCoverageRequest{
			Profile: "private", Fence: api.DocumentSearchFence{VaultUID: catalog.VaultID()},
			Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: filters},
		})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var report api.CoverageReport
		require.NoError(t, json.Unmarshal([]byte(body), &report))
		require.NotNil(t, report.Scope)
		require.NotNil(t, report.ObservedScopeCount)
		require.Zero(t, *report.ObservedScopeCount)
		require.Equal(t, "complete", report.State)
		require.Zero(t, report.Renditions.Total)
		for _, embedding := range report.Embeddings {
			require.Zero(t, embedding.Total)
		}
	}
}

func TestProcessingCoverageScopeValidation(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureProcessingTestService(t))
	file := createFileWithContent(t, ts, catalog, "/synthetic-filter.txt", "synthetic filters\n")
	for _, tc := range []struct {
		name   string
		scope  any
		ids    []string
		vault  string
		status int
		code   string
	}{
		{name: "neither", status: 422, code: "validation"},
		{name: "both", scope: map[string]any{"kind": "all_live"}, ids: []string{file.CurrentVersionID}, status: 422, code: "validation"},
		{name: "unknown scope", scope: map[string]any{"kind": "unknown"}, status: 422, code: "validation"},
		{name: "foreign vault", scope: map[string]any{"kind": "all_live"}, vault: "11111111-1111-4111-8111-111111111111", status: 422, code: "foreign_vault"},
		{name: "mime", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"mime_type": "malformed mime"}}, status: 422, code: "validation"},
		{name: "time", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"modified_since": "invalid"}}, status: 422, code: "validation"},
		{name: "reversed time", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"modified_since": "2026-10-02T00:00:00Z", "modified_before": "2026-10-01T00:00:00Z"}}, status: 422, code: "validation"},
		{name: "negative directory", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"under_node_id": -1}}, status: 422, code: "validation"},
		{name: "missing directory", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"under_node_id": 9223372036854775807}}, status: 404, code: "not_found"},
		{name: "file as directory", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"under_node_id": file.ID}}, status: 422, code: "not_dir"},
		{name: "missing tag", scope: map[string]any{"kind": "all_live", "filters": map[string]any{"tag_id": "11111111-1111-4111-8111-111111111111"}}, status: 404, code: "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := tc.vault
			if vault == "" {
				vault = catalog.VaultID()
			}
			response, body := do(t, ts, http.MethodPost, "/api/v1/coverage", nil, map[string]any{
				"profile": "private", "fence": map[string]any{"vault_uid": vault, "content_version_ids": tc.ids}, "scope": tc.scope,
			})
			require.Equal(t, tc.status, response.StatusCode, body)
			var failure api.Error
			require.NoError(t, json.Unmarshal([]byte(body), &failure))
			require.Equal(t, tc.code, failure.Code)
		})
	}
}
