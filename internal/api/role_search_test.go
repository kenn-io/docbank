package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

// Every snapshot-building route refuses Bcc, including inside saved queries.
func TestRoleSearchBrowserPrivacyRouteMatrix(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	_, err := s.CreatePerson(t.Context(), "Matrix Concealed", "operator")
	require.NoError(t, err)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"1"`}
	payload := `{"syntax":"advanced","text":"bcc:\"Matrix Concealed\""}`
	saved, _ := createSavedQuery(t, ts.URL, "Matrix concealed", payload)
	nested := fmt.Sprintf(`{"syntax":"advanced","text":%q}`, `saved:"Matrix concealed"`)
	exportQuery := fmt.Sprintf(`{"v":1,"syntax":"advanced","mode":"lexical","filters":{},"sort":{"field":"name","direction":"asc"},"text":%q}`, `saved:"Matrix concealed"`)
	routes := []struct{ name, path, body string }{
		{"parse", queryParsePath, payload},
		{"highlights", queryHighlightsPath, nested},
		{"workspace", "/api/v1/workspace/queries", `{"query":` + payload + `}`},
		{"saved run", "/api/v1/saved-queries/" + saved.ID + "/runs", `{}`},
		{"query export", "/api/v1/exports/sources", `{"operation_id":"33333333-3333-4333-8333-333333333333","kind":"query","query":` + exportQuery + `}`},
		{"saved export", "/api/v1/exports/sources", fmt.Sprintf(`{"operation_id":"44444444-4444-4444-8444-444444444444","kind":"saved_query","saved_query_id":%q,"saved_query_revision":1}`, saved.ID)},
		{"report", "/api/v1/search-exports", fmt.Sprintf(`{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only","terms":[{"number":1,"expression":%q,"syntax":"advanced","dates":{"start":"2020-01-01","end":"2100-01-01"}}]}`, `saved:"Matrix concealed"`)},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, route.path, browser, route.body)
			require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
			require.Contains(t, body, "invalid_query")
			require.Contains(t, body, "bcc: is not available with safe people disclosure")
		})
	}
	reportRoute := routes[len(routes)-1]
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, reportRoute.path+"?people_disclosure=safe", map[string]string{"X-Api-Key": testAPIKey}, reportRoute.body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	require.Contains(t, body, "bcc: is not available with safe people disclosure")
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", browser, `{"query":{},"facets":["collections","tags","media_family","extension","modified","size","text_coverage","duplicates","person_role"]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}
