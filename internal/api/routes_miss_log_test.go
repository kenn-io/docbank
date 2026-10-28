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
	"go.kenn.io/docbank/internal/store"
)

func TestMissLogListExcludesExpiredRecords(t *testing.T) {
	t.Parallel()
	ts, st := newTestServer(t, func(d *api.Deps) {
		d.Cfg.MissLog.Enabled = true
		d.Cfg.MissLog.RetentionDays = 1
	})
	now := time.Now().UTC()
	for _, entry := range []store.MissLogRecord{
		{ID: "expired", CreatedAt: now.Add(-72 * time.Hour), Receipt: `{"id":"expired"}`},
		{ID: "recent", CreatedAt: now.Add(-time.Hour), Receipt: `{"id":"recent"}`},
	} {
		require.NoError(t, st.AddMissLogRecord(t.Context(), entry, now, 90, 5000))
	}
	for _, test := range []struct {
		path string
		ids  []string
	}{
		{"/api/v1/miss-log", []string{"recent"}},
		{"/api/v1/miss-log?limit=1&offset=0", []string{"recent"}},
		{"/api/v1/miss-log?limit=1&offset=1", []string{}},
	} {
		resp, body := get(t, ts, test.path, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.MissLogPage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		ids := []string{}
		for _, receipt := range page.Items {
			ids = append(ids, receipt.ID)
		}
		require.Equal(t, test.ids, ids)
	}
	require.NoError(t, st.DeleteMissLogRecord(t.Context(), "recent"))
	resp, body := get(t, ts, "/api/v1/miss-log", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.MissLogPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Empty(t, page.Items, "an idle daemon hides its three-day-old record")
}

func TestMissLogDisabledRetentionKeepsHistoryVisible(t *testing.T) {
	t.Parallel()
	for _, days := range []int{0, 1} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			ts, st := newTestServer(t, func(d *api.Deps) {
				d.Cfg.MissLog.Enabled = false
				d.Cfg.MissLog.RetentionDays = days
			})
			now := time.Now().UTC()
			require.NoError(t, st.AddMissLogRecord(t.Context(), store.MissLogRecord{
				ID: "retained", CreatedAt: now.Add(-72 * time.Hour), Receipt: `{"id":"retained"}`,
			}, now, 90, 5000))
			resp, body := get(t, ts, "/api/v1/miss-log", nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var page api.MissLogPage
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			require.Len(t, page.Items, 1)
			require.Equal(t, "retained", page.Items[0].ID)
		})
	}
}

func TestMissLogRecordsThirtyPhotoFilters(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	filters := map[string]any{
		"focus_min": "0", "focus_max": "1", "blur_min": "0", "blur_max": "1",
		"brightness_min": "0", "brightness_max": "1", "framing_min": "0", "framing_max": "1",
		"aesthetics_min": "0", "aesthetics_max": "1", "color_red_min": "0", "color_red_max": "1",
		"color_green_min": "0", "color_green_max": "1", "color_blue_min": "0", "color_blue_max": "1",
		"rating_min": float64(1), "rating_max": float64(5), "flags": []any{"pick"}, "labels": []any{"red"},
		"kinds": []any{"photo"}, "cameras": []any{"Synthetic Camera"}, "lenses": []any{"Synthetic Lens"}, "locations": []any{"Synthetic Location"},
		"iso_min": float64(100), "iso_max": float64(800), "capture_after": "2026-01-01", "capture_before": "2026-12-31",
		"unevaluated": true, "gps_bounds": map[string]any{"south": "0", "north": "45", "west": "0", "east": "90"},
	}
	query, err := json.Marshal(map[string]any{"text": "synthetic photo", "filters": filters})
	require.NoError(t, err)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(query)})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	filters["syntax"], filters["corpus"], filters["hidden"] = "simple", "photos", false
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "synthetic photo", RequestedMode: "lexical", ActualMode: "lexical", Filters: filters})
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/miss-log", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.MissLogPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, filters, page.Items[0].Filters)
}

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
	rows, err := st.ListMissLogRecords(t.Context(), time.Time{}, 10, 0)
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
	rows, err := st.ListMissLogRecords(t.Context(), time.Time{}, 10, 0)
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
	rows, err := st.ListMissLogRecords(t.Context(), time.Time{}, 10, 0)
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
	rows, err := st.ListMissLogRecords(t.Context(), time.Time{}, 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}
