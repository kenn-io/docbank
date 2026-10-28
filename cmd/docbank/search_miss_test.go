package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
)

func TestSubmittedSearchMissRecording(t *testing.T) {
	for _, mode := range []string{"auto", "lexical", "explicit"} {
		for _, nameMatch := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "/name", false: "/empty"}[nameMatch], func(t *testing.T) {
				const vault = "11111111-1111-4111-8111-111111111111"
				var misses []misslog.Request
				setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/processing/profiles":
						require.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"embed"}, QueryEmbeddingBindings: []string{"embed"}}}))
					case "/api/v1/info":
						require.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: vault}))
					case "/api/v1/processing/source-fences/resolve":
						fp, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: vault, ContentVersionIDs: []string{processingTestVersionID}})
						require.NoError(t, err)
						require.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: vault, ContentVersionIDs: []string{processingTestVersionID}}, FenceFingerprint: fp, ObservedScopeCount: 1}))
					case "/api/v1/search":
						if r.Method == http.MethodPost {
							require.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}))
						} else {
							report := api.SearchReport{Limit: 50}
							if nameMatch {
								report.Hits = []api.SearchHit{{Node: api.Node{ID: 7, Kind: "dir"}, Path: "/match", Match: "name"}}
							}
							require.NoError(t, json.MarshalWrite(w, report))
						}
					case "/api/v1/miss-log":
						var receipt misslog.Request
						require.NoError(t, json.UnmarshalRead(r.Body, &receipt))
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
					args = append(args, "--mode", "hybrid", "--profile", "private", "--source-version", processingTestVersionID)
				}
				rootCmd.SetArgs(args)
				require.NoError(t, rootCmd.ExecuteContext(context.Background()))
				if nameMatch && mode != "explicit" {
					require.Empty(t, misses)
				} else {
					require.Len(t, misses, 1)
					require.Equal(t, "missing", misses[0].Query)
				}
			})
		}
	}
}
