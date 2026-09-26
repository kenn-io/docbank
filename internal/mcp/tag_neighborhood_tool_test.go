package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const testTagID = "33333333-3333-4333-8333-333333333333"

func TestTagNeighborhoodToolRequiresExactFenceAndOneSupportedSeed(t *testing.T) {
	tool := catalogMap(toolCatalog(false, false))["get_tag_neighborhood"]
	require.NotNil(t, tool, "agents need a discoverable read tool for existing tag neighborhoods")
	valid := map[string]any{
		"vault_id": testVaultID, "content_version_ids": []any{testVersionID},
		"seed_tag_id": testTagID, "limit": 20,
	}
	assertSchemaAccepts(t, tool.InputSchema, valid)
	for name, input := range map[string]map[string]any{
		"missing fence": {"seed_tag_id": testTagID},
		"empty fence":   {"vault_id": testVaultID, "content_version_ids": []any{}, "seed_tag_id": testTagID},
		"missing seed":  {"vault_id": testVaultID, "content_version_ids": []any{testVersionID}},
		"two seeds": {"vault_id": testVaultID, "content_version_ids": []any{testVersionID},
			"seed_tag_id": testTagID, "seed_node_id": 7},
		"unsupported seed": {"vault_id": testVaultID, "content_version_ids": []any{testVersionID},
			"passage_id": "passage-1"},
		"oversized limit": {"vault_id": testVaultID, "content_version_ids": []any{testVersionID},
			"seed_tag_id": testTagID, "limit": 101},
	} {
		t.Run(name, func(t *testing.T) { assertSchemaRejects(t, tool.InputSchema, input) })
	}
}

func TestTagNeighborhoodToolReturnsScopedTopicalPathsWithPrivateCache(t *testing.T) {
	require.NotNil(t, catalogMap(toolCatalog(false, false))["get_tag_neighborhood"])
	weight := store.TagRarityWeight(1, 1)
	path := []api.TagGraphPathStep{{Kind: store.TagGraphNodeTag, TagID: testTagID},
		{Kind: store.TagGraphNodeDocument, NodeID: 7}}
	daemon := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/api/v1/tags/neighborhood", request.URL.Path)
		assert.Equal(t, http.MethodPost, request.Method)
		var input api.TagNeighborhoodRequest
		if !assert.NoError(t, json.UnmarshalRead(request.Body, &input)) {
			return
		}
		assert.Equal(t, testVaultID, input.Fence.VaultUID)
		assert.Equal(t, []string{testVersionID}, input.Fence.ContentVersionIDs)
		assert.Equal(t, testTagID, input.Seed.TagID)
		assert.Equal(t, []string{store.TagAssignmentDocument}, input.AssignmentKinds)
		writeDaemonJSON(t, response, api.TagNeighborhoodResponse{
			VaultUID: testVaultID, DocumentCount: 1, VisitedNodes: 2,
			Documents: []api.TagGraphDocument{{NodeID: 7, ContentVersionID: testVersionID,
				Name: "synthetic.md", Path: "/synthetic.md", ModifiedAt: "2026-09-24T00:00:00Z",
				Score: weight, GraphPath: path,
				SharedTags: []api.TagGraphTag{{ID: testTagID, Name: "topic", ScopedDocumentCount: 1,
					Weight: weight, AssignmentOrigin: store.TagAssignmentOriginLegacy,
					Path: []api.TagGraphPathStep{}}}}},
			Tags: []api.TagGraphTag{},
		})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "get_tag_neighborhood", map[string]any{
		"vault_id": testVaultID, "content_version_ids": []string{testVersionID}, "seed_tag_id": testTagID,
	})
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := structuredMap(t, result.StructuredContent)
	assert.Equal(t, testVaultID, output["vault_uid"])
	assert.EqualValues(t, 1, output["document_count"])
	assert.Equal(t, "private", output["cacheScope"])
	assert.EqualValues(t, 0, output["ttlMs"])
	documents, ok := output["documents"].([]any)
	require.True(t, ok)
	require.Len(t, documents, 1)
	document, ok := documents[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, testVersionID, document["content_version_id"])
	assert.Equal(t, "/synthetic.md", document["path"])
	assert.InDelta(t, weight, document["score"], 1e-12)
	graphPath, ok := document["graph_path"].([]any)
	require.True(t, ok)
	require.Len(t, graphPath, 2)
	first, ok := graphPath[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "tag", first["kind"])
	assert.Equal(t, testTagID, first["tag_id"])
	second, ok := graphPath[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "document", second["kind"])
	assert.EqualValues(t, 7, second["node_id"])
	assert.NotEmpty(t, document["shared_tags"])
}
