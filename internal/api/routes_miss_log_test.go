package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
)

func TestMissLogDoesNotWaitForMaintenance(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, st := newTestServer(t, func(d *api.Deps) {
		d.Cfg.MissLog.Enabled = true
		d.Gate = gate
	})
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	defer func() {
		close(release)
		require.NoError(t, <-done)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/search?q=synthetic-busy-miss", nil)
	require.NoError(t, err)
	start := time.Now()
	response, err := ts.Client().Do(request)
	require.NoError(t, err, "an empty search must not wait for best-effort logging")
	defer func() { _ = response.Body.Close() }()
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var report api.SearchReport
	require.NoError(t, json.UnmarshalRead(response.Body, &report))
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMissLogPreservesFuzzyCoverage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query, actual, status string
		reasons               []any
	}{
		{"syntheticmisszz", "fuzzy", "complete", []any{}},
		{"café", "lexical", "not_run", []any{"query_ineligible"}},
	} {
		t.Run(tc.status, func(t *testing.T) {
			ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
			resp, body := get(t, ts, "/api/v1/search?fuzzy=true&q="+url.QueryEscape(tc.query), nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			var receipt misslog.Receipt
			require.NoError(t, json.Unmarshal([]byte(rows[0].Receipt), &receipt))
			require.Equal(t, "fuzzy", receipt.RequestedMode)
			require.Equal(t, tc.actual, receipt.ActualMode)
			require.Equal(t, map[string]any{"status": tc.status, "reasons": tc.reasons}, receipt.Coverage["fuzzy"])
			require.Equal(t, false, receipt.Coverage["truncated"])
		})
	}
}

func TestMissLogRecordsOnlyEnabledEmptySearch(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			ts, s := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = enabled })
			resp, body := get(t, ts, "/api/v1/search?q=synthetic-private-miss", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			resp, body = do(t, ts, http.MethodPost, "/api/v1/search", nil, api.DocumentSearchRequest{Query: "synthetic-private-miss", Mode: "lexical", Profile: "private", Fence: api.DocumentSourceFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{}}})
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			records, err := s.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			if enabled {
				require.Len(t, records, 2)
				require.Contains(t, records[0].Receipt, "synthetic-private-miss")
			} else {
				require.Empty(t, records)
			}
			_, err = s.Mkdir(t.Context(), s.RootID(), "synthetic-hit")
			require.NoError(t, err)
			resp, body = get(t, ts, "/api/v1/search?q=synthetic-hit", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			again, err := s.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Len(t, again, len(records))
			resp, body = get(t, ts, "/api/v1/search?q=another-miss&skip_miss_log=true", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			resp, body = get(t, ts, "/api/v1/miss-log", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var page api.MissLogPage
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			require.Len(t, page.Items, len(records))
		})
	}
}

func TestMissLogManagementRequiresMaster(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	for _, entry := range []struct{ method, path string }{{"GET", "/api/v1/miss-log"}, {"GET", "/api/v1/miss-log/record-id"}, {"DELETE", "/api/v1/miss-log/record-id"}, {"DELETE", "/api/v1/miss-log"}} {
		resp, body = do(t, ts, entry.method, entry.path, browser, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
}

func TestMissLogEmptyFenceIsBoundedAndValidated(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	for _, bad := range []map[string]any{
		{"query": "needle", "mode": "lexical", "profile": "private"},
		{"query": "needle", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": "00000000-0000-4000-8000-000000000000", "content_version_ids": []string{}}},
		{"query": "", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": s.VaultID(), "content_version_ids": []string{}}},
		{"query": "needle", "mode": "lexical", "profile": "unknown", "fence": map[string]any{"vault_uid": s.VaultID(), "content_version_ids": []string{}}},
	} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, bad)
		require.NotEqual(t, http.StatusOK, resp.StatusCode, body)
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{"query": "synthetic empty scope", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": s.VaultID(), "content_version_ids": []string{}}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	rows, err := s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Receipt, "synthetic empty scope")
}

func TestMissLogDirectDocumentSearchAndManagement(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	node := createFileWithContent(t, ts, s, "/synthetic.txt", "ordinary fixture content")
	resp, body := do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{"query": "unfindablequestionzz", "mode": "lexical", "profile": "private", "limit": 7, "content_first": true, "fence": map[string]any{"vault_uid": s.VaultID(), "content_version_ids": []string{node.CurrentVersionID}}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.DocumentSearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	rows, err := s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	resp, body = get(t, ts, "/api/v1/miss-log/"+rows[0].ID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Contains(t, body, "unfindablequestionzz")
	require.Contains(t, body, node.CurrentVersionID)
	require.Contains(t, body, `"content_first":true`)
	for _, request := range []api.DocumentSearchRequest{
		{Query: "another-miss", SkipMissLog: true},
		{Query: "synthetic"},
	} {
		request.Mode, request.Profile = "lexical", "private"
		request.Fence = api.DocumentSourceFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{node.CurrentVersionID}}
		resp, body = do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
	}
	resp, body = get(t, ts, "/api/v1/miss-log?limit=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var page api.MissLogPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+rows[0].ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+rows[0].ID, nil, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/search?modified_since=2100-01-01T00:00:00Z", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	records, err := s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Contains(t, records[0].Receipt, `"query":""`)
	require.Contains(t, records[0].Receipt, `"live_filters":true`)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log", nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	records, err = s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, records)
}
