package api_test

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

// Every snapshot-building route refuses Bcc, including inside saved queries.
func TestRoleSearchBrowserPrivacyRouteMatrix(t *testing.T) {
	t.Parallel()
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
	require.NoError(t, s.EnsureDocumentEventRecipe(ctx, store.DocumentEventsDeriverFingerprint))
	targets, err := s.MissingDocumentEventTargetsAfter(ctx, store.DocumentEventsDeriverFingerprint, "", 100)
	require.NoError(t, err)
	for _, target := range targets {
		evidence, err := s.LoadDocumentEventEvidence(ctx, target)
		require.NoError(t, err)
		record := document.DocumentEventsV1{VaultUID: s.VaultID(), ContentVersionID: target.ContentVersionID, ContractVersion: document.DocumentEventsContractV1, DocumentKind: store.DocumentEventKindForMIMEType(target.MIMEType), Diagnostics: []document.DocumentEventDiagnosticV1{}, Events: []document.DocumentEventV1{}, Primaries: []document.DocumentEventPrimaryV1{}, Sources: []document.DocumentEventSourceV1{}}
		raw, _, err := document.MarshalDocumentEventsV1(record)
		require.NoError(t, err)
		_, err = s.PublishDocumentEvents(ctx, target, store.DocumentEventsDeriverFingerprint, evidence.InputsSHA256, raw)
		require.NoError(t, err)
	}
	for _, id := range []string{private.CurrentVersionID, public.CurrentVersionID} {
		input, err := s.PrepareDocumentPeopleInputs(ctx, id)
		require.NoError(t, err)
		raw, err := canonical.Marshal(input)
		require.NoError(t, err)
		digest := sha256.Sum256(raw)
		_, err = s.PublishDocumentPeople(ctx, store.DocumentPeoplePublication{People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: id, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{}}, InputsSHA256: fmt.Sprintf("%x", digest), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, NodeRevision: input.NodeRevision, BindingEpoch: input.BindingEpoch})
		require.NoError(t, err)
	}
	browser := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts), "If-Match": `"1"`}
	for _, tc := range []struct{ name, payload, message string }{
		{"concealed", `{"syntax":"advanced","text":"bcc:\"Matrix Concealed\""}`, "bcc: is not available with safe people disclosure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, _ := createSavedQuery(t, ts.URL, "Matrix "+tc.name, tc.payload)
			nested := fmt.Sprintf(`{"syntax":"advanced","text":%q}`, `saved:"Matrix `+tc.name+`"`)
			exportQuery := fmt.Sprintf(`{"v":1,"syntax":"advanced","mode":"lexical","filters":{},"sort":{"field":"name","direction":"asc"},"text":%q}`, `saved:"Matrix `+tc.name+`"`)
			routes := []struct{ name, path, body string }{
				{"parse", queryParsePath, tc.payload},
				{"highlights", queryHighlightsPath, nested},
				{"workspace", "/api/v1/workspace/queries", `{"query":` + tc.payload + `}`},
				{"saved run", "/api/v1/saved-queries/" + saved.ID + "/runs", `{}`},
				{"query export", "/api/v1/exports/sources", `{"operation_id":"33333333-3333-4333-8333-333333333333","kind":"query","query":` + exportQuery + `}`},
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
	t.Run("api key selected safe facet", func(t *testing.T) {
		request := fmt.Sprintf(`{"query":{"syntax":"advanced","text":"name:private","filters":{"people":[{"person_id":%q,"roles":["blind_copy"]},{"person_id":%q,"roles":["blind_copy"]}]}},"facets":["person_role"]}`, visible.PersonID, concealed.PersonID)
		resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": testAPIKey}, request)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var snapshot api.WorkspaceQueryResponse
		require.NoError(t, json.Unmarshal([]byte(body), &snapshot))
		require.EqualValues(t, 1, snapshot.Total)
		require.Len(t, snapshot.Rows, 1)
		require.Equal(t, private.CurrentVersionID, snapshot.Rows[0].ContentVersionID)
		require.Len(t, snapshot.Facets, 1)
		facet := snapshot.Facets[0]
		require.Equal(t, "person_role", facet.Dimension)
		require.True(t, facet.Available)
		require.NotNil(t, facet.Total)
		require.NotNil(t, facet.Missing)
		require.NotNil(t, facet.Other)
		require.EqualValues(t, 1, *facet.Total)
		require.EqualValues(t, 1, *facet.Missing)
		require.Zero(t, *facet.Other)
		require.Len(t, facet.Values, 1)
		require.Equal(t, visible.PersonID, facet.Values[0].Key)
		require.Equal(t, "Matrix Visible", facet.Values[0].Label)
		require.True(t, facet.Values[0].Selected)
		require.Zero(t, facet.Values[0].Count)
		require.Empty(t, facet.Values[0].Roles)
	})
	// The legacy person operand keeps its any-role meaning in both scopes.
	payload := `{"syntax":"advanced","text":"person:\"Matrix Visible\""}`
	exportQuery := `{"v":1,"syntax":"advanced","mode":"lexical","filters":{},"sort":{"field":"name","direction":"asc"},"text":"person:\"Matrix Visible\""}`
	saved, _ := createSavedQuery(t, ts.URL, "Matrix public", payload)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int64
	}{
		{"browser", browser, 2}, {"api key", map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, 2},
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
				fmt.Sprintf(`{"operation_id":%q,"kind":"query","query":%s}`, uuid.New().String(), exportQuery),
				fmt.Sprintf(`{"operation_id":%q,"kind":"saved_query","saved_query_id":%q,"saved_query_revision":1}`, uuid.New().String(), saved.ID),
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
