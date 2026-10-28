package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestSearchAllLiveInputSchema(t *testing.T) {
	input, _ := searchDocumentsSchemas()
	for _, tc := range []struct {
		name     string
		selector map[string]any
		valid    bool
	}{
		{"all live", map[string]any{"all_live": true}, true},
		{"filtered all live", map[string]any{"all_live": true, "filters": map[string]any{"mime_type": "text/plain"}}, true},
		{"explicit", map[string]any{"content_version_ids": []string{testVersionID}}, true},
		{"filters only", map[string]any{"filters": map[string]any{}}, true},
		{"missing", map[string]any{}, false},
		{"false", map[string]any{"all_live": false}, false},
		{"false with filters", map[string]any{"all_live": false, "filters": map[string]any{}}, false},
		{"null", map[string]any{"all_live": nil}, false},
		{"null with filters", map[string]any{"all_live": nil, "filters": map[string]any{}}, false},
		{"IDs and all live", map[string]any{"all_live": true, "content_version_ids": []string{testVersionID}}, false},
		{"IDs and filters", map[string]any{"content_version_ids": []string{testVersionID}, "filters": map[string]any{}}, false},
		{"all three", map[string]any{"all_live": true, "content_version_ids": []string{testVersionID}, "filters": map[string]any{}}, false},
		{"null filters", map[string]any{"all_live": true, "filters": nil}, false},
		{"unknown filter", map[string]any{"all_live": true, "filters": map[string]any{"unknown": true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.selector["query"] = "synthetic"
			tc.selector["profile"] = "local"
			if tc.valid {
				assertSchemaAccepts(t, input, tc.selector)
			} else {
				assertSchemaRejects(t, input, tc.selector)
				wireErr := decodeWireError(t, callToolWire(t, "search_documents", tc.selector))
				assert.Equal(t, int64(jsonrpc.CodeInvalidParams), wireErr.Code)
			}
		})
	}
	for _, keyword := range []string{"oneOf", "allOf", "anyOf"} {
		assert.NotContains(t, input, keyword)
	}
}

func TestSearchReadAllLiveUsesFinalScopeWithoutEnumerating(t *testing.T) {
	for _, mode := range []string{"", "hybrid"} {
		for _, count := range []int{0, 5000} {
			for _, filter := range []string{"none", "empty", "mime"} {
				t.Run(mode+"/"+strconv.Itoa(count)+"/filtered="+filter, func(t *testing.T) {
					requested, actual := "auto", "lexical"
					if mode != "" {
						requested, actual = mode, mode
					}
					scope := &api.DocumentSearchScope{Kind: "all_live"}
					arguments := map[string]any{"query": "synthetic", "profile": "local", "all_live": true}
					if mode != "" {
						arguments["mode"] = mode
					}
					if filter == "mime" {
						scope.Filters = &api.DocumentSourceFenceFilters{MIMEType: "text/plain"}
						arguments["filters"] = map[string]any{"mime_type": "TEXT/PLAIN"}
					}
					if filter == "empty" {
						scope.Filters = &api.DocumentSourceFenceFilters{}
						arguments["filters"] = map[string]any{}
					}
					identityCalls, searchCalls, acquisitions := 0, 0, 0
					daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/api/v1/info":
							t.Error("all_live search must not inventory blobs for vault identity")
							http.Error(w, "unexpected vault inventory", http.StatusInternalServerError)
						case "/api/v1/audit/status":
							identityCalls++
							assert.Empty(t, r.URL.RawQuery, "vault identity lookup must be unfiltered")
							writeDaemonJSON(t, w, api.AuditStatus{VaultID: testVaultID})
						case "/api/v1/search":
							searchCalls++
							var request api.DocumentSearchRequest
							if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
								return
							}
							assert.Empty(t, request.Fence.ContentVersionIDs)
							assert.Equal(t, testVaultID, request.Fence.VaultUID)
							if !assert.NotNil(t, request.Scope) {
								return
							}
							assert.Equal(t, "all_live", request.Scope.Kind)
							assert.Equal(t, requested, request.Mode)
							if filter == "mime" {
								if assert.NotNil(t, request.Scope.Filters) {
									assert.Equal(t, "TEXT/PLAIN", request.Scope.Filters.MIMEType)
								}
							}
							coverage := api.DocumentSearchCoverage{ScopedDocuments: count, State: "unknown"}
							if actual == "hybrid" {
								coverage.BindingRequired = true
								coverage.CompleteDocuments = count
								coverage.State = "complete"
							}
							results := []api.DocumentSearchResult{}
							if count > 0 {
								results = append(results, api.DocumentSearchResult{VaultUID: testVaultID, NodeID: 7,
									ContentVersionID: testVersionID, Rank: 1, Score: 1, LexicalRank: 1, Path: "/synthetic.md",
									Evidence: []api.DocumentEvidenceReference{{Kind: "node_name"}}})
							}
							writeDaemonJSON(t, w, api.DocumentSearchReport{Scope: scope, ObservedScopeCount: &count, RequestedMode: requested, ActualMode: actual, Coverage: coverage, Degradations: []string{}, Results: results})
						case "/api/v1/documents/resolve":
							var request api.DocumentSummaryResolveRequest
							if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
								return
							}
							assert.Equal(t, []api.DocumentIdentity{{NodeID: 7, ContentVersionID: testVersionID, Path: "/synthetic.md"}}, request.Identities)
							writeDaemonJSON(t, w, api.DocumentSummaryResolveResponse{Items: []api.DocumentSummary{{
								NodeID: 7, ContentVersionID: testVersionID, Path: "/synthetic.md", Name: "synthetic.md",
								MediaType: "text/markdown", ModifiedAt: "2026-08-28T00:00:00Z",
								ActiveRenditions: []api.DocumentRenditionIdentity{{ProfileFingerprint: testProfileID, AttachmentID: testAttachmentID, BuildID: testBuildID}},
							}}})
						default:
							t.Errorf("unexpected request: %s", r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					defer daemon.Close()
					lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
						acquisitions++
						return daemonconn.New(daemon.URL, ""), nil
					}, func(*daemonconn.Connection) error { return nil })
					result, err := invokeReadTool(t.Context(), lease, "search_documents", arguments)
					require.NoError(t, err)
					require.False(t, result.IsError)
					output := structuredMap(t, result.StructuredContent)
					assert.EqualValues(t, count, output["observed_scope_count"])
					assert.Equal(t, actual, output["actual_mode"])
					assert.Equal(t, map[string]any{"vault_id": testVaultID}, output["fence"])
					assert.NotContains(t, output, "fence_fingerprint")
					echoed, ok := output["scope"].(map[string]any)
					require.True(t, ok)
					assert.Equal(t, "all_live", echoed["kind"])
					if filter == "mime" {
						assert.Equal(t, map[string]any{"mime_type": "text/plain"}, echoed["filters"])
					}
					if filter == "empty" {
						assert.Equal(t, map[string]any{}, echoed["filters"])
					}
					if count > 0 {
						assert.Equal(t, []string{canonicalTestRenditionURI()}, resourceLinkURIs(result))
					} else {
						assert.Empty(t, resourceLinkURIs(result))
					}
					assert.Equal(t, 1, acquisitions, "identity, search and summaries reuse one acquired daemon connection")
					assert.Equal(t, 1, identityCalls)
					assert.Equal(t, 1, searchCalls)
				})
			}
		}
	}
}

