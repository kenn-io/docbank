package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestEmptySearchHints(t *testing.T) {
	for _, tc := range []struct {
		name, query                                                string
		flags                                                      []string
		hybrid, fileHit, nonempty, filterOnly, unavailable, folder bool
		pending                                                    int
		next                                                       string
	}{
		{name: "human", pending: 1},
		{name: "extracted", pending: 0},
		{name: "JSON", flags: []string{"--json"}, pending: 1, next: "docbank search 'lease renewal' '--json=true'"},
		{name: "JSON quote", query: "tenant's lease", flags: []string{"--json"}, pending: 1},
		{name: "agent", flags: []string{"--agent"}, pending: 1},
		{name: "fuzzy", flags: []string{"--fuzzy"}, pending: 1},
		{name: "hybrid agent with lexical file", flags: []string{"--agent"}, hybrid: true, fileHit: true, pending: 1},
		{name: "hybrid JSON", flags: []string{"--json"}, hybrid: true, pending: 1, next: "docbank search 'lease renewal' '--json=true'"},
		{name: "nonempty", nonempty: true},
		{name: "hybrid nonempty", hybrid: true, nonempty: true},
		{name: "filter only", filterOnly: true},
		{name: "hybrid folder", hybrid: true, folder: true},
		{name: "single word", query: "lease"},
		{name: "coverage unavailable", unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := cmp.Or(tc.query, "lease renewal")
			if tc.filterOnly {
				query = ""
			}
			base := automaticSearchCase{query: query, limit: 50, empty: !tc.nonempty}
			if tc.hybrid {
				base.profiles = []api.ProcessingProfileSummary{{Name: "alpha", EmbeddingBindings: []string{"a"}, QueryEmbeddingBindings: []string{"a"}}}
				base.wantSearch = 1
			}
			var record kitdaemon.RuntimeRecord
			var profileCalls, searchCalls, coverageCalls int
			fallback := automaticSearchDaemon(t, base, &record, &profileCalls, &searchCalls)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/search/text-coverage":
					coverageCalls++
					if tc.unavailable {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					assert.NoError(t, json.MarshalWrite(w, api.SearchTextCoverage{NoText: 1, Families: []string{"pdf"}, Pending: tc.pending}))
				case "/api/v1/search":
					if r.Method != http.MethodGet {
						fallback(w, r)
						return
					}
					report := api.SearchReport{Limit: 50, Hits: []api.SearchHit{}}
					if tc.nonempty || tc.fileHit || tc.folder {
						report.Hits = []api.SearchHit{{Node: api.Node{ID: 42, Kind: "file", Name: "lease.txt"}, Path: "/lease.txt", Match: "name"}}
					}
					if tc.folder {
						report.Hits[0].Node.Kind = "dir"
					}
					if slices.Contains(tc.flags, "--fuzzy") {
						report.Fuzzy = &api.FuzzyReceipt{Status: "complete", Reasons: []string{}}
					}
					if tc.filterOnly {
						report.ModifiedSince = "2000-01-01T00:00:00.000000000Z"
					}
					assert.NoError(t, json.MarshalWrite(w, report))
				default:
					fallback(w, r)
				}
			}))
			defer server.Close()
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
			args := []string{"search"}
			if query != "" {
				args = append(args, query)
			} else {
				args = append(args, "--modified-since", "2000-01-01T00:00:00Z")
			}
			args = append(args, tc.flags...)
			rootCmd.SetArgs(args)
			defer rootCmd.SetArgs(nil)
			require.NoError(t, rootCmd.ExecuteContext(context.Background()))
			if tc.nonempty || tc.filterOnly || tc.folder {
				require.Zero(t, coverageCalls)
				require.Empty(t, stderr.String())
				return
			}
			require.Equal(t, 1, coverageCalls)
			if slices.Contains(tc.flags, "--json") {
				var report struct {
					Hints []searchHint `json:"hints"`
				}
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
				require.Equal(t, "no_searchable_text", report.Hints[0].Code)
				require.Equal(t, "docbank processing profiles", report.Hints[0].Next)
				require.Equal(t, "indexing_pending", report.Hints[1].Code)
				require.Equal(t, tc.next, report.Hints[1].Next)
				require.Empty(t, stderr.String())
			} else {
				if !tc.unavailable {
					require.Contains(t, stderr.String(), "Hint: 1 file (PDF) has no searchable text")
				}
				if tc.pending > 0 {
					require.Contains(t, stderr.String(), "Hint: 1 file is still being indexed")
				} else {
					require.NotContains(t, stderr.String(), "still being indexed")
				}
				if tc.hybrid || query == "lease" || slices.Contains(tc.flags, "--fuzzy") {
					require.NotContains(t, stderr.String(), "every word")
				} else {
					require.Contains(t, stderr.String(), "no name or text segment contains every word")
				}
				if slices.Contains(tc.flags, "--agent") {
					mode := "lexical"
					if tc.hybrid {
						mode = "hybrid"
					}
					require.True(t, strings.HasPrefix(stdout.String(), "OK search count=0 query=\"lease renewal\" mode="+mode+" degraded=false truncated=false"))
				} else if !tc.hybrid && !slices.Contains(tc.flags, "--fuzzy") {
					require.Equal(t, "no matches\n", stdout.String())
				}
			}
		})
	}
}
