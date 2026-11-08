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
)

// The generic tool must forward source selection and preserve the returned
// authored identity instead of reconstructing it from a mutable archive path.
func TestPublishedNoteSearchToolPreservesIdentity(t *testing.T) {
	fixture := newReadToolDaemon(t)
	t.Cleanup(fixture.Close)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			fixture.Config.Handler.ServeHTTP(w, r)
			return
		}
		var request api.DocumentSearchRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) || !assert.NotNil(t, request.Scope) || !assert.NotNil(t, request.Scope.Filters.PublishedNotes) {
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "notebook", request.Scope.Filters.PublishedNotes.Publication)
		identity := &api.PublishedNoteIdentity{VaultID: testVaultID, Publication: "notebook", SourceRef: "folder/original.md", NodeID: 7, ContentVersionID: testVersionID, Hash: testAttachmentID, Size: 12, SourceRevision: 2, NamespaceRevision: 3}
		writeDaemonJSON(t, w, api.DocumentSearchReport{RequestedMode: "lexical", ActualMode: "lexical", Coverage: api.DocumentSearchCoverage{State: "unknown"}, Degradations: []string{}, Results: []api.DocumentSearchResult{{VaultUID: testVaultID, NodeID: 7, ContentVersionID: testVersionID, Rank: 1, Score: 1, Path: "/synthetic.md", LexicalRank: 1, PublishedNote: identity, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}}})
	}))
	t.Cleanup(daemon.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	result, err := invokeReadTool(t.Context(), lease, "search_documents", map[string]any{"query": "synthetic", "profile": "local", "mode": "lexical", "limit": 1, "filters": map[string]any{"published_notes": map[string]any{"publication": "notebook"}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	output := structuredMap(t, result.StructuredContent)
	hits, ok := output["results"].([]any)
	require.True(t, ok)
	require.Len(t, hits, 1)
	hit, ok := hits[0].(map[string]any)
	require.True(t, ok)
	identity, ok := hit["published_note"].(map[string]any)
	require.True(t, ok, "published identity must survive tool serialization")
	require.Equal(t, "folder/original.md", identity["source_ref"])
}
