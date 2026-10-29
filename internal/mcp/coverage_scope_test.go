package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestProcessingCoverageScopeInputSchema(t *testing.T) {
	input, _ := getProcessingCoverageSchemas()
	require.NotContains(t, input, "oneOf")
	for _, selection := range []map[string]any{
		{"content_version_ids": []string{testVersionID}},
		{"all_live": true},
		{"all_live": true, "filters": map[string]any{"mime_type": "text/plain", "under_node_id": 7, "tag_id": testVersionID, "modified_since": "2026-01-01T00:00:00Z", "modified_before": "2026-02-01T00:00:00Z"}},
	} {
		selection["profile"] = "local"
		selection["vault_id"] = testVaultID
		assertSchemaAccepts(t, input, selection)
	}
	for _, selection := range []map[string]any{
		{}, {"all_live": false}, {"all_live": true, "content_version_ids": []string{testVersionID}},
		{"all_live": false, "content_version_ids": []string{testVersionID}},
		{"content_version_ids": []string{testVersionID}, "filters": map[string]any{}},
		{"filters": map[string]any{}}, {"all_live": true, "filters": map[string]any{"unexpected": 1}},
		{"all_live": true, "filters": map[string]any{"under_node_id": 0}},
		{"all_live": true, "content_version_ids": []string{}},
	} {
		selection["profile"] = "local"
		selection["vault_id"] = testVaultID
		assertSchemaRejects(t, input, selection)
	}
	ids := make([]string, 4097)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
	}
	assertSchemaRejects(t, input, map[string]any{"profile": "local", "vault_id": testVaultID, "content_version_ids": ids})
}

func TestProcessingCoverageScopeToolAdmitsLargeCounts(t *testing.T) {
	filters := &api.DocumentSourceFenceFilters{MIMEType: "text/plain", TagID: testVersionID, UnderNodeID: 7, ModifiedSince: "2026-01-01T00:00:00Z", ModifiedBefore: "2026-02-01T00:00:00Z"}
	count := 35007
	report := api.CoverageReport{VaultUID: testVaultID, ProfileFingerprint: strings.Repeat("c", 64), State: "partial",
		Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: filters}, ObservedScopeCount: &count,
		Renditions: api.CoverageClass{Name: "rendition", Required: true, State: "partial", Complete: 5001, Unavailable: 5001, Stale: 5001, Ineligible: 5001, Rebuilding: 5001, PreviousGenerationServing: 5001, Total: 35007}, Embeddings: []api.CoverageClass{}}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v1/coverage", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		var body api.DocumentCoverageRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
			return
		}
		assert.Equal(t, "local", body.Profile)
		assert.Equal(t, testVaultID, body.Fence.VaultUID)
		assert.Empty(t, body.Fence.ContentVersionIDs)
		requestFilters := *filters
		requestFilters.MIMEType = "TEXT/PLAIN"
		assert.Equal(t, &api.DocumentSearchScope{Kind: "all_live", Filters: &requestFilters}, body.Scope)
		writeDaemonJSON(t, w, report)
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "get_processing_coverage", "arguments": map[string]any{"profile": "local", "vault_id": testVaultID, "all_live": true, "filters": map[string]any{"mime_type": "TEXT/PLAIN", "tag_id": testVersionID, "under_node_id": 7, "modified_since": "2026-01-01T00:00:00Z", "modified_before": "2026-02-01T00:00:00Z"}},
	})))
	require.NotEqual(t, true, result["isError"])
	output := objectField(t, result, "structuredContent")
	assert.NotContains(t, output, "content_version_ids")
	assert.EqualValues(t, 35007, output["observed_scope_count"])
	assert.Equal(t, map[string]any{"kind": "all_live", "filters": map[string]any{"mime_type": "text/plain", "tag_id": testVersionID, "under_node_id": float64(7), "modified_since": "2026-01-01T00:00:00Z", "modified_before": "2026-02-01T00:00:00Z"}}, output["scope"])
	classes, ok := output["coverage"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, classes)
	class, ok := classes[0].(map[string]any)
	require.True(t, ok)
	for _, key := range []string{"complete", "unavailable", "stale", "ineligible", "rebuilding", "previous_generation_serving"} {
		assert.EqualValues(t, 5001, class[key])
	}
	_, schema := getProcessingCoverageSchemas()
	class["complete"] = -1
	assertSchemaRejects(t, schema, output)
}

func TestProcessingCoverageScopeToolRejectsInvalidFilters(t *testing.T) {
	for _, value := range []string{"not-a-date", "2026-02-30T00:00:00Z", "2026-08-28T00:00:00"} {
		t.Run(value, func(t *testing.T) {
			wireErr := decodeWireError(t, callToolWire(t, "get_processing_coverage", map[string]any{
				"profile": "local", "vault_id": testVaultID, "all_live": true, "filters": map[string]any{"modified_since": value},
			}))
			assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
		})
	}
}

func TestProcessingCoverageExactToolKeepsGETAndOutput(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/v1/coverage", r.URL.Path)
		assert.Equal(t, "local", r.URL.Query().Get("profile"))
		assert.Equal(t, testVaultID, r.URL.Query().Get("vault_uid"))
		assert.Equal(t, []string{testVersionID}, r.URL.Query()["content_version_id"])
		writeDaemonJSON(t, w, api.CoverageReport{VaultUID: testVaultID, ProfileFingerprint: testProfileID, State: "complete",
			Renditions: api.CoverageClass{Name: "rendition", Required: true, State: "complete", Total: 1, Complete: 1}, Embeddings: []api.CoverageClass{}})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "get_processing_coverage", "arguments": map[string]any{"profile": "local", "vault_id": testVaultID, "content_version_ids": []string{testVersionID}},
	})))
	require.NotEqual(t, true, result["isError"])
	output := objectField(t, result, "structuredContent")
	assert.Equal(t, []any{testVersionID}, output["content_version_ids"])
	assert.NotContains(t, output, "scope")
	assert.NotContains(t, output, "observed_scope_count")
}
