package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

// Every snapshot-building route must resolve on the browser's visibility scope,
// including when the restricted definition is reached through a saved query.
func TestRoleSearchBrowserPrivacyRouteMatrix(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	private := createFileWithContent(t, ts, s, "/matrix-private.txt", "synthetic private")
	public := createFileWithContent(t, ts, s, "/matrix-public.txt", "synthetic public")
	concealed, err := s.CreatePerson(ctx, "Matrix Concealed", "operator")
	require.NoError(t, err)
	visible, err := s.CreatePerson(ctx, "Matrix Visible", "operator")
	require.NoError(t, err)
	for _, edge := range []struct{ version, id, role string }{
		{private.CurrentVersionID, concealed.PersonID, "blind_copy"},
		{private.CurrentVersionID, visible.PersonID, "blind_copy"},
		{public.CurrentVersionID, visible.PersonID, "sender"},
	} {
		_, err = s.AssertDocumentPerson(ctx, store.PersonDocumentAssertion{ContentVersionID: edge.version, PersonID: edge.id, Role: edge.role, Action: "assert", Revision: 1})
		require.NoError(t, err)
	}
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"1"`}
	for _, tc := range []struct{ name, payload, message string }{
		{"concealed", `{"syntax":"advanced","text":"person:\"Matrix Concealed\""}`, "unknown person reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, _ := createSavedQuery(t, ts.URL, "Matrix "+tc.name, tc.payload)
			nested := fmt.Sprintf(`{"syntax":"advanced","text":%q}`, `saved:"Matrix `+tc.name+`"`)
			routes := []struct{ name, path, body string }{
				{"parse", queryParsePath, tc.payload},
				{"highlights", queryHighlightsPath, nested},
				{"workspace", "/api/v1/workspace/queries", `{"query":` + tc.payload + `}`},
				{"saved run", "/api/v1/saved-queries/" + saved.ID + "/runs", `{}`},
				{"query export", "/api/v1/exports/sources", `{"operation_id":"33333333-3333-4333-8333-333333333333","kind":"query","query":` + nested + `}`},
				{"saved export", "/api/v1/exports/sources", fmt.Sprintf(`{"operation_id":"44444444-4444-4444-8444-444444444444","kind":"saved_query","saved_query_id":%q,"saved_query_revision":1}`, saved.ID)},
				{"report", "/api/v1/search-exports", fmt.Sprintf(`{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only","terms":[{"number":1,"expression":%q,"syntax":"advanced","dates":{"start":"2020-01-01","end":"2100-01-01"}}]}`, `saved:"Matrix `+tc.name+`"`)},
			}
			for _, route := range routes {
				t.Run(route.name, func(t *testing.T) {
					resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, route.path, browser, route.body)
					require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
					require.Contains(t, body, "invalid_query")
					require.Contains(t, body, tc.message)
				})
			}
		})
	}
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", browser, `{"query":{},"facets":["collections","tags","media_family","extension","modified","size","text_coverage","duplicates","person_role"]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	// The same concealed-only reference is valid for the API key owner.
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": testAPIKey}, `{"query":{"syntax":"advanced","text":"person:\"Matrix Concealed\""}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var full api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &full))
	require.EqualValues(t, 1, full.Total)
	// A visible person resolves in both scopes, but their hidden edge must not
	// contribute to a browser snapshot or an export source.
	payload := `{"syntax":"advanced","text":"person:\"Matrix Visible\""}`
	saved, _ := createSavedQuery(t, ts.URL, "Matrix public", payload)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int64
	}{
		{"browser", browser, 1}, {"api key", map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", tc.headers, `{"query":`+payload+`}`)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var snapshot api.WorkspaceQueryResponse
			require.NoError(t, json.Unmarshal([]byte(body), &snapshot))
			require.Equal(t, tc.want, snapshot.Total)
			for _, path := range []string{queryParsePath, queryHighlightsPath} {
				resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, path, tc.headers, payload)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
			}
			reportBody := `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only","terms":[{"number":1,"expression":"person:\"Matrix Visible\"","syntax":"advanced","dates":{"start":"2020-01-01","end":"2100-01-01"}}]}`
			resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/search-exports", tc.headers, reportBody)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var summary report.Summary
			require.NoError(t, json.Unmarshal([]byte(body), &summary))
			require.Len(t, summary.Counts, 1)
			require.Equal(t, tc.want, summary.Counts[0].Hits)
			resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", tc.headers, `{}`)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var run api.SavedQueryRunResult
			require.NoError(t, json.Unmarshal([]byte(body), &run))
			require.Equal(t, tc.want, run.Snapshot.Total)
			for _, request := range []string{
				`{"operation_id":"55555555-5555-4555-8555-555555555555","kind":"query","query":` + payload + `}`,
				fmt.Sprintf(`{"operation_id":"66666666-6666-4666-8666-666666666666","kind":"saved_query","saved_query_id":%q,"saved_query_revision":1}`, saved.ID),
			} {
				resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/exports/sources", tc.headers, request)
				require.Equal(t, http.StatusOK, resp.StatusCode, body)
				var source bundle.Source
				require.NoError(t, json.Unmarshal([]byte(body), &source))
				require.EqualValues(t, tc.want, source.Total)
			}
		})
	}
}

// Every browser entry that compiles a photo selection uses the same person fence.
func TestPhotoQueryRoutesConcealBccOnlyPerson(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	file, err := s.CreateFile(ctx, s.RootID(), "synthetic.txt", testHash("synthetic"), 10, "text/plain")
	require.NoError(t, err)
	person, err := s.CreatePerson(ctx, "Concealed Person", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, store.PersonDocumentAssertion{ContentVersionID: file.CurrentVersionID, PersonID: person.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	album, err := s.CreatePhotoSet(ctx, "Synthetic album")
	require.NoError(t, err)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"1"`}
	body := `{"query":{"syntax":"advanced","text":"person:\"Concealed Person\""}}`
	for _, path := range []string{
		"/api/v1/photos/assets/query", "/api/v1/photos/assets/timeline",
		"/api/v1/photos/rejects/preflight", "/api/v1/photos/authored/prepare",
		"/api/v1/photos/albums/" + album.ID + "/members/add",
	} {
		response, result := rawJSONRequest(t, ts.URL, http.MethodPost, path, headers, body)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, path+": "+result)
		require.Contains(t, result, "unknown person reference")
	}
}
