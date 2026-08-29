package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

func TestSearchModesRequireExplicitBindingWhenProfileIsAmbiguous(t *testing.T) {
	profileFingerprint := strings.Repeat("a", 64)
	vaultID := "123e4567-e89b-42d3-a456-426614174001"
	var searchRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{
				Name: "private", Fingerprint: profileFingerprint,
				EmbeddingBindings: []string{"general", "multilingual"},
			}}))
		case "GET /api/v1/info":
			assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vaultID}))
		case "POST /api/v1/search":
			searchRequests++
			var body api.DocumentSearchRequest
			assert.NoError(t, json.UnmarshalRead(request.Body, &body))
			assert.Equal(t, "hybrid", body.Mode)
			assert.Equal(t, "multilingual", body.BindingID)
			assert.Equal(t, vaultID, body.Fence.VaultUID)
			assert.Equal(t, []string{processingTestVersionID}, body.Fence.ContentVersionIDs)
			assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{
				RequestedMode: "hybrid", ActualMode: "hybrid",
				Coverage: api.DocumentSearchCoverage{BindingRequired: true, ScopedDocuments: 1,
					CompleteDocuments: 1, State: "complete"},
				Results: []api.DocumentSearchResult{{VaultUID: vaultID, NodeID: 42,
					ContentVersionID: processingTestVersionID, Rank: 1, Score: 0.75,
					Path: "/docs/report.pdf", Excerpt: "synthetic match", LexicalRank: 1,
					Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}},
				Trace: []api.DocumentSearchTrace{{Code: "source_fence", Count: 1}},
			}))
		default:
			http.Error(w, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	c := client.New(server.URL, "test-key")

	command, _ := processingTestCommand()
	err := runDocumentSearch(command, c, "synthetic", documentSearchCLIOptions{
		Mode: "hybrid", Profile: "private", ContentVersionIDs: []string{processingTestVersionID}, Limit: 10,
	})
	require.ErrorContains(t, err, "--binding")
	assert.Zero(t, searchRequests)

	command, output := processingTestCommand()
	require.NoError(t, runDocumentSearch(command, c, "synthetic", documentSearchCLIOptions{
		Mode: "hybrid", Profile: "private", BindingID: "multilingual",
		ContentVersionIDs: []string{processingTestVersionID}, Limit: 10, Explain: true, JSON: true,
	}))
	var report api.DocumentSearchReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	assert.Equal(t, "hybrid", report.ActualMode)
	assert.Equal(t, "source_fence", report.Trace[0].Code)
	assert.Equal(t, 1, searchRequests)
}

func TestSearchModesRequireSourceFence(t *testing.T) {
	command, _ := processingTestCommand()
	err := runDocumentSearch(command, client.New("http://127.0.0.1:1", "test-key"), "query",
		documentSearchCLIOptions{Mode: "auto", Profile: "private", Limit: 10})
	require.ErrorContains(t, err, "--source-version")
}
