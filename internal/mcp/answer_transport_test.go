package mcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestAnswerQuestionScopeLimitTransport(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = fmt.Fprint(w, `{"status":413,"code":"scope_too_large","observed_scope_count":4097}`)
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	output := exportCall(t, server, "answer_question", map[string]any{"question": "example"})
	require.Equal(t, "INCOMPLETE_COVERAGE", output["status"])
	receipt := schemaMap(t, output["receipt"])
	require.EqualValues(t, 4097, receipt["observed_scope_count"])
}

func TestAnswerQuestionProcessingErrorTransport(t *testing.T) {
	for _, tc := range []struct {
		name, code, profile, binding string
		local                        bool
	}{
		{name: "daemon unavailable", code: "processing_unavailable"},
		{name: "daemon profile", code: "processing_profile_unavailable", profile: "chosen"},
		{name: "daemon reranking unavailable", code: "reranking_unavailable"},
		{name: "daemon reranking failed", code: "reranking_failed"},
		{name: "daemon processing failed", code: "processing_failed"},
		{name: "no profiles", code: "processing_unavailable", local: true},
		{name: "missing profile", code: "processing_profile_unavailable", profile: "missing", local: true},
		{name: "missing binding", code: "processing_profile_unavailable", profile: "chosen", binding: "missing", local: true},
		{name: "no reranking support", code: "reranking_unavailable", local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fence := api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}
			fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: fence.ContentVersionIDs})
			require.NoError(t, err)
			searchCalls := 0
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/processing/source-fences/resolve":
					writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: fence, FenceFingerprint: fingerprint, ObservedScopeCount: len(fence.ContentVersionIDs)})
				case "/api/v1/processing/profiles":
					profiles := []api.ProcessingProfileSummary{{Name: "chosen", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"query"}, RerankingAvailable: tc.name != "no reranking support"}}
					if tc.local && tc.code == "processing_unavailable" {
						profiles = []api.ProcessingProfileSummary{}
					}
					writeDaemonJSON(t, w, profiles)
				case "/api/v1/search", "/api/v1/search/validate":
					searchCalls++
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprintf(w, `{"status":503,"code":%q,"detail":"private provider details"}`, tc.code)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer daemon.Close()
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
			args := map[string]any{"question": "example", "rerank": true}
			if tc.profile != "" {
				args["profile"] = tc.profile
			}
			if tc.binding != "" {
				args["binding_id"] = tc.binding
			}
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "answer_question", "arguments": args}))
			require.NotContains(t, string(raw), "private provider details")
			if tc.code == "processing_failed" {
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
			} else {
				result := decodeResult(t, raw)
				require.Equal(t, true, result["isError"])
				require.Equal(t, tc.code, objectField(t, result, "structuredContent")["code"])
			}
			wantCalls := 1
			if tc.local {
				wantCalls = 0
			}
			require.Equal(t, wantCalls, searchCalls)
		})
	}
}

func TestAnswerQuestionEmptyFenceValidationDisconnectTransport(t *testing.T) {
	fence := api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{}}
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: fence.ContentVersionIDs})
	require.NoError(t, err)
	validationCalls, searchCalls := 0, 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: fence, FenceFingerprint: fingerprint})
		case "/api/v1/processing/profiles":
			writeDaemonJSON(t, w, []api.ProcessingProfileSummary{{Name: "chosen", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"query"}}})
		case "/api/v1/search/validate":
			validationCalls++
			conn, _, hijackErr := http.NewResponseController(w).Hijack()
			if hijackErr != nil {
				t.Error(hijackErr)
				return
			}
			_ = conn.Close()
		case "/api/v1/search":
			searchCalls++
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "answer_question", "arguments": map[string]any{"question": "example"}}))
	require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
	require.NotContains(t, string(raw), daemon.URL)
	require.NotContains(t, string(raw), "EOF")
	require.Equal(t, 1, validationCalls)
	require.Zero(t, searchCalls)
}

func TestAnswerQuestionRejectsBlankQuestionTransport(t *testing.T) {
	fingerprint, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}})
	require.NoError(t, err)
	calls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v1/processing/source-fences/resolve":
			writeDaemonJSON(t, w, api.DocumentSourceFenceResolution{Fence: api.ResolvedDocumentSourceFence{VaultUID: testVaultID, ContentVersionIDs: []string{testVersionID}}, FenceFingerprint: fingerprint, ObservedScopeCount: 1})
		case "/api/v1/processing/profiles":
			writeDaemonJSON(t, w, []api.ProcessingProfileSummary{{Name: "chosen", Fingerprint: strings.Repeat("a", 64), QueryEmbeddingBindings: []string{"query"}}})
		case "/api/v1/search":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprint(w, `{"status":422,"code":"search_query_required"}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL))
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": "answer_question", "arguments": map[string]any{"question": " \t\n "}}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	require.Zero(t, calls)
}
