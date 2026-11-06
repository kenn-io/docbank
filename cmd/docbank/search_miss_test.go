package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
)

func TestSubmittedSearchMissRecording(t *testing.T) {
	for _, mode := range []string{"explicit", "lexical", "fuzzy", "fallback"} {
		for _, nameMatch := range []bool{false, true} {
			if mode == "explicit" && nameMatch {
				continue
			}
			t.Run(mode+map[bool]string{true: "/name", false: "/empty"}[nameMatch], func(t *testing.T) {
				const vault = "11111111-1111-4111-8111-111111111111"
				var misses []misslog.Request
				setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/processing/profiles":
						assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"absent", "embed"}, QueryEmbeddingBindings: []string{"absent", "embed"}}}))
					case "/api/v1/vault":
						assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vault}))
					case "/api/v1/search":
						if r.Method == http.MethodPost {
							var request api.DocumentSearchRequest
							assert.NoError(t, json.UnmarshalRead(r.Body, &request))
							if mode == "fallback" {
								w.WriteHeader(http.StatusInternalServerError)
								_, _ = io.WriteString(w, `{"status":500,"code":"processing_failed","detail":"synthetic failure"}`)
								return
							}
							coverage := api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}
							assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: coverage}))
						} else {
							report := api.SearchReport{Limit: 50}
							if mode == "fuzzy" {
								report.Fuzzy = &api.FuzzyReceipt{Status: "complete", Reasons: []string{}}
							}
							if nameMatch {
								report.Hits = []api.SearchHit{{Node: api.Node{ID: 7, Kind: "dir"}, Path: "/match", Match: "name"}}
							}
							assert.NoError(t, json.MarshalWrite(w, report))
						}
					case "/api/v1/search/text-coverage":
						assert.NoError(t, json.MarshalWrite(w, api.SearchTextCoverage{NoText: 1, Families: []string{"pdf"}, Pending: 1}))
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
				if mode == "explicit" {
					args = append(args, "--mode", "hybrid", "--profile", "private", "--binding", "embed", "--source-version", processingTestVersionID)
				} else if mode == "lexical" {
					args = append(args, "--mode", "lexical")
				} else if mode == "fuzzy" {
					args = append(args, "--fuzzy")
				}
				rootCmd.SetArgs(args)
				require.NoError(t, rootCmd.ExecuteContext(context.Background()))
				if nameMatch {
					require.Empty(t, misses)
				} else {
					require.Len(t, misses, 1)
					require.Equal(t, "missing", misses[0].Query)
					require.Empty(t, misses[0].Filters)
					if mode != "explicit" {
						require.Equal(t, map[string]any{"no_text": float64(1), "families": []any{"pdf"}, "pending": float64(1)}, misses[0].Coverage["text"])
					}
					if mode == "fuzzy" {
						require.Equal(t, map[string]any{"status": "complete", "reasons": []any{}}, misses[0].Coverage["fuzzy"])
					}
					if mode == "explicit" {
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
