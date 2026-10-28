package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
)

func TestMissLogRecordingAndManagement(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "synthetic empty submission", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0)})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/miss-log", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.MissLogPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+page.Items[0].ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/missing", nil, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "another miss", RequestedMode: "lexical", ActualMode: "lexical"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log", nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMissLogBrowserMayRecordButCannotManage(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	resp, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var issued struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &issued))
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issued.Token}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", browser, misslog.Request{Query: "synthetic browser miss", RequestedMode: "lexical", ActualMode: "lexical"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for _, entry := range []struct{ method, path string }{{"GET", "/api/v1/miss-log"}, {"DELETE", "/api/v1/miss-log/" + rows[0].ID}, {"DELETE", "/api/v1/miss-log"}} {
		resp, body = do(t, ts, entry.method, entry.path, browser, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, body)
	}
}

func TestMissLogDoesNotWaitForMaintenance(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true; d.Gate = gate })
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error { close(started); <-release; return nil })
	}()
	<-started
	defer func() { close(release); require.NoError(t, <-done) }()
	start := time.Now()
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "synthetic busy miss", RequestedMode: "lexical", ActualMode: "lexical"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	require.Less(t, time.Since(start), time.Second)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestMissLogInputLimits(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	for _, modes := range [][2]string{{"semnatic", "lexical"}, {"hybrid", "semnatic"}, {"", "lexical"}, {"hybrid", ""}} {
		resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "missing", RequestedMode: modes[0], ActualMode: modes[1]})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	for field, count := range map[string]int{"coverage": 33, "reranking": 9} {
		properties := map[string]any{}
		for i := range count {
			properties[strconv.Itoa(i)] = true
		}
		resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, map[string]any{"query": "missing", "requested_mode": "lexical", "actual_mode": "lexical", "truncated": false, field: properties})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: strings.Repeat("q", 128<<10), RequestedMode: "lexical", ActualMode: "lexical"})
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, body)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}
