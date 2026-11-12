package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestRoleSearchBrowserDisclosurePaths(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	file := createFileWithContent(t, ts, s, "/private-email.txt", "synthetic message")
	concealed, err := s.CreatePerson(ctx, "Concealed Person", "operator")
	require.NoError(t, err)
	visible, err := s.CreatePerson(ctx, "Visible Person", "operator")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, store.PersonDocumentAssertion{ContentVersionID: file.CurrentVersionID, PersonID: concealed.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(ctx, store.PersonDocumentAssertion{ContentVersionID: file.CurrentVersionID, PersonID: visible.PersonID, Role: "sender", Action: "assert", Revision: 1})
	require.NoError(t, err)
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"1"`}
	hiddenQuery := `{"syntax":"advanced","text":"person:\"Concealed Person\""}`
	bccQuery := `{"syntax":"advanced","text":"bcc:\"Visible Person\""}`
	saved, _ := createSavedQuery(t, ts.URL, "Restricted", bccQuery)
	nested := `{"syntax":"advanced","text":"saved:Restricted"}`
	for _, tc := range []struct{ path, body string }{
		{queryParsePath, hiddenQuery}, {queryHighlightsPath, hiddenQuery},
		{queryParsePath, bccQuery}, {queryHighlightsPath, nested},
		{"/api/v1/workspace/queries", `{"query":` + hiddenQuery + `}`},
		{"/api/v1/workspace/queries", `{"query":` + nested + `}`},
		{"/api/v1/saved-queries/" + saved.ID + "/runs", `{}`},
		{"/api/v1/exports/sources", `{"operation_id":"11111111-1111-4111-8111-111111111111","kind":"query","query":` + nested + `}`},
		{"/api/v1/exports/sources", fmt.Sprintf(`{"operation_id":"22222222-2222-4222-8222-222222222222","kind":"saved_query","saved_query_id":%q,"saved_query_revision":1}`, saved.ID)},
		{"/api/v1/search-exports", `{"version":1,"all_documents":true,"timezone":"UTC","coverage_mode":"available_only","terms":[{"number":1,"expression":"saved:Restricted","syntax":"advanced","dates":{"start":"2026-01-01","end":"2026-12-31"}}]}`},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, tc.path, browser, tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
			require.Contains(t, body, "invalid_query")
		})
	}
	resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", browser, `{"query":{},"facets":["collections","tags","media_family","extension","modified","size","text_coverage","duplicates","person_role"]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
}
