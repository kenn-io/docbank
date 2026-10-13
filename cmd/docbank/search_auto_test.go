package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestAutomaticSearchSelectionAndFallback(t *testing.T) {
	const vault = "22222222-2222-4222-8222-222222222222"
	profiles := []api.ProcessingProfileSummary{
		{Name: "zeta", EmbeddingBindings: []string{"z"}, QueryEmbeddingBindings: []string{"z"}},
		{Name: "alpha", EmbeddingBindings: []string{"b", "a"}, QueryEmbeddingBindings: []string{"b", "a"}},
	}
	cases := []struct {
		name                                             string
		query                                            string
		profiles                                         []api.ProcessingProfileSummary
		profileError, scopeError, searchError            string
		empty, malformed, cold, jsonCLI, humanCLI, mixed bool
		limit                                            int
		opts                                             daemonconn.SearchOptions
		wantReason                                       string
		wantError                                        bool
		wantScope, wantSearch                            int
	}{
		{name: "cold first binding", profiles: profiles, limit: 50, cold: true, wantScope: 1, wantSearch: 2},
		{name: "JSON hybrid", profiles: profiles, limit: 50, jsonCLI: true, wantScope: 1, wantSearch: 1},
		{name: "query length boundary", query: strings.Repeat("界", 8192), profiles: profiles, limit: 50, jsonCLI: true, wantScope: 1, wantSearch: 1},
		{name: "query too long", query: strings.Repeat("界", 8193), profiles: profiles, limit: 50, jsonCLI: true, wantReason: "query_requires_lexical"},
		{name: "JSON fallback", profiles: profiles, limit: 50, jsonCLI: true, searchError: "processing_failed", wantScope: 1, wantSearch: 3, wantReason: "processing_failed"},
		{name: "mixed fallback reasons", profiles: profiles, limit: 50, mixed: true, searchError: "processing_failed", wantScope: 1, wantSearch: 3, wantReason: "processing_consent_required"},
		{name: "human fallback", profiles: profiles, limit: 50, humanCLI: true, searchError: "processing_consent_required", wantScope: 1, wantSearch: 3, wantReason: "processing_consent_required"},
		{name: "no profiles", limit: 50},
		{name: "rendition only", profiles: []api.ProcessingProfileSummary{{Name: "text", Rendition: true}}, limit: 50},
		{name: "processing disabled", profileError: "processing_unavailable", limit: 50},
		{name: "no query encoder", profiles: []api.ProcessingProfileSummary{{Name: "text", EmbeddingBindings: []string{"doc"}}}, limit: 50, wantReason: "query_embedding_unavailable"},
		{name: "tag", profiles: profiles, limit: 50, opts: daemonconn.SearchOptions{TagID: processingTestVersionID}, wantReason: "filters_require_lexical"},
		{name: "mime", profiles: profiles, limit: 50, opts: daemonconn.SearchOptions{MIMEType: "text/plain"}, wantReason: "filters_require_lexical"},
		{name: "directory", profiles: profiles, limit: 50, opts: daemonconn.SearchOptions{UnderNodeID: 42}, wantReason: "filters_require_lexical"},
		{name: "since", profiles: profiles, limit: 50, opts: daemonconn.SearchOptions{ModifiedSince: "2026-01-01T00:00:00Z"}, wantReason: "filters_require_lexical"},
		{name: "before", profiles: profiles, limit: 50, opts: daemonconn.SearchOptions{ModifiedBefore: "2026-01-01T00:00:00Z"}, wantReason: "filters_require_lexical"},
		{name: "large limit", profiles: profiles, limit: 1000, wantReason: "limit_requires_lexical"},
		{name: "empty scope", profiles: profiles, limit: 50, empty: true, wantScope: 1, wantReason: "empty_source_scope"},
		{name: "large scope", profiles: profiles, limit: 50, scopeError: "scope_too_large", wantScope: 1, wantReason: "scope_too_large"},
		{name: "source changed after resolution", profiles: profiles, limit: 50, searchError: "stale_version", wantScope: 1, wantSearch: 3, wantReason: "stale_version"},
		{name: "auth discovery", profileError: "unauthorized", limit: 50, wantError: true},
		{name: "auth search", profiles: profiles, limit: 50, searchError: "unauthorized", wantScope: 1, wantSearch: 1, wantError: true},
		{name: "unknown search error", profiles: profiles, limit: 50, searchError: "foreign_vault", wantScope: 1, wantSearch: 1, wantError: true},
		{name: "malformed success", profiles: profiles, limit: 50, malformed: true, wantScope: 1, wantSearch: 1, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := tc.query
			if query == "" {
				query = "query words"
			}
			var scopeCalls, searchCalls int
			var record kitdaemon.RuntimeRecord
			problem := func(w http.ResponseWriter, code string) {
				status := http.StatusInternalServerError
				observed := 0
				switch code {
				case "unauthorized":
					status = http.StatusUnauthorized
				case "processing_unavailable":
					status = http.StatusServiceUnavailable
				case "processing_consent_required":
					status = http.StatusPreconditionRequired
				case "stale_version":
					status = http.StatusConflict
				case "foreign_vault":
					status = http.StatusUnprocessableEntity
				case "scope_too_large":
					status = http.StatusUnprocessableEntity
					observed = 4097
				}
				w.WriteHeader(status)
				assert.NoError(t, json.MarshalWrite(w, api.Error{Status: status, Code: code, Detail: "provider details must stay private", ObservedScopeCount: observed}))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /api/ping":
					assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: record.Service, Version: record.Version, PID: record.PID}))
				case "GET /api/daemon/challenge":
					nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
					assert.NoError(t, err)
					assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof("synthetic-token", nonce)}))
				case "GET /api/v1/search":
					assert.True(t, tc.jsonCLI || tc.humanCLI)
					assert.Equal(t, query, r.URL.Query().Get("q"))
					assert.NoError(t, json.MarshalWrite(w, api.SearchReport{Limit: tc.limit, Hits: []api.SearchHit{{Node: api.Node{ID: 42, Kind: "file", Name: "query.txt"}, Path: "/query.txt", Match: "name"}}}))
				case "GET /api/v1/processing/profiles":
					if tc.profileError != "" {
						problem(w, tc.profileError)
						return
					}
					listed := append([]api.ProcessingProfileSummary{}, tc.profiles...)
					for i := range listed {
						listed[i].Fingerprint = strings.Repeat("a", 64)
					}
					assert.NoError(t, json.MarshalWrite(w, listed))
				case "POST /api/v1/processing/source-fences/resolve":
					scopeCalls++
					if tc.scopeError != "" {
						problem(w, tc.scopeError)
						return
					}
					var body api.DocumentSourceFenceResolveRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &body))
					if !assert.NotNil(t, body.Filters) {
						return
					}
					assert.Equal(t, api.DocumentSourceFenceFilters{}, *body.Filters)
					assert.Empty(t, body.ContentVersionIDs)
					ids := []string{processingTestVersionID}
					if tc.empty {
						ids = []string{}
					}
					fence := processing.SourceFence{VaultUID: vault, ContentVersionIDs: ids}
					fp, err := processing.SourceFenceFingerprint(fence)
					assert.NoError(t, err)
					assert.NoError(t, json.MarshalWrite(w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: vault, ContentVersionIDs: ids}, FenceFingerprint: fp, ObservedScopeCount: len(ids)}))
				case "POST /api/v1/search":
					searchCalls++
					var body api.DocumentSearchRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &body))
					assert.Equal(t, query, body.Query)
					assert.Equal(t, "hybrid", body.Mode)
					expectedProfile, expectedBinding := "alpha", "a"
					if searchCalls == 2 {
						expectedBinding = "b"
					}
					if searchCalls == 3 {
						expectedProfile, expectedBinding = "zeta", "z"
					}
					assert.Equal(t, expectedProfile, body.Profile)
					assert.Equal(t, expectedBinding, body.BindingID)
					assert.Equal(t, tc.limit, body.Limit)
					assert.Equal(t, []string{processingTestVersionID}, body.Fence.ContentVersionIDs)
					if tc.mixed && searchCalls == 1 {
						problem(w, "processing_consent_required")
						return
					}
					if tc.cold && searchCalls == 1 {
						problem(w, "processing_failed")
						return
					}
					if tc.searchError != "" {
						problem(w, tc.searchError)
						return
					}
					if tc.malformed {
						_, _ = w.Write([]byte(`{"actual_mode":"semantic"}`))
						return
					}
					assert.NoError(t, json.MarshalWrite(w, api.DocumentSearchReport{RequestedMode: "hybrid", ActualMode: "hybrid", Coverage: api.DocumentSearchCoverage{State: "complete", BindingRequired: true, ScopedDocuments: 1, CompleteDocuments: 1}, Results: []api.DocumentSearchResult{{VaultUID: vault, NodeID: 42, ContentVersionID: processingTestVersionID, Rank: 1, Score: .75, Path: "/query.txt", Excerpt: "query words", LexicalRank: 1, Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}}}}))
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusNotFound)
				}
			}))
			defer server.Close()
			if tc.jsonCLI || tc.humanCLI {
				root := t.TempDir()
				t.Setenv("DOCBANK_HOME", root)
				record = daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "key", "synthetic-token", "")
				_, err := daemonconn.RuntimeStore(root).Write(record)
				require.NoError(t, err)

				resetFlags(rootCmd)
				defer resetFlags(rootCmd)
				var stdout, stderr bytes.Buffer
				rootCmd.SetOut(&stdout)
				rootCmd.SetErr(&stderr)
				args := append([]string{"search"}, strings.Split(query, " ")...)
				if tc.jsonCLI {
					args = append(args, "--json")
				}
				rootCmd.SetArgs(args)
				defer rootCmd.SetArgs(nil)
				require.NoError(t, rootCmd.ExecuteContext(context.Background()))
				if tc.humanCLI {
					assert.True(t, strings.HasPrefix(stdout.String(), "degraded: "+tc.wantReason+"\n"))
					assert.Contains(t, stdout.String(), "SELECTOR")
					assert.Contains(t, stdout.String(), "/query.txt")
					assert.Empty(t, stderr.String())
				} else if tc.wantReason != "" {
					var lexical api.SearchReport
					require.NoError(t, json.Unmarshal(stdout.Bytes(), &lexical))
					require.Len(t, lexical.Hits, 1)
					assert.Equal(t, "/query.txt", lexical.Hits[0].Path)
					assert.Equal(t, "degraded: "+tc.wantReason+"\n", stderr.String())
					assert.NotContains(t, stdout.String(), "results")
				} else {
					var hybrid api.DocumentSearchReport
					require.NoError(t, json.Unmarshal(stdout.Bytes(), &hybrid))
					require.Len(t, hybrid.Results, 1)
					assert.Equal(t, "hybrid", hybrid.ActualMode)
					assert.Empty(t, stderr.String())
					assert.NotContains(t, stdout.String(), "hits")
				}
			} else {
				cmd, _ := processingTestCommand()
				report, reason, err := tryAutomaticSearch(cmd, daemonconn.New(server.URL, "key"), query, tc.limit, tc.opts)
				if tc.wantError {
					require.Error(t, err)
					assert.Nil(t, report)
					assert.Empty(t, reason)
				} else {
					require.NoError(t, err)
					assert.Equal(t, tc.wantReason, reason)
					if tc.wantSearch > 0 && tc.wantReason == "" {
						require.NotNil(t, report)
						assert.Equal(t, "hybrid", report.ActualMode)
					} else {
						assert.Nil(t, report)
					}
				}
			}
			assert.Equal(t, tc.wantScope, scopeCalls)
			assert.Equal(t, tc.wantSearch, searchCalls)
		})
	}
}

