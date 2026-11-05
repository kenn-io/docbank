package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestSubmittedSearchMissRecording(t *testing.T) {
	for _, mode := range []string{"lexical", "explicit", "fallback"} {
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

func TestExplicitSearchKeepsLiveResultsWhenAnotherSourceIsTrashed(t *testing.T) {
	root := t.TempDir()
	catalog, err := store.Open(t.Context(), filepath.Join(root, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	live, err := catalog.CreateFile(t.Context(), catalog.RootID(), "live.txt", strings.Repeat("a", 64), 4, "text/plain")
	require.NoError(t, err)
	trashed, err := catalog.CreateFile(t.Context(), catalog.RootID(), "trashed.txt", strings.Repeat("b", 64), 4, "text/plain")
	require.NoError(t, err)
	_, _, err = catalog.Trash(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	name, profile, err := processing.NewSuppliedMediaProfile(catalog, blobs, "test:operator")
	require.NoError(t, err)
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), SpoolDirectory: filepath.Join(root, "spool"), Profiles: map[string]processing.ProfileConfig{name: profile}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, Processing: service, Cfg: cfg, VaultRoot: root})
	t.Cleanup(server.Close)
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	command, output := processingTestCommand()
	require.NoError(t, runDocumentSearch(command, daemonconn.New(endpoint.URL, cfg.Server.APIKey), "live", documentSearchCLIOptions{
		Mode: "lexical", Profile: name, ContentVersionIDs: []string{live.CurrentVersionID, trashed.CurrentVersionID}, Limit: 10, JSON: true,
	}))
	var report api.DocumentSearchReport
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	require.Len(t, report.Results, 1)
	assert.Equal(t, live.CurrentVersionID, report.Results[0].ContentVersionID)
}
