package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchDocumentsProfileCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, mode, profile, binding, code string
		failures                           bool
		want                               []string
		wantProfile, actual, guidance      string
	}{
		{name: "automatic retries", mode: "hybrid", want: []string{"local-text:first:hybrid", "local-text:second:hybrid"}, wantProfile: "local-text", actual: "hybrid"},
		{name: "exhausted hybrid", mode: "hybrid", failures: true, want: []string{"local-text:first:hybrid", "local-text:second:hybrid", "supplied-captions::lexical"}, wantProfile: "supplied-captions", actual: "lexical", guidance: "processing_failed"},
		{name: "exhausted semantic", mode: "semantic", failures: true, want: []string{"local-text:first:semantic", "local-text:second:semantic"}, code: "semantic_unavailable", guidance: "processing_failed"},
		{name: "hybrid captions", mode: "hybrid", profile: "supplied-captions", binding: "first", want: []string{"supplied-captions::lexical"}, wantProfile: "supplied-captions", actual: "lexical", guidance: "local-text"},
		{name: "auto captions", mode: "auto", profile: "supplied-captions", binding: "first", want: []string{"supplied-captions::lexical"}, wantProfile: "supplied-captions", actual: "lexical", guidance: "local-text"},
		{name: "semantic captions", mode: "semantic", profile: "supplied-captions", binding: "first", code: "semantic_unavailable", guidance: "local-text"},
		{name: "semantic explicit binding failure", mode: "semantic", profile: "local-text", binding: "first", failures: true, want: []string{"local-text:first:semantic"}, code: "semantic_unavailable", guidance: "processing_failed"},
		{name: "explicit binding", mode: "hybrid", profile: "local-text", binding: "second", want: []string{"local-text:second:hybrid"}, wantProfile: "local-text", actual: "hybrid"},
		{name: "lexical explicit binding", mode: "lexical", profile: "local-text", binding: "second", want: []string{"local-text:second:lexical"}, wantProfile: "local-text", actual: "lexical"},
		{name: "unknown profile", mode: "hybrid", profile: "default", code: "processing_profile_unavailable", guidance: "local-text"},
	} {
		for _, versions := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/versions=%t", tc.name, versions), func(t *testing.T) {
				var attempts []string
				resolutions := 0
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/processing/source-fences/resolve":
						var request api.DocumentSourceFenceResolveRequest
						if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
							return
						}
						assert.Equal(t, []string{testVersionID}, request.ContentVersionIDs)
						resolutions++
						writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}, FenceFingerprint: testFenceFingerprint, ObservedScopeCount: 1})
					case "/api/v1/processing/profiles":
						if versions {
							assert.Equal(t, 1, resolutions)
						}
						writeDaemonJSON(t, w, []api.ProcessingProfileSummary{{Name: "supplied-captions"}, {Name: "local-text", QueryEmbeddingBindings: []string{"second", "first"}}})
					case "/api/v1/search":
						var request api.DocumentSearchRequest
						if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
							return
						}
						assert.Equal(t, "refund", request.Query)
						if versions {
							assert.Nil(t, request.Scope)
							assert.Equal(t, []string{testVersionID}, request.Fence.ContentVersionIDs)
						} else if assert.NotNil(t, request.Scope) {
							assert.Equal(t, "all_live", request.Scope.Kind)
						}
						attempts = append(attempts, request.Profile+":"+request.BindingID+":"+request.Mode)
						if request.BindingID == "first" || tc.failures && request.BindingID != "" {
							w.Header().Set("Content-Type", "application/problem+json")
							w.WriteHeader(http.StatusConflict)
							code := "stale_index"
							if request.BindingID == "first" {
								code = "processing_failed"
							}
							_, _ = fmt.Fprintf(w, `{"status":409,"code":%q}`, code)
							return
						}
						writeDaemonJSON(t, w, api.DocumentSearchReport{RequestedMode: request.Mode, ActualMode: request.Mode, Coverage: api.DocumentSearchCoverage{State: "complete"}, Degradations: []string{}, Results: []api.DocumentSearchResult{}})
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(daemon.Close)
				server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
				args := map[string]any{"query": "refund", "filters": map[string]any{}, "mode": tc.mode}
				if versions {
					delete(args, "filters")
					args["content_version_ids"] = []string{testVersionID}
				}
				if tc.profile != "" {
					args["profile"] = tc.profile
				}
				if tc.binding != "" {
					args["binding_id"] = tc.binding
				}
				output := exportCall(t, server, "search_documents", args)
				require.Equal(t, tc.want, attempts)
				if versions {
					require.Equal(t, 1, resolutions)
				} else {
					require.Zero(t, resolutions)
				}
				if tc.code != "" {
					require.Equal(t, tc.code, output["code"])
					require.Contains(t, output["message"], tc.guidance)
					if tc.failures {
						require.NotContains(t, output["message"], "docbank processing local")
						require.NotContains(t, output["message"], "has no available")
					}
					if tc.code == "processing_profile_unavailable" {
						require.Contains(t, output["message"], "supplied-captions")
					}
					return
				}
				require.Equal(t, tc.wantProfile, output["profile"])
				require.Equal(t, tc.actual, output["actual_mode"])
				require.Equal(t, tc.mode, output["requested_mode"])
				if tc.guidance != "" {
					require.Contains(t, fmt.Sprint(output["skipped_reasons"]), tc.guidance)
					if tc.failures {
						require.Equal(t, []any{"processing_failed"}, output["skipped_reasons"])
					}
				}
			})
		}
	}
}

func TestSearchDocumentsBindingRequiresProfile(t *testing.T) {
	wire := callToolWire(t, "search_documents", map[string]any{"query": "refund", "binding_id": "first", "filters": map[string]any{}})
	require.EqualValues(t, -32602, decodeWireError(t, wire).Code)
}

func TestSearchDocumentsAvailableProfilesFitErrorBudget(t *testing.T) {
	profiles := make([]api.ProcessingProfileSummary, 40)
	for i := range profiles {
		profiles[i].Name = fmt.Sprintf("profile-%03d-", i) + strings.Repeat("a", 115)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/processing/profiles" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeDaemonJSON(t, w, profiles)
	}))
	t.Cleanup(daemon.Close)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "search_documents", "arguments": map[string]any{"query": "refund", "profile": "default", "filters": map[string]any{}}})))
	require.Equal(t, true, result["isError"])
	output := objectField(t, result, "structuredContent")
	require.Equal(t, "processing_profile_unavailable", output["code"])
	require.Contains(t, output["message"], profiles[0].Name)
	require.Contains(t, output["message"], "omitted")
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 1024)
}
