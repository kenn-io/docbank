package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestSearchSimilarUsesDaemonSelectorAnd4096Fence(t *testing.T) {
	vault := "22222222-2222-4222-8222-222222222222"
	versions := []string{processingTestVersionID}
	for len(versions) < 4096 {
		versions = append(versions, uuid.New().String())
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes/42":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Kind: "file", CurrentVersionID: processingTestVersionID}))
		case "/api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"semantic"}}}))
		case "/api/v1/info":
			assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vault}))
		case "/api/v1/search/similar":
			calls++
			var request api.DocumentSimilarRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, versions, request.Fence.ContentVersionIDs)
			assert.Equal(t, int64(42), request.Selector.NodeID)
			assert.Equal(t, processingTestVersionID, request.Selector.ContentVersionID)
			assert.NoError(t, json.MarshalWrite(w, api.DocumentSimilarReport{State: "unavailable", Source: api.DocumentSimilarSource{NodeID: 42, ContentVersionID: processingTestVersionID}, BindingID: "semantic",
				MissingCoverage: &api.DocumentMissingCoverage{Kind: "embedding", BindingID: "semantic", ProfileFingerprint: strings.Repeat("a", 64), ContentVersionID: processingTestVersionID}, Coverage: api.DocumentSearchCoverage{State: "unknown"}}))
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	selector, err := parseNodeSelector("id:42")
	require.NoError(t, err)
	command, output := processingTestCommand()
	options := documentSearchCLIOptions{Profile: "private", ContentVersionIDs: versions, Limit: 20}
	require.NoError(t, runSimilarSearch(command, daemonconn.New(server.URL, "key"), selector, "", options))
	assert.Contains(t, output.String(), "unavailable: no current embedding")
	assert.Equal(t, 1, calls)
	options.JSON = true
	output.Reset()
	require.NoError(t, runSimilarSearch(command, daemonconn.New(server.URL, "key"), selector, processingTestVersionID, options))
	assert.Contains(t, output.String(), `"state":"unavailable"`)
	options.ContentVersionIDs = versions[1:]
	require.ErrorContains(t, runSimilarSearch(command, daemonconn.New(server.URL, "key"), selector, "", options), "selected version")
}
