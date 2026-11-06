package api_test

import (
	"encoding/json/v2"
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

	known := api.DocumentSearchReport{Coverage: api.DocumentSearchCoverage{ScopedDocuments: 3, State: "incomplete"}}
	knownMiss := api.DocumentSearchMiss("synthetic question", "private", allLive, known)
	require.NotNil(t, knownMiss.DocumentCount)
	assert.Equal(t, 3, *knownMiss.DocumentCount)

	emptyFence := api.DocumentSearchRequest{Fence: api.DocumentSourceFence{VaultUID: "synthetic-vault"}}
	emptyFenceMiss := api.DocumentSearchMiss("synthetic question", "private", emptyFence, unknown)
	require.NotNil(t, emptyFenceMiss.DocumentCount)
	assert.Zero(t, *emptyFenceMiss.DocumentCount)
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
	filters["syntax"], filters["corpus"], filters["hidden"] = "simple", "photos", false
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{Query: "synthetic photo", RequestedMode: "lexical", ActualMode: "lexical", Filters: filters})
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
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
	receipt := misslog.Request{
		Query: "synthetic empty submission", RequestedMode: "hybrid", ActualMode: "lexical", DocumentCount: new(0),
	}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, receipt)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err := listMisses(t, ts)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	resp, body = get(t, ts, "/api/v1/miss-log", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var page api.MissLogPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, "synthetic empty submission", page.Items[0].Query)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/"+rows[0].ID, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err = listMisses(t, ts)
	require.NoError(t, err)
	require.Empty(t, rows)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log/missing", nil, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, receipt)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodDelete, "/api/v1/miss-log", nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
	rows, err = listMisses(t, ts)
	require.NoError(t, err)
	require.Empty(t, rows)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{
		Query: "large", RequestedMode: "lexical", ActualMode: "lexical", Filters: map[string]any{"value": strings.Repeat("x", 64<<10)},
	})
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, body)
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

func TestMissLogIdentifierLimits(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"profile", "binding_id"} {
		for _, size := range []int{128, 129, 256} {
			t.Run(field+"/"+strconv.Itoa(size), func(t *testing.T) {
				ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
				value := strings.Repeat("x", size)
				resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, map[string]any{
					"query": "synthetic miss", "requested_mode": "lexical", "actual_mode": "lexical", "truncated": false, field: value,
				})
				rows, err := listMisses(t, ts)
				require.NoError(t, err)
				if size > 128 {
					assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
					assert.Empty(t, rows)
					return
				}
				require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
				require.Len(t, rows, 1)
				if field == "profile" {
					assert.Equal(t, value, rows[0].Profile)
				} else {
					assert.Equal(t, value, rows[0].BindingID)
				}
			})
		}
	}
}

func TestMissLogQueryLengthLimit(t *testing.T) {
	t.Parallel()
	for _, size := range []int{32768, 32769} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
			query := strings.Repeat("x", size)
			resp, body := do(t, ts, http.MethodPost, "/api/v1/miss-log", nil, misslog.Request{
				Query: query, RequestedMode: "lexical", ActualMode: "lexical",
			})
			rows, err := listMisses(t, ts)
			require.NoError(t, err)
			if size > 32768 {
				assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
				assert.Empty(t, rows)
				return
			}
			require.Equal(t, http.StatusNoContent, resp.StatusCode, body)
			require.Len(t, rows, 1)
			assert.Equal(t, query, rows[0].Query)
		})
	}
}

func TestMissLogInputLimits(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.MissLog.Enabled = true })
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
	rows, err := listMisses(t, ts)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func listMisses(t *testing.T, ts *httptest.Server) ([]misslog.Receipt, error) {
	t.Helper()
	resp, body := get(t, ts, "/api/v1/miss-log", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page api.MissLogPage
	err := json.Unmarshal([]byte(body), &page)
	return page.Items, err
}