func TestAutomaticSearchTransportAndCancellation(t *testing.T) {
	cmd, _ := processingTestCommand()
	_, _, err := tryAutomaticSearch(cmd, daemonconn.New("http://127.0.0.1:1", "key"), "query", 50, daemonconn.SearchOptions{})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)
	_, _, err = tryAutomaticSearch(cmd, daemonconn.New("http://127.0.0.1:1", "key"), "query", 50, daemonconn.SearchOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestSearchCLIAgentAndExplicitLexicalFilters(t *testing.T) {
	setupVaultHome(t)
	source := writeSourceFile(t, "query-words.txt", "synthetic text")
	_, err := runCLI(t, "add", source, "--dest", "/docs")
	require.NoError(t, err)
	t.Cleanup(func() { resetFlags(rootCmd) })
	out, err := runCLI(t, "search", "query", "words", "--agent")
	require.NoError(t, err)
	assert.Contains(t, out, "OK search count=1 query=\"query words\" mode=lexical degraded=false")
	assert.Equal(t, 2, strings.Count(out, "\n"))
	out, err = runCLI(t, "search", "query", "--mode", "lexical", "--mime-type", "text/plain", "--agent")
	require.NoError(t, err)
	assert.Contains(t, out, "count=1")
	out, err = runCLI(t, "search", "--mode", "lexical", "--modified-since", "2000-01-01T00:00:00Z", "--agent")
	require.NoError(t, err)
	assert.Contains(t, out, "match=filter")
}
