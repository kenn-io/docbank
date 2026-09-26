package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/report"
)

func TestScopedPrincipalsCannotReadExistingTermReportHistoryOrCache(t *testing.T) {
	first := routePrincipal(time.Now(), "synthetic:first-source")
	first.SubjectID = "synthetic:first"
	second := routePrincipal(time.Now(), "synthetic:second-source")
	second.SubjectID = "synthetic:second"
	ts, catalog := newTestServer(t, func(deps *api.Deps) {
		deps.AuthenticatePrincipal = func(request *http.Request) (api.Principal, bool) {
			switch request.Header.Get("Authorization") {
			case "Bearer synthetic-first":
				return first, true
			case "Bearer synthetic-second":
				return second, true
			default:
				return api.Principal{}, false
			}
		}
	})
	createFileWithContent(t, ts, catalog, "/synthetic-first.txt", "alpha")
	createFileWithContent(t, ts, catalog, "/synthetic-second.txt", "alpha")
	requestBody := `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only","terms":[{"number":1,"expression":"alpha","syntax":"simple","dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports",
		map[string]string{"X-Api-Key": testAPIKey}, requestBody)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	require.NotEmpty(t, summary.ID)
	require.NotEmpty(t, summary.Counts)

	response, body = get(t, ts, "/api/v1/search-exports", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, summary.ID)
	require.Contains(t, body, `"total":1`)
	response, body = get(t, ts, "/api/v1/search-exports/"+summary.ID+"/csv", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, "Unique Families")

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/search-exports", requestBody},
		{http.MethodGet, "/api/v1/search-exports", ""},
		{http.MethodGet, "/api/v1/search-exports/" + summary.ID, ""},
		{http.MethodPost, "/api/v1/search-exports/" + summary.ID + "/dates", `{}`},
		{http.MethodPost, "/api/v1/search-exports/" + summary.ID + "/revisions", `{"choices":[]}`},
		{http.MethodPost, "/api/v1/search-exports/" + summary.ID + "/download", `{"format":"csv"}`},
		{http.MethodGet, "/api/v1/search-exports/" + summary.ID + "/csv", ""},
		{http.MethodGet, "/api/v1/search-exports/" + summary.ID + "/bundle", ""},
	} {
		for _, token := range []string{"synthetic-first", "synthetic-second"} {
			t.Run(token+route.method+route.path, func(t *testing.T) {
				response, body := rawJSONRequest(t, ts.URL, route.method, route.path,
					map[string]string{"X-Api-Key": "", "Authorization": "Bearer " + token}, route.body)
				require.Equal(t, http.StatusNotFound, response.StatusCode, body)
				require.Contains(t, body, `"code":"not_found"`)
				require.NotContains(t, body, summary.ID)
				require.NotContains(t, strings.ToLower(body), "alpha")
			})
		}
	}
}
