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
		failures, versions                 bool
		want                               []string
		wantProfile, actual, guidance      string
		failure, firstFailure              string
	}{
		{name: "automatic retries", mode: "hybrid", versions: true, want: []string{"z-local-text:first:hybrid", "z-local-text:second:hybrid"}, wantProfile: "z-local-text", actual: "hybrid"},
		{name: "semantic retries", mode: "semantic", want: []string{"z-local-text:first:semantic", "z-local-text:second:semantic"}, wantProfile: "z-local-text", actual: "semantic"},
		{name: "semantic consent before index failure", mode: "semantic", failures: true, firstFailure: "processing_consent_required", want: []string{"z-local-text:first:semantic", "z-local-text:second:semantic"}, code: "consent_required", guidance: "consent"},
		{name: "semantic index before consent failure", mode: "semantic", failure: "processing_consent_required", firstFailure: "stale_index", want: []string{"z-local-text:first:semantic", "z-local-text:second:semantic"}, code: "semantic_unavailable", guidance: "stale_index"},
		{name: "exhausted auto uses default", mode: "auto", failures: true, want: []string{"z-local-text:first:hybrid", "z-local-text:second:hybrid", "a-captions::lexical"}, wantProfile: "a-captions", actual: "lexical", guidance: "check the daemon log"},
		{name: "provider failure guidance", mode: "hybrid", profile: "z-local-text", binding: "second", failure: "processing_failed", want: []string{"z-local-text:second:hybrid", "z-local-text:second:lexical"}, wantProfile: "z-local-text", actual: "lexical", guidance: "check the daemon log"},
		{name: "semantic unavailable guidance", mode: "hybrid", profile: "z-local-text", binding: "second", failure: "semantic_unavailable", want: []string{"z-local-text:second:hybrid", "z-local-text:second:lexical"}, wantProfile: "z-local-text", actual: "lexical", guidance: "configure a text-query embedding binding"},
		{name: "semantic provider failure guidance", mode: "semantic", profile: "z-local-text", binding: "second", failure: "processing_failed", want: []string{"z-local-text:second:semantic"}, code: "semantic_unavailable", guidance: "check the daemon log"},
		{name: "semantic exhausted unavailable guidance", mode: "semantic", profile: "z-local-text", binding: "second", failure: "semantic_unavailable", want: []string{"z-local-text:second:semantic"}, code: "semantic_unavailable", guidance: "configure a text-query embedding binding"},
		{name: "hybrid captions", mode: "hybrid", profile: "a-captions", want: []string{"a-captions::lexical"}, wantProfile: "a-captions", actual: "lexical", guidance: "z-local-text"},
		{name: "explicit auto captions", mode: "auto", profile: "a-captions", want: []string{"a-captions::lexical"}, wantProfile: "a-captions", actual: "lexical"},
		{name: "explicit auto query binding", mode: "auto", profile: "z-local-text", binding: "second", want: []string{"z-local-text:second:lexical"}, wantProfile: "z-local-text", actual: "lexical"},
		{name: "semantic captions", mode: "semantic", profile: "a-captions", want: []string{"a-captions::semantic"}, code: "semantic_unavailable", guidance: "z-local-text"},
		{name: "explicit binding", mode: "hybrid", profile: "z-local-text", binding: "second", want: []string{"z-local-text:second:hybrid"}, wantProfile: "z-local-text", actual: "hybrid"},
		{name: "lexical explicit binding", mode: "lexical", profile: "z-local-text", binding: "second", want: []string{"z-local-text:second:lexical"}, wantProfile: "z-local-text", actual: "lexical"},
		{name: "stale index guidance", mode: "hybrid", profile: "z-local-text", binding: "second", failure: "stale_index", want: []string{"z-local-text:second:hybrid", "z-local-text:second:lexical"}, wantProfile: "z-local-text", actual: "lexical", guidance: "needs rebuilding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
					writeDaemonJSON(t, w, []api.ProcessingProfileSummary{{Name: "a-captions", Fingerprint: strings.Repeat("a", 64)}, {Name: "z-local-text", Fingerprint: strings.Repeat("b", 64), EmbeddingBindings: []string{"first", "second"}, QueryEmbeddingBindings: []string{"first", "second"}}})
				case "/api/v1/search":
					var request api.DocumentSearchRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
						return
					}
					if request.Profile == "a-captions" && request.Mode == "semantic" {
						attempts = append(attempts, "a-captions::semantic")
						w.Header().Set("Content-Type", "application/problem+json")
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = fmt.Fprint(w, `{"status":422,"code":"semantic_unavailable"}`)
						return
					}
					assert.Equal(t, "refund", request.Query)
					if tc.versions {
						assert.Nil(t, request.Scope)
						assert.Equal(t, []string{testVersionID}, request.Fence.ContentVersionIDs)
					} else if assert.NotNil(t, request.Scope) {
						assert.Equal(t, "all_live", request.Scope.Kind)
					}
					attempts = append(attempts, request.Profile+":"+request.BindingID+":"+request.Mode)
					if request.Mode != "lexical" && (request.BindingID == "first" || tc.failures && request.BindingID != "" || tc.failure != "") {
						w.Header().Set("Content-Type", "application/problem+json")
						status, code := http.StatusConflict, "stale_index"
						if request.BindingID == "first" {
							status, code = http.StatusInternalServerError, "processing_failed"
						}
						if tc.failure != "" {
							code = tc.failure
						}
						if request.BindingID == "first" && tc.firstFailure != "" {
							code = tc.firstFailure
						}
						w.WriteHeader(status)
						_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q}`, status, code)
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
			if tc.versions {
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
			if tc.versions {
				require.Equal(t, 1, resolutions)
			} else {
				require.Zero(t, resolutions)
			}
			if tc.code != "" {
				require.Equal(t, tc.code, output["code"])
				require.Contains(t, output["message"], tc.guidance)
				return
			}
			require.Equal(t, tc.wantProfile, output["profile"])
			require.Equal(t, tc.actual, output["actual_mode"])
			require.Equal(t, tc.mode, output["requested_mode"])
			if tc.guidance != "" {
				require.Contains(t, fmt.Sprint(output["skipped_reasons"]), tc.guidance)
			} else {
				require.Empty(t, output["skipped_reasons"])
			}
			if tc.failure != "" {
				require.Contains(t, output["skipped_reasons"], tc.failure)
			}
			if tc.failure == "stale_index" {
				require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing plan <path-or-id> --profile <profile>")
				require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing build <path-or-id> --profile <profile> --plan-fingerprint <reviewed-sha256> --consent")
			}
			if tc.failures {
				require.Contains(t, output["skipped_reasons"], "processing_failed")
				require.NotContains(t, fmt.Sprint(output["skipped_reasons"]), "z-local-text")
			}
		})
	}
}

func TestSearchDocumentsBindingRequiresProfile(t *testing.T) {
	wire := callToolWire(t, "search_documents", map[string]any{"query": "refund", "binding_id": "first", "filters": map[string]any{}})
	require.EqualValues(t, -32602, decodeWireError(t, wire).Code)
}

func TestSearchDocumentsAvailableProfilesFitErrorBudget(t *testing.T) {
	profiles := make([]api.ProcessingProfileSummary, 40)
	for i := range profiles {
		profiles[i].Fingerprint = strings.Repeat("a", 64)
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

func TestSearchDocumentsRerankingGuidance(t *testing.T) {
	for _, mode := range []string{"hybrid", "auto", "semantic"} {
		for _, alternative := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/alternative=%t", mode, alternative), func(t *testing.T) {
				profiles := []api.ProcessingProfileSummary{
					{Name: "a-captions", Fingerprint: strings.Repeat("a", 64), RerankingAvailable: true},
					{Name: "b-text", Fingerprint: strings.Repeat("b", 64), EmbeddingBindings: []string{"query"}, QueryEmbeddingBindings: []string{"query"}},
				}
				if alternative {
					profiles = append(profiles, api.ProcessingProfileSummary{Name: "z-rerank-text", Fingerprint: strings.Repeat("c", 64), RerankingAvailable: true, EmbeddingBindings: []string{"query"}, QueryEmbeddingBindings: []string{"query"}})
				}
				var attempts []string
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/processing/profiles":
						writeDaemonJSON(t, w, profiles)
					case "/api/v1/search":
						var request api.DocumentSearchRequest
						if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
							return
						}
						assert.True(t, request.Rerank)
						assert.Equal(t, "refund", request.Query)
						attempts = append(attempts, request.Profile+":"+request.Mode)
						if request.Mode == "semantic" {
							w.WriteHeader(http.StatusUnprocessableEntity)
							_, _ = fmt.Fprint(w, `{"status":422,"code":"semantic_unavailable"}`)
							return
						}
						writeDaemonJSON(t, w, api.DocumentSearchReport{RequestedMode: request.Mode, ActualMode: request.Mode, Coverage: api.DocumentSearchCoverage{State: "complete"}, Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "skipped"}})
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(daemon.Close)
				server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
				args := map[string]any{"query": "refund", "mode": mode, "rerank": true, "filters": map[string]any{}}
				if mode != "auto" {
					args["profile"] = "a-captions"
				}
				output := exportCall(t, server, "search_documents", args)
				if mode == "auto" && alternative {
					require.Equal(t, []string{"z-rerank-text:hybrid"}, attempts)
					require.Equal(t, "z-rerank-text", output["profile"])
					require.Empty(t, output["skipped_reasons"])
					return
				}
				guidance := fmt.Sprint(output["skipped_reasons"])
				if mode == "semantic" {
					require.Equal(t, []string{"a-captions:semantic"}, attempts)
					require.Equal(t, "semantic_unavailable", output["code"])
					guidance = fmt.Sprint(output["message"])
				} else {
					require.Equal(t, []string{"a-captions:lexical"}, attempts)
					require.Equal(t, "lexical", output["actual_mode"])
				}
				require.NotContains(t, guidance, `Try profile "b-text"`)
				if alternative {
					require.Contains(t, guidance, `Try profile "z-rerank-text"`)
				} else {
					require.Contains(t, guidance, "configure")
					require.Contains(t, guidance, "reranking")
				}
			})
		}
	}
}

func TestSearchDocumentsUnknownBindingValidation(t *testing.T) {
	for _, embeddings := range []bool{false, true} {
		t.Run(fmt.Sprintf("embeddings=%t", embeddings), func(t *testing.T) {
			profile := api.ProcessingProfileSummary{Name: "text", Fingerprint: strings.Repeat("a", 64)}
			if embeddings {
				profile.EmbeddingBindings, profile.QueryEmbeddingBindings = []string{"query"}, []string{"query"}
			}
			calls := 0
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/processing/profiles":
					writeDaemonJSON(t, w, []api.ProcessingProfileSummary{profile})
				case "/api/v1/search":
					calls++
					var request api.DocumentSearchRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
						return
					}
					assert.Equal(t, "text", request.Profile)
					assert.Equal(t, "missing", request.BindingID)
					assert.Equal(t, "hybrid", request.Mode)
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprint(w, `{"status":404,"code":"not_found"}`)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(daemon.Close)
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			output := exportCall(t, server, "search_documents", map[string]any{"query": "refund", "mode": "hybrid", "profile": "text", "binding_id": "missing", "filters": map[string]any{}})
			require.Equal(t, "not_found", output["code"])
			if embeddings {
				require.Equal(t, 1, calls, "the API owns validation for profiles with embeddings")
			} else {
				require.Zero(t, calls)
			}
		})
	}
}
