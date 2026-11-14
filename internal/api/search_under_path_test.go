package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestSearchUnderPath(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	dir, err := s.Mkdir(t.Context(), s.RootID(), "Café")
	require.NoError(t, err)
	file, err := s.CreateFile(t.Context(), dir.ID, "mercury.txt", testHash("under-path"), 4, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "mercury.txt", testHash("outside-path"), 4, "text/plain")
	require.NoError(t, err)
	for _, suffix := range []string{"", "&fuzzy=true"} {
		resp, body := get(t, ts, "/api/v1/search?q=mercury&under_path="+url.QueryEscape("/Café")+suffix, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var report api.SearchReport
		require.NoError(t, json.Unmarshal([]byte(body), &report))
		require.Equal(t, "/Café", report.UnderPath)
		require.Equal(t, dir.ID, report.UnderNodeID)
		require.Len(t, report.Hits, 1)
		require.Equal(t, file.ID, report.Hits[0].Node.ID)
	}
	for _, endpoint := range []string{"/api/v1/search?q=mercury&", "/api/v1/search/text-coverage?"} {
		for _, tc := range []struct {
			query, code string
			status      int
		}{
			{"under_path=" + url.QueryEscape("/Café/mercury.txt"), "not_dir", 422},
			{"under_path=/missing", "not_found", 404},
			{"under_path=" + url.QueryEscape("/Café/.."), "validation", 422},
			{"under_path=relative", "validation", 422},
			{"under_path=", "validation", 422},
			{"under_path", "validation", 422},
			{"under_path=" + url.QueryEscape("/"+strings.Repeat("é", 8192)), "validation", 422},
		} {
			resp, body := get(t, ts, endpoint+tc.query, nil)
			require.Equal(t, tc.status, resp.StatusCode, body)
			require.Contains(t, body, `"code":"`+tc.code+`"`)
		}
	}
}
