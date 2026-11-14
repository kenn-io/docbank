package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
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
		_, err = s.PublishDocumentPeople(ctx, store.DocumentPeoplePublication{People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: id, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{}}, InputsSHA256: hex.EncodeToString(digest[:]), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, NodeRevision: input.NodeRevision, BindingEpoch: input.BindingEpoch})
		require.NoError(t, err)
	}
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
	})
	// The browser's legacy person operand retains its any-role meaning.
	resp, body = rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", browser, `{"query":{"syntax":"advanced","text":"person:\"Matrix Visible\""}}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var legacy api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &legacy))
	require.EqualValues(t, 2, legacy.Total)
}
