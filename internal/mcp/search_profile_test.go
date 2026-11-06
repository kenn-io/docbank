package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		{name: "exhausted hybrid", mode: "hybrid", failures: true, want: []string{"local-text:first:hybrid", "local-text:second:hybrid", "supplied-captions::lexical"}, wantProfile: "supplied-captions", actual: "lexical", guidance: "local-text"},
		{name: "exhausted semantic", mode: "semantic", failures: true, want: []string{"local-text:first:semantic", "local-text:second:semantic"}, code: "semantic_unavailable", guidance: "local-text"},
		{name: "hybrid captions", mode: "hybrid", profile: "supplied-captions", binding: "first", want: []string{"supplied-captions::lexical"}, wantProfile: "supplied-captions", actual: "lexical", guidance: "local-text"},
		{name: "auto captions", mode: "auto", profile: "supplied-captions", binding: "first", want: []string{"supplied-captions::lexical"}, wantProfile: "supplied-captions", actual: "lexical", guidance: "local-text"},
		{name: "semantic captions", mode: "semantic", profile: "supplied-captions", binding: "first", code: "semantic_unavailable", guidance: "local-text"},
		{name: "explicit binding", mode: "hybrid", profile: "local-text", binding: "second", want: []string{"local-text:second:hybrid"}, wantProfile: "local-text", actual: "hybrid"},
		{name: "lexical explicit binding", mode: "lexical", profile: "local-text", binding: "second", want: []string{"local-text:second:lexical"}, wantProfile: "local-text", actual: "lexical"},
		{name: "unknown profile", mode: "hybrid", profile: "default", code: "processing_profile_unavailable", guidance: "local-text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts []string
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/processing/profiles":
					writeDaemonJSON(t, w, []api.ProcessingProfileSummary{{Name: "supplied-captions"}, {Name: "local-text", QueryEmbeddingBindings: []string{"second", "first"}}})
				case "/api/v1/search":
					var request api.DocumentSearchRequest
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) || !assert.NotNil(t, request.Scope) {
						return
					}
					assert.Equal(t, "refund", request.Query)
					assert.Equal(t, "all_live", request.Scope.Kind)
					attempts = append(attempts, request.Profile+":"+request.BindingID+":"+request.Mode)
					if request.BindingID == "first" || tc.failures && request.BindingID != "" {
						w.Header().Set("Content-Type", "application/problem+json")
						w.WriteHeader(http.StatusConflict)
						_, _ = fmt.Fprint(w, `{"status":409,"code":"stale_index"}`)
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
			if tc.profile != "" {
				args["profile"] = tc.profile
			}
			if tc.binding != "" {
				args["binding_id"] = tc.binding
			}
			output := exportCall(t, server, "search_documents", args)
			require.Equal(t, tc.want, attempts)
			if tc.code != "" {
				require.Equal(t, tc.code, output["code"])
				require.Contains(t, output["message"], tc.guidance)
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
			}
		})
	}
}
