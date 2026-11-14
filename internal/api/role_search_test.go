package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
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
	person, err := s.CreatePerson(t.Context(), "Matrix Concealed", "operator")
	require.NoError(t, err)
	hash, size, err := s.Blobs.WriteContext(t.Context(), strings.NewReader("synthetic letter"))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), "letter.txt", hash, size, "text/plain")
	require.NoError(t, err)
	_, err = s.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: person.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
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
			require.Contains(t, body, "bcc searches are unavailable here")
		})
	}
	t.Run("person retains its existing meaning", func(t *testing.T) {
		resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", browser, `{"query":{"syntax":"advanced","text":"person:\"Matrix Concealed\""},"page_size":50}`)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.WorkspaceQueryResponse
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Rows, 1)
		require.Equal(t, "letter.txt", page.Rows[0].Name)
	})
	t.Run("people evidence wire fields", func(t *testing.T) {
		visible, err := s.CreatePerson(t.Context(), "Matrix Sender", "operator")
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: visible.PersonID, Role: "sender", Action: "assert", Revision: 1})
		require.NoError(t, err)
		_, err = s.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: node.CurrentVersionID, PersonID: visible.PersonID, Role: "author", Action: "assert", Revision: 1})
		require.NoError(t, err)
		ctx := t.Context()
		require.NoError(t, s.EnsureDocumentEventRecipe(ctx, store.DocumentEventsDeriverFingerprint))
		targets, err := s.MissingDocumentEventTargetsAfter(ctx, store.DocumentEventsDeriverFingerprint, "", 100)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		target := targets[0]
		evidence, err := s.LoadDocumentEventEvidence(ctx, target)
		require.NoError(t, err)
		record := document.DocumentEventsV1{VaultUID: s.VaultID(), ContentVersionID: target.ContentVersionID, ContractVersion: document.DocumentEventsContractV1, DocumentKind: store.DocumentEventKindForMIMEType(target.MIMEType), Diagnostics: []document.DocumentEventDiagnosticV1{}, Events: []document.DocumentEventV1{}, Primaries: []document.DocumentEventPrimaryV1{}, Sources: []document.DocumentEventSourceV1{}}
		raw, _, err := document.MarshalDocumentEventsV1(record)
		require.NoError(t, err)
		_, err = s.PublishDocumentEvents(ctx, target, store.DocumentEventsDeriverFingerprint, evidence.InputsSHA256, raw)
		require.NoError(t, err)
		input, err := s.PrepareDocumentPeopleInputs(ctx, node.CurrentVersionID)
		require.NoError(t, err)
		raw, err = canonical.Marshal(input)
		require.NoError(t, err)
		digest := sha256.Sum256(raw)
		_, err = s.PublishDocumentPeople(ctx, store.DocumentPeoplePublication{People: document.DocumentPeopleV1{ContractVersion: document.PersonContractV1, ContentVersionID: node.CurrentVersionID, EventGenerationID: input.EventGenerationID, Edges: []document.DocumentPersonEdgeV1{}}, InputsSHA256: hex.EncodeToString(digest[:]), ResolverFingerprint: document.PersonResolverFingerprint(), NodeID: input.NodeID, NodeRevision: input.NodeRevision, BindingEpoch: input.BindingEpoch})
		require.NoError(t, err)
		request := fmt.Sprintf(`{"query":{"syntax":"advanced","text":"from:\"Matrix Sender\"","filters":{"people":[{"person_id":%q,"roles":["sender"]},{"person_id":%q,"roles":["author"]}]}},"facets":["person_role"]}`, visible.PersonID, visible.PersonID)
		resp, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/workspace/queries", browser, request)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.WorkspaceQueryResponse
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Rows, 1)
		require.Equal(t, []string{"author", "sender"}, page.Rows[0].MatchedRoles)
		require.Len(t, page.Facets, 1)
		require.Len(t, page.Facets[0].Values, 1)
		require.Equal(t, []string{"author", "sender"}, page.Facets[0].Values[0].Roles)
		require.Equal(t, []string{visible.PersonID}, page.Facets[0].Values[0].SelectedPersonIDs)
	})
}
