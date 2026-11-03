package api_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type missLogWriterDriver struct {
	docsqlite.Driver

	writer *sql.DB
}

func (d *missLogWriterDriver) Open(path string, options docsqlite.OpenOptions) (*sql.DB, error) {
	db, err := d.Driver.Open(path, options)
	if err == nil && options.TransactionMode == docsqlite.Immediate {
		d.writer = db
	}
	return db, err
}

func TestMissLogDoesNotWaitForOrdinaryWrites(t *testing.T) {
	t.Parallel()
	for _, entry := range []struct {
		name                 string
		enabled, logMutation bool
	}{{"disabled", false, false}, {"writer", true, false}, {"queued log mutation", true, true}} {
		t.Run(entry.name, func(t *testing.T) {
			driver := &missLogWriterDriver{Driver: store.DefaultSQLiteDriver()}
			ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = entry.enabled }, driver)
			// This is the Store's actual writer pool, not an independently opened database.
			writer, err := driver.writer.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			_, err = writer.ExecContext(t.Context(), `UPDATE nodes SET name=name WHERE id=$1`, st.RootID())
			require.NoError(t, err)
			if entry.logMutation {
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				waiting := driver.writer.Stats().WaitCount
				go func() { done <- st.PruneMissLogRecords(ctx, time.Now(), 90, 5000) }()
				defer func() { cancel(); require.ErrorIs(t, <-done, context.Canceled) }()
				require.Eventually(t, func() bool { return driver.writer.Stats().WaitCount > waiting }, 3*time.Second, time.Millisecond)
			}
			// The writer stays held until the response; this is only a hang guard.
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/search?q=synthetic-owned-writer-miss", nil)
			require.NoError(t, err)
			response, err := ts.Client().Do(request)
			require.NoError(t, err, "successful search must not wait for automatic receipt persistence")
			defer func() { _ = response.Body.Close() }()
			require.Equal(t, http.StatusOK, response.StatusCode)
			var report api.SearchReport
			require.NoError(t, json.UnmarshalRead(response.Body, &report))
			require.Empty(t, report.MissLogID)
		})
	}
}

