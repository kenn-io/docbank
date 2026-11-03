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
	for _, mode := range []string{"auto", "lexical", "explicit", "fallback", "emptyscope"} {
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
						versions := []string{processingTestVersionID}
						if mode == "emptyscope" {
							versions = []string{}
						}
						fp, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: vault, ContentVersionIDs: versions})
						assert.NoError(t, err)
						assert.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: vault, ContentVersionIDs: versions}, FenceFingerprint: fp, ObservedScopeCount: len(versions)}))
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
							coverage := api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}
							if mode == "emptyscope" {
								coverage.ScopedDocuments, coverage.CompleteDocuments = 0, 0
							}
							assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: coverage}))
						} else {
							report := api.SearchReport{Limit: 50, SourcePresent: mode == "lexical"}
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
					args = append(args, "--mode", "lexical", "--source-present")
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
					if mode == "lexical" {
						require.Equal(t, map[string]any{"source_present": true}, misses[0].Filters)
					} else {
						require.Empty(t, misses[0].Filters)
					}
					if mode == "explicit" {
						require.Equal(t, 1, resolutions)
					}
					if mode == "emptyscope" {
						require.Zero(t, resolutions)
					}
					if mode == "explicit" || mode == "auto" {
						require.Equal(t, "embed", misses[0].BindingID)
						require.Equal(t, "hybrid", misses[0].RequestedMode)
						require.Equal(t, "hybrid", misses[0].ActualMode)
						require.Equal(t, new(1), misses[0].DocumentCount)
					}
					if mode == "emptyscope" {
						require.Equal(t, new(0), misses[0].DocumentCount)
						require.Empty(t, misses[0].FenceFingerprint)
						require.Equal(t, "hybrid", misses[0].ActualMode)
						require.Equal(t, "complete", misses[0].Coverage["state"])
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

func TestExplicitSearchRejectsTrashedVersionInSourceFence(t *testing.T) {
	const vault = "11111111-1111-4111-8111-111111111111"
	const trashed = "22222222-2222-4222-8222-222222222222"
	searchRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private"}}))
		case "/api/v1/info":
			assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vault}))
		case "/api/v1/processing/source-fences/resolve":
			var request api.DocumentSourceFenceResolveRequest
			assert.NoError(t, json.UnmarshalRead(r.Body, &request))
			assert.Equal(t, []string{processingTestVersionID, trashed}, request.ContentVersionIDs)
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"status":409,"code":"stale_version"}`)
		case "/api/v1/search":
			searchRequests++
			t.Error("search must not start when the source fence contains a trashed version")
			http.NotFound(w, r)
		case "/api/v1/miss-log":
			t.Error("live match must not record a miss")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	command, output := processingTestCommand()
	err := runDocumentSearch(command, daemonconn.New(server.URL, "test-key"), "live", documentSearchCLIOptions{
		Mode: "lexical", Profile: "private", ContentVersionIDs: []string{processingTestVersionID, trashed}, Limit: 10, JSON: true,
	})
	require.Error(t, err)
	assert.Zero(t, searchRequests)
	assert.Empty(t, output.String())
}
