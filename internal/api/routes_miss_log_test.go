package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/misslog"
)

func TestMissLogRecordingAndManagement(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			ts, st := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = enabled })
			for _, receipt := range []misslog.Request{
				{Query: " \t"},
				{Query: "large", Filters: map[string]any{"value": strings.Repeat("x", 64<<10)}},
				{Query: "synthetic empty submission", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0)},
			} {
				resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, receipt)
				require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
			}
			rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			if enabled {
				require.Len(t, rows, 1)
			} else {
				require.Empty(t, rows)
			}
			resp, body := get(t, ts, "/api/v1/miss-log", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var page api.MissLogPage
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			require.Len(t, page.Items, len(rows))
			if enabled {
				resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+rows[0].ID, nil, nil)
				require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
			}
			resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/missing", nil, nil)
			require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
			resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log", nil, nil)
			require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
			rows, err = st.ListMissLogRecords(t.Context(), 10, 0)
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
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
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", browser, misslog.Request{Query: "synthetic browser miss"})
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
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "synthetic busy miss"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	require.Less(t, time.Since(start), time.Second)
	rows, err := st.ListMissLogRecords(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}