func TestSearchReadAllLiveIdentityFailures(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"null", `{"vault_id":"00000000-0000-0000-0000-000000000000"}`, "unauthorized"} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v1/audit/status", r.URL.Path)
				assert.Empty(t, r.URL.RawQuery)
				assert.Equal(t, "identity-key", r.Header.Get("X-Api-Key"))
				if body == "unauthorized" {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusUnauthorized)
					writeDaemonJSON(t, w, api.Error{Status: http.StatusUnauthorized, Code: "unauthorized", Detail: "private identity detail"})
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(body))
				assert.NoError(t, err)
			}))
			defer daemon.Close()
			connection := daemonconn.New(daemon.URL, "identity-key")
			defer func() { assert.NoError(t, connection.Close()) }()
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				return connection, nil
			}, func(c *daemonconn.Connection) error { return c.Close() })
			_, _, err := searchDocuments(t.Context(), daemonDocumentSession(lease), []byte(`{"query":"synthetic","profile":"local","all_live":true}`))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private identity detail")
			assert.Equal(t, 1, calls, "invalid identity or authorization must not search or replay")
		})
	}
}

func TestSearchReadAllLiveIdentityCancellation(t *testing.T) {
	t.Parallel()
	started, canceled := make(chan struct{}), make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/audit/status", r.URL.Path)
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer daemon.Close()
	defer daemon.CloseClientConnections()
	connection := daemonconn.New(daemon.URL, "")
	defer func() { assert.NoError(t, connection.Close()) }()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return connection, nil
	}, func(c *daemonconn.Connection) error { return c.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := searchDocuments(ctx, daemonDocumentSession(lease), []byte(`{"query":"synthetic","profile":"local","all_live":true}`))
		done <- err
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	select {
	case <-started:
	case <-deadline.C:
		t.Fatal("identity request did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-deadline.C:
		t.Fatal("identity request did not return after cancellation")
	}
	select {
	case <-canceled:
	case <-deadline.C:
		t.Fatal("identity HTTP handler did not observe cancellation")
	}
}
