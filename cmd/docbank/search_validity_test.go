package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestSearchValidityCLIReasons(t *testing.T) {
	cmd, out := processingTestCommand()
	report := api.DocumentSearchReport{AsOf: "2025-01-01", Results: []api.DocumentSearchResult{
		{NodeID: 1, Rank: 1, Path: "/old.txt", Validity: &api.DocumentResultValidity{State: "superseded", SupersededBy: []api.DocumentValiditySuccessor{{NodeID: 2, Path: "/new.txt"}}}},
		{NodeID: 3, Rank: 2, Path: "/expired.txt", Validity: &api.DocumentResultValidity{State: "expired"}},
		{NodeID: 4, Rank: 3, Path: "/future.txt", Validity: &api.DocumentResultValidity{State: "not_yet_effective"}},
	}}
	require.NoError(t, writeDocumentSearchReport(cmd, report, false, true, nil))
	require.Contains(t, out.String(), "/old.txt (superseded by /new.txt)")
	require.Contains(t, out.String(), "/expired.txt (expired)")
	require.Contains(t, out.String(), "/future.txt (not yet effective)")
	cmd, out = processingTestCommand()
	require.NoError(t, writeDocumentSearchAgent(cmd, "lease", report, nil))
	require.Contains(t, out.String(), "validity=superseded")
	require.Contains(t, out.String(), `validity_reason="(superseded by /new.txt)"`)
}

func TestAutomaticSearchValidityCLI(t *testing.T) {
	profiles := []api.ProcessingProfileSummary{{Name: "alpha", EmbeddingBindings: []string{"a"}, QueryEmbeddingBindings: []string{"a"}}}
	for _, tc := range []struct {
		name, searchError string
		profiles          []api.ProcessingProfileSummary
		wantError         string
	}{
		{name: "date reaches automatic request", profiles: profiles},
		{name: "failed attempt stops", profiles: profiles, searchError: "processing_failed", wantError: "processing_failed"},
		{name: "no automatic profile", wantError: "--as-of requires"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var record kitdaemon.RuntimeRecord
			var profileCalls, searchCalls, lexicalCalls int
			spec := automaticSearchCase{query: "lease", limit: 1, profiles: tc.profiles, searchError: tc.searchError, wantSearch: 1}
			handler := automaticSearchDaemon(t, spec, &record, &profileCalls, &searchCalls)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/v1/search" {
					raw, err := io.ReadAll(r.Body)
					if !assert.NoError(t, err) {
						return
					}
					var request api.DocumentSearchRequest
					if !assert.NoError(t, json.Unmarshal(raw, &request)) {
						return
					}
					assert.Equal(t, "2025-01-01", request.AsOf)
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
				}
				if r.Method == http.MethodGet && r.URL.Path == "/api/v1/search" {
					lexicalCalls++
				}
				handler(w, r)
			}))
			defer server.Close()
			root := t.TempDir()
			t.Setenv("DOCBANK_HOME", root)
			record = daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "key", "synthetic-token", "")
			_, err := daemonconn.RuntimeStore(root).Write(record)
			require.NoError(t, err)
			defer resetFlags(rootCmd)
			_, err = runCLI(t, "search", "lease", "--as-of", "2025-01-01", "--limit", "1")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Zero(t, lexicalCalls)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, lexicalCalls)
			}
		})
	}
}