func TestMissLogDoesNotWaitForMaintenance(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, _ := newTestServer(t, func(d *api.Deps) {
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
	response, err := ts.Client().Do(request)
	require.NoError(t, err, "an empty search must not wait for best-effort logging")
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var report api.SearchReport
	require.NoError(t, json.UnmarshalRead(response.Body, &report))
	require.Empty(t, report.MissLogID)
}

func TestMissLogDeletionRejectsMaintenance(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, st := newTestServer(t, func(d *api.Deps) {
		d.Cfg.MissLog.Enabled = true
		d.Gate = gate
	})
	resp, body := get(t, ts, "/api/v1/search?q=synthetic-retained-miss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.NotEmpty(t, report.MissLogID)
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
	for _, entry := range []struct{ name, path string }{
		{"record", "/api/v1/miss-log/" + report.MissLogID},
		{"before", "/api/v1/miss-log?before=2100-01-01T00:00:00Z"},
		{"purge", "/api/v1/miss-log"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodDelete, ts.URL+entry.path, nil)
			require.NoError(t, err)
			request.Header.Set("X-Api-Key", testAPIKey)
			response, err := ts.Client().Do(request)
			require.NoError(t, err, "miss log deletion must not wait for maintenance")
			defer func() { _ = response.Body.Close() }()
			require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
			var problem api.Error
			require.NoError(t, json.UnmarshalRead(response.Body, &problem))
			require.Equal(t, "maintenance_busy", problem.Code)
		})
	}
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "rejected deletion must preserve the private receipt")
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
			ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = enabled })
			resp, body := get(t, ts, "/api/v1/search?q=synthetic-private-miss", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var result map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &result))
			records, err := s.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			if enabled {
				require.Len(t, records, 1)
				require.NotEmpty(t, result["miss_log_id"])
				require.Contains(t, records[0].Receipt, "synthetic-private-miss")
			} else {
				require.Empty(t, records)
				require.Nil(t, result["miss_log_id"])
			}
			_, err = s.Mkdir(t.Context(), s.RootID(), "synthetic-hit")
			require.NoError(t, err)
			resp, body = get(t, ts, "/api/v1/search?q=synthetic-hit", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			again, err := s.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Len(t, again, len(records))
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
	for _, entry := range []struct{ method, path string }{{"GET", "/api/v1/miss-log"}, {"DELETE", "/api/v1/miss-log/record-id"}, {"DELETE", "/api/v1/miss-log"}} {
		resp, body = do(t, ts, entry.method, entry.path, browser, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
		resp, body = do(t, ts, entry.method, entry.path, nil, nil)
		require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, resp.StatusCode, body)
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
	require.NotEmpty(t, report["miss_log_id"])
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
	require.NotEmpty(t, report.MissLogID)
	resp, body = get(t, ts, "/api/v1/miss-log/"+report.MissLogID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Contains(t, body, "unfindablequestionzz")
	require.Contains(t, body, node.CurrentVersionID)
	require.Contains(t, body, `"content_first":true`)
	resp, body = get(t, ts, "/api/v1/miss-log?limit=1", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var page api.MissLogPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log?before=invalid", nil, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log?before=0001-01-01T00:00:00Z", nil, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	records, err := s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1, "a supplied zero cutoff must not purge existing receipts")
	require.Contains(t, records[0].Receipt, report.MissLogID)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+report.MissLogID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/miss-log/"+report.MissLogID, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/search?modified_since=2100-01-01T00:00:00Z", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	records, err = s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Contains(t, records[0].Receipt, `"query":""`)
	require.Contains(t, records[0].Receipt, `"live_filters":true`)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log?before=2100-01-01T00:00:00Z", nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	records, err = s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, records)
}

func TestMissLogCompletionUsesBoundedOwnerReceipt(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	input := map[string]any{"search": map[string]any{"query": "synthetic merged miss", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": s.VaultID(), "content_version_ids": []string{}}}, "actual_mode": "lexical", "coverage": map[string]any{"state": "complete", "binding_required": false, "scoped_documents": 0, "complete_documents": 0}}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/search/complete-empty", nil, input)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var completion api.EmptySearchCompletion
	require.NoError(t, json.Unmarshal([]byte(body), &completion))
	require.NotEmpty(t, completion.MissLogID)
	rows, err := s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Receipt, `"source":"client_finalized"`)
	input["excerpt"] = "must not persist"
	resp, body = do(t, ts, http.MethodPost, "/api/v1/search/complete-empty", nil, input)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	rows, err = s.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestMissLogFeedbackPersistsHitsAndProtectsCapability(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	node, err := st.Mkdir(t.Context(), st.RootID(), "synthetic-hit")
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/search?q=synthetic-hit", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	id, _ := result["miss_log_id"].(string)
	capability, _ := result["miss_log_capability"].(string)
	require.Len(t, id, 48)
	require.Len(t, capability, 64)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	feedback := map[string]any{"id": id, "capability": capability, "feedback": map[string]any{"verdict": "low_confidence"}}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/search/feedback", nil, feedback)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NotContains(t, body, "synthetic-hit")
	require.NotContains(t, body, capability)
	resp, body = get(t, ts, "/api/v1/miss-log/"+id, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt misslog.Receipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	require.Len(t, receipt.Hits, 1)
	require.Equal(t, node.ID, receipt.Hits[0].NodeID)
	require.Nil(t, receipt.Hits[0].Score)
	require.NotContains(t, body, capability)
	feedback["capability"] = "incorrect"
	resp, body = do(t, ts, http.MethodPost, "/api/v1/search/feedback", nil, feedback)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log/"+id+"/feedback", nil, map[string]any{"verdict": "none_in_vault"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+id, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	feedback["capability"] = capability
	resp, body = do(t, ts, http.MethodPost, "/api/v1/search/feedback", nil, feedback)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
}

func TestMissLogFeedbackValidatesAndRequiresOwner(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	resp, body := get(t, ts, "/api/v1/search?q=synthetic-miss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	for _, input := range []map[string]any{{"verdict": "other"}, {"verdict": "none_in_vault", "expected": []map[string]any{{"node_id": st.RootID()}}}, {"verdict": "wrong", "expected": []map[string]any{{"node_id": 999999}}}} {
		resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log/"+report.MissLogID+"/feedback", nil, input)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	for _, path := range []string{"/api/v1/search/feedback", "/api/v1/miss-log/" + report.MissLogID + "/feedback"} {
		resp, body = do(t, ts, http.MethodPost, path, browser, map[string]any{"verdict": "wrong"})
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
}

func TestMissLogCompletionRequiresHitNodeID(t *testing.T) {
	t.Parallel()
	// Request validation must still run when recording is disabled.
	ts, st := newTestServer(t, nil)
	input := map[string]any{
		"search":      map[string]any{"query": "synthetic missing hit identity", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{}}},
		"actual_mode": "lexical",
		"coverage":    map[string]any{"state": "complete", "binding_required": false, "scoped_documents": 0, "complete_documents": 0},
		"hits":        []map[string]any{{"rank": 1}},
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/search/complete", nil, input)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Equal(t, "validation", decodeProblem(t, body).Code)
}

func TestMissLogMergedHitCompletionKeepsFinalReceipt(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	node := createFileWithContent(t, ts, st, "/synthetic.txt", "synthetic content")
	search := map[string]any{"query": "synthetic merged hit", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{node.CurrentVersionID}}}
	hits := []map[string]any{{"rank": 1, "node_id": node.ID, "content_version_id": node.CurrentVersionID, "path": "/synthetic.txt", "score": 0.5}}
	input := map[string]any{"search": search, "actual_mode": "lexical", "coverage": map[string]any{"state": "complete", "binding_required": false, "scoped_documents": 1, "complete_documents": 1}, "hits": hits}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/search/complete", nil, input)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var completion map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &completion))
	id, ok := completion["miss_log_id"].(string)
	require.True(t, ok, "completion should return a miss-log ID")
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log/"+id+"/feedback", nil, map[string]any{"verdict": "wrong", "expected": []map[string]any{{"node_id": node.ID, "content_version_id": node.CurrentVersionID}}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/miss-log/"+id, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var receipt misslog.Receipt
	require.NoError(t, json.Unmarshal([]byte(body), &receipt))
	require.Equal(t, "client_finalized", receipt.Source)
	require.Len(t, receipt.Hits, 1)
	require.Len(t, receipt.Feedback.Expected, 1)
	hits[0]["excerpt"] = "private excerpt must be rejected"
	resp, body = do(t, ts, http.MethodPost, "/api/v1/search/complete", nil, input)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
}

func TestMissLogFeedbackValidationDoesNotEchoPrivateNote(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	resp, body := get(t, ts, "/api/v1/search?q=synthetic-private-miss", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var result api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	note := "private-note-sentinel" + strings.Repeat("x", 1001)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log/"+result.MissLogID+"/feedback", nil, map[string]any{"verdict": "wrong", "note": note})
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.NotContains(t, body, "private-note-sentinel")
}

func TestMissLogCompletionValidationNeverEchoesPrivateReceipt(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	node := createFileWithContent(t, ts, st, "/synthetic.txt", "synthetic")
	for _, field := range []string{"query", "path", "evidence", "capability"} {
		t.Run(field, func(t *testing.T) {
			sentinel := "private-" + field + "-sentinel"
			search := map[string]any{"query": "synthetic", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{node.CurrentVersionID}}}
			hits := []map[string]any{{"rank": 1, "node_id": node.ID, "content_version_id": node.CurrentVersionID, "path": "/synthetic.txt"}}
			input := map[string]any{"search": search, "actual_mode": "lexical", "coverage": map[string]any{"state": "complete", "binding_required": false, "scoped_documents": 1, "complete_documents": 1}, "hits": hits}
			path := "/api/v1/search/complete"
			switch field {
			case "query":
				search["query"] = sentinel + strings.Repeat("x", 8193)
			case "path":
				hits[0]["path"] = "/" + sentinel + strings.Repeat("x", 16385)
			case "evidence":
				hits[0]["evidence_ids"] = []string{sentinel + strings.Repeat("x", 1025)}
			case "capability":
				path = "/api/v1/search/feedback"
				input = map[string]any{"id": strings.Repeat("a", 48), "capability": sentinel + strings.Repeat("x", 65), "feedback": map[string]any{"verdict": "wrong"}}
			}
			resp, body := do(t, ts, http.MethodPost, path, nil, input)
			require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
			require.NotContains(t, body, sentinel)
		})
	}
}

func TestMissLogDocumentHitReceiptRetainsEvidenceWithoutExcerpts(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { configureProcessingTestService(t)(d); d.Cfg.MissLog.Enabled = true })
	node := createFileWithContent(t, ts, st, "/synthetic.txt", "needle plus privateexcerptsentinel\n")
	selector := map[string]any{"node_id": node.ID, "content_version_id": node.CurrentVersionID, "profile": "private"}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/processing/plans", nil, map[string]any{"selector": selector})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var plan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(body), &plan))
	resp, body = do(t, ts, http.MethodPost, "/api/v1/processing/jobs", nil, map[string]any{"selector": selector, "plan_fingerprint": plan.Fingerprint, "consent": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/search", nil, map[string]any{"query": "needle", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{node.CurrentVersionID}}})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var report api.DocumentSearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.Results, 1)
	require.Len(t, report.MissLogCapability, 64)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Contains(t, report.Results[0].Excerpt, "privateexcerptsentinel")
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log/"+report.MissLogID+"/feedback", nil, map[string]any{"verdict": "wrong"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	row, err := st.MissLogRecordByID(t.Context(), report.MissLogID)
	require.NoError(t, err)
	require.NotContains(t, row.Receipt, "privateexcerptsentinel")
	var receipt misslog.Receipt
	require.NoError(t, json.Unmarshal([]byte(row.Receipt), &receipt))
	require.Len(t, receipt.Hits, 1)
	require.Equal(t, node.ID, receipt.Hits[0].NodeID)
	require.Equal(t, node.CurrentVersionID, receipt.Hits[0].ContentVersionID)
	require.Equal(t, new(report.Results[0].Score), receipt.Hits[0].Score)
	require.NotEmpty(t, receipt.Hits[0].EvidenceIDs)
}

func TestMissLogCompletionUsesActualServiceEnabledState(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled injected service", true: "enabled injected service"}[enabled], func(t *testing.T) {
			ts, st := newTestServer(t, func(d *api.Deps) {
				configureProcessingTestService(t)(d)
				serviceConfig := d.Cfg.MissLog
				serviceConfig.Enabled = enabled
				d.Cfg.MissLog.Enabled = !enabled // The service is authoritative when injected.
				d.MissLog = misslog.New(d.Store, serviceConfig, nil)
			})
			input := map[string]any{"search": map[string]any{"query": "synthetic completion", "mode": "lexical", "profile": "private", "fence": map[string]any{"vault_uid": st.VaultID(), "content_version_ids": []string{"11111111-1111-4111-8111-111111111111"}}}, "actual_mode": "lexical", "coverage": map[string]any{"state": "complete", "binding_required": false, "scoped_documents": 1, "complete_documents": 0}, "hits": []any{}}
			resp, body := do(t, ts, http.MethodPost, "/api/v1/search/complete", nil, input)
			if enabled {
				require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
			var ticket api.EmptySearchCompletion
			require.NoError(t, json.Unmarshal([]byte(body), &ticket))
			require.Empty(t, ticket.MissLogID)
			require.Empty(t, ticket.MissLogCapability)
		})
	}
}
