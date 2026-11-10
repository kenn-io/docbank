package api_test

import (
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
)

func TestDocumentSearchMissUsesKnownDocumentCounts(t *testing.T) {
	t.Parallel()
	allLive := api.DocumentSearchRequest{Scope: &api.DocumentSearchScope{Kind: "all_live"}}
	unknown := api.DocumentSearchReport{Coverage: api.DocumentSearchCoverage{State: "unknown"}}
	unknownMiss := api.DocumentSearchMiss("synthetic question", "private", allLive, unknown)
	assert.Nil(t, unknownMiss.DocumentCount)

	known := api.DocumentSearchReport{
		Coverage:  api.DocumentSearchCoverage{BindingRequired: true, ScopedDocuments: 3, CompleteDocuments: 1, State: "incomplete"},
		Truncated: true, Degradations: []string{"synthetic degradation"},
		Reranking: &api.DocumentSearchRerankingReceipt{Outcome: "applied", Cause: "synthetic cause", CandidateCount: 2},
	}
	knownMiss := api.DocumentSearchMiss("synthetic question", "private", allLive, known)
	require.NotNil(t, knownMiss.DocumentCount)
	assert.Equal(t, 3, *knownMiss.DocumentCount)
	assert.Equal(t, map[string]any{"binding_required": true, "scoped_documents": 3, "complete_documents": 1, "state": "incomplete"}, knownMiss.Coverage)
	assert.True(t, knownMiss.Truncated)
	assert.Equal(t, []string{"synthetic degradation"}, knownMiss.Degradations)
	assert.Equal(t, map[string]any{"outcome": "applied", "cause": "synthetic cause", "candidate_count": 2}, knownMiss.Reranking)

	emptyFence := api.DocumentSearchRequest{Fence: api.DocumentSourceFence{VaultUID: "synthetic-vault"}}
	emptyFenceMiss := api.DocumentSearchMiss("synthetic question", "private", emptyFence, unknown)
	require.NotNil(t, emptyFenceMiss.DocumentCount)
	assert.Zero(t, *emptyFenceMiss.DocumentCount)
}

func TestMissLogRecordingAndManagement(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	filters := map[string]any{"cameras": []any{"Synthetic Camera"}}
	for i, receipt := range []misslog.Request{
		{Query: "synthetic empty submission", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0)},
		{Query: "synthetic photo", RequestedMode: "lexical", ActualMode: "lexical", Filters: filters},
	} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, receipt)
		require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
		resp, body = get(t, ts, "/api/v1/miss-log", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		var page api.MissLogPage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Items, i+1)
		require.Equal(t, receipt.Query, page.Items[0].Query)
		require.Equal(t, receipt.Filters, page.Items[0].Filters)
	}
	rows, err := listMisses(t, ts)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	resp, body := do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+rows[0].ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err = listMisses(t, ts)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "synthetic empty submission", rows[0].Query)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/missing", nil, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log", nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err = listMisses(t, ts)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMissLogSubmittedLexicalTextCoverage(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	pdf, err := s.CreateFile(t.Context(), s.RootID(), "contract.pdf", testHash("pdf"), 3, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "notes.txt", testHash("notes"), 5, "text/plain")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), tag.ID, pdf.ID, pdf.Revision)
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		query    string
		mode     string
		filters  map[string]any
		coverage map[string]any
		wantText any
		count    *int
		fence    string
	}{
		{name: "web and TUI", query: "lease renewal", mode: "lexical", wantText: map[string]any{"no_text": float64(1), "families": []any{"pdf"}, "pending": float64(1)}},
		{name: "selected tag", query: "lease renewal", mode: "lexical", filters: map[string]any{"tag_id": tag.ID}, wantText: map[string]any{"no_text": float64(1), "families": []any{"pdf"}, "pending": float64(0)}},
		{name: "preserve client coverage", query: "lease renewal", mode: "lexical", coverage: map[string]any{"text": map[string]any{"pending": float64(9)}, "fuzzy": "not_run"}, wantText: map[string]any{"pending": float64(9)}},
		{name: "preserve other coverage", query: "lease renewal", mode: "lexical", coverage: map[string]any{"state": "incomplete"}, wantText: map[string]any{"no_text": float64(1), "families": []any{"pdf"}, "pending": float64(1)}},
		{name: "nonlexical", query: "lease renewal", mode: "semantic"},
		{name: "filter only", query: " \t ", mode: "lexical", filters: map[string]any{"tag_id": tag.ID}},
		{name: "unavailable scope", query: "lease renewal", mode: "lexical", filters: map[string]any{"tag_id": "not-a-tag"}},
		{name: "advanced documents", query: "lease renewal", mode: "lexical", filters: map[string]any{"v": float64(1), "syntax": "simple", "mode": "lexical", "filters": map[string]any{"mime_types": []any{"text/plain"}}}, coverage: map[string]any{"configuration": "unconfigured"}},
		{name: "photos", query: "lease renewal", mode: "lexical", filters: map[string]any{"corpus": "photos", "syntax": "simple", "hidden": false}, coverage: map[string]any{"coverage_incomplete": false}},
		{name: "empty fence", query: "lease renewal", mode: "lexical", count: new(0), coverage: map[string]any{"state": "complete", "scoped_documents": float64(0)}},
		{name: "exact fence", query: "lease renewal", mode: "lexical", count: new(1), fence: "sha256:synthetic", coverage: map[string]any{"state": "incomplete", "scoped_documents": float64(1)}},
		{name: "flat MIME filter", query: "lease renewal", mode: "lexical", filters: map[string]any{"mime_type": "text/plain"}},
		{name: "mixed flat filters", query: "lease renewal", mode: "lexical", filters: map[string]any{"tag_id": tag.ID, "mime_type": "text/plain"}},
		{name: "unrecognized fence", query: "lease renewal", mode: "lexical", fence: "sha256:synthetic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{
				Query: tc.query, RequestedMode: tc.mode, ActualMode: tc.mode, Filters: tc.filters, Coverage: tc.coverage,
				DocumentCount: tc.count, FenceFingerprint: tc.fence,
			})
			require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
			rows, err := listMisses(t, ts)
			require.NoError(t, err)
			require.NotEmpty(t, rows)
			require.Equal(t, tc.query, rows[0].Query)
			require.Equal(t, tc.wantText, rows[0].Coverage["text"])
			require.Equal(t, tc.filters, rows[0].Filters)
			require.Equal(t, tc.count, rows[0].DocumentCount)
			require.Equal(t, tc.fence, rows[0].FenceFingerprint)
			for key, value := range tc.coverage {
				require.Equal(t, value, rows[0].Coverage[key])
			}
		})
	}
}

