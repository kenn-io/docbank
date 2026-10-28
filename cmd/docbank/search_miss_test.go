package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
)

func TestSubmittedSearchMissRecording(t *testing.T) {
	for _, mode := range []string{"auto", "lexical", "explicit", "fallback"} {
		for _, nameMatch := range []bool{false, true} {
			if mode == "explicit" && nameMatch {
				continue
			}
			t.Run(mode+map[bool]string{true: "/name", false: "/empty"}[nameMatch], func(t *testing.T) {
				const vault = "11111111-1111-4111-8111-111111111111"
				var misses []misslog.Request
				resolutions := 0
				setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/processing/profiles":
						assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"absent", "embed"}, QueryEmbeddingBindings: []string{"absent", "embed"}}}))
					case "/api/v1/info":
						assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vault}))
					case "/api/v1/processing/source-fences/resolve":
						resolutions++
						fp, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: vault, ContentVersionIDs: []string{processingTestVersionID}})
						assert.NoError(t, err)
						assert.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: vault, ContentVersionIDs: []string{processingTestVersionID}}, FenceFingerprint: fp, ObservedScopeCount: 1}))
					case "/api/v1/search":
						if r.Method == http.MethodPost {
							var request api.DocumentSearchRequest
							assert.NoError(t, json.UnmarshalRead(r.Body, &request))
							if request.BindingID == "absent" && mode == "auto" {
								w.WriteHeader(http.StatusNotFound)
								_, _ = io.WriteString(w, `{"status":404,"code":"not_found"}`)
								return
							}
							if mode == "fallback" {
								w.WriteHeader(http.StatusInternalServerError)
								_, _ = io.WriteString(w, `{"status":500,"code":"processing_failed","detail":"synthetic failure"}`)
								return
							}
							assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}))
						} else {
							report := api.SearchReport{Limit: 50}
							if nameMatch {
								report.Hits = []api.SearchHit{{Node: api.Node{ID: 7, Kind: "dir"}, Path: "/match", Match: "name"}}
							}
							assert.NoError(t, json.MarshalWrite(w, report))
						}
					case "/api/v1/miss-log":
						var receipt misslog.Request
						assert.NoError(t, json.UnmarshalRead(r.Body, &receipt))
						misses = append(misses, receipt)
						w.WriteHeader(http.StatusNoContent)
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				})
				defer func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil); resetFlags(rootCmd) }()
				resetFlags(rootCmd)
				rootCmd.SetOut(io.Discard)
				rootCmd.SetErr(io.Discard)
				args := []string{"search", "missing", "--json"}
				if mode == "lexical" {
					args = append(args, "--mode", "lexical")
				}
				if mode == "explicit" {
					args = append(args, "--mode", "hybrid", "--profile", "private", "--binding", "embed", "--source-version", processingTestVersionID)
				}
				rootCmd.SetArgs(args)
				require.NoError(t, rootCmd.ExecuteContext(context.Background()))
				if nameMatch && mode != "explicit" {
					require.Empty(t, misses)
				} else {
					require.Len(t, misses, 1)
					require.Equal(t, "missing", misses[0].Query)
					require.Empty(t, misses[0].Filters)
					if mode == "explicit" {
						require.Zero(t, resolutions)
					}
					if mode == "explicit" || mode == "auto" {
						require.Equal(t, "embed", misses[0].BindingID)
						require.Equal(t, "hybrid", misses[0].RequestedMode)
						require.Equal(t, "hybrid", misses[0].ActualMode)
						require.Equal(t, new(1), misses[0].DocumentCount)
					}
					if mode == "fallback" {
						require.Equal(t, "private", misses[0].Profile)
						require.Equal(t, "absent", misses[0].BindingID)
						require.Equal(t, "hybrid", misses[0].RequestedMode)
						require.Equal(t, "lexical", misses[0].ActualMode)
						require.Equal(t, []string{"processing_failed"}, misses[0].Degradations)
					}
				}
			})
		}
	}
}

func TestExplicitSearchPreservesLiveMatchWithTrashedVersion(t *testing.T) {
	const vault = "11111111-1111-4111-8111-111111111111"
	const trashed = "22222222-2222-4222-8222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private"}}))
		case "/api/v1/info":
			assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vault}))
		case "/api/v1/processing/source-fences/resolve":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"status":409,"code":"stale_version"}`)
		case "/api/v1/search":
			var request api.DocumentSearchRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, vault, request.Fence.VaultUID)
			assert.Equal(t, []string{processingTestVersionID, trashed}, request.Fence.ContentVersionIDs)
			assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{
				RequestedMode: "lexical", ActualMode: "lexical",
				Coverage: api.DocumentSearchCoverage{ScopedDocuments: 1, CompleteDocuments: 1, State: "complete"},
				Results:  []api.DocumentSearchResult{{VaultUID: vault, NodeID: 42, ContentVersionID: processingTestVersionID, Path: "/live.txt", Rank: 1, LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}},
			}))
		case "/api/v1/miss-log":
			t.Error("live match must not record a miss")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	command, output := processingTestCommand()
	require.NoError(t, runDocumentSearch(command, daemonconn.New(server.URL, "test-key"), "live", documentSearchCLIOptions{
		Mode: "lexical", Profile: "private", ContentVersionIDs: []string{processingTestVersionID, trashed}, Limit: 10, JSON: true,
	}))
	var report api.DocumentSearchReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	require.Len(t, report.Results, 1)
	assert.Equal(t, "/live.txt", report.Results[0].Path)
}