func TestMissLogBrowserMayRecordButCannotManage(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", browser, misslog.Request{Query: "synthetic browser miss", RequestedMode: "lexical", ActualMode: "lexical"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err := listMisses(t, ts)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for _, entry := range []struct{ method, path string }{{"GET", "/api/v1/miss-log"}, {"DELETE", "/api/v1/miss-log/" + rows[0].ID}, {"DELETE", "/api/v1/miss-log"}} {
		resp, body = do(t, ts, entry.method, entry.path, browser, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
}

func TestMissLogRecordsRequestNearBodyLimit(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	request := misslog.Request{Query: "synthetic miss", RequestedMode: "lexical", ActualMode: "lexical", Filters: map[string]any{"value": ""}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	request.Filters["value"] = strings.Repeat("x", 65471-len(encoded))
	encoded, err = json.Marshal(request)
	require.NoError(t, err)
	require.Len(t, encoded, 65471)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, request)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err := listMisses(t, ts)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, request.Query, rows[0].Query)
	require.Equal(t, request.Filters, rows[0].Filters)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{
		Query: "large", RequestedMode: "lexical", ActualMode: "lexical", Filters: map[string]any{"value": strings.Repeat("x", 64<<10)},
	})
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, body)
}

func TestMissLogInputLimits(t *testing.T) {
	t.Parallel()
	type validationCase struct {
		name     string
		fields   map[string]any
		accepted bool
	}
	var tests []validationCase
	for _, field := range []string{"profile", "binding_id", "query"} {
		limit := 128
		if field == "query" {
			limit = 32768
		}
		for _, size := range []int{limit, limit + 1} {
			tests = append(tests, validationCase{field + "/" + strconv.Itoa(size), map[string]any{field: strings.Repeat("x", size)}, size == limit})
		}
	}
	for _, modes := range [][2]string{{"semnatic", "lexical"}, {"hybrid", "semnatic"}, {"", "lexical"}, {"hybrid", ""}} {
		tests = append(tests, validationCase{"modes/" + modes[0] + "/" + modes[1], map[string]any{"requested_mode": modes[0], "actual_mode": modes[1]}, false})
	}
	for field, count := range map[string]int{"coverage": 33, "reranking": 9} {
		properties := map[string]any{}
		for i := range count {
			properties[strconv.Itoa(i)] = true
		}
		tests = append(tests, validationCase{field, map[string]any{field: properties}, false})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
			request := map[string]any{"query": "synthetic miss", "requested_mode": "lexical", "actual_mode": "lexical", "truncated": false}
			maps.Copy(request, test.fields)
			resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, request)
			rows, err := listMisses(t, ts)
			require.NoError(t, err)
			if !test.accepted {
				assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
				assert.Empty(t, rows)
				return
			}
			require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
			require.Len(t, rows, 1)
			for field, value := range test.fields {
				switch field {
				case "profile":
					assert.Equal(t, value, rows[0].Profile)
				case "binding_id":
					assert.Equal(t, value, rows[0].BindingID)
				case "query":
					assert.Equal(t, value, rows[0].Query)
				}
			}
		})
	}
}

func listMisses(t *testing.T, ts *httptest.Server) ([]misslog.Receipt, error) {
	t.Helper()
	resp, body := get(t, ts, "/api/v1/miss-log", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.MissLogPage
	err := json.Unmarshal([]byte(body), &page)
	return page.Items, err
}
