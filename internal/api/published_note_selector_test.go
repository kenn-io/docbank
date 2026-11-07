package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPublishedNoteSearchListAndCursor(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, configureProcessingTestService(t))
	scan, err := s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "notebook", Destination: "/notes", PolicyFingerprint: testHash("policy"), OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	var accepted store.NotePublicationReceipt
	for _, name := range []string{"alpha.md", "beta.md"} {
		receipt, err := s.AcceptPublishedNote(t.Context(), store.NotePublicationUploadRequest{Name: "notebook", ScanID: scan.ScanID, SourceRef: "folder/" + name, FileName: name, ParentPath: "/notes/folder", OperationID: uuid.NewV4().String(), Hash: testHash(name), Size: 4}, store.BlobPhysical{Encoding: "raw", StoredBytes: 4, PackEligible: true, Created: true})
		require.NoError(t, err)
		if name == "alpha.md" {
			accepted = receipt
		}
	}
	_, err = s.CreateFile(t.Context(), s.RootID(), "alpha-archive.md", testHash("archive"), 4, "text/markdown")
	require.NoError(t, err)
	prefix := "/api/v1/documents?page_size=1&published_notes%5Bpublication%5D=notebook"
	response, body := get(t, ts, prefix, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotNil(t, page.PublishedNotes)
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Items[0].PublishedNote)
	require.Equal(t, "folder/alpha.md", page.Items[0].PublishedNote.SourceRef)
	require.NotEmpty(t, page.NextCursor)
	response, body = get(t, ts, prefix+"&cursor="+url.QueryEscape(page.NextCursor), nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/documents?page_size=1&published_notes%5Bpublication%5D=&cursor="+url.QueryEscape(page.NextCursor), nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body, "all-publications must not borrow a named cursor")

	response, body = get(t, ts, "/api/v1/search?q=alpha&published_notes%5Bpublication%5D=notebook", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var legacy api.SearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &legacy))
	require.Len(t, legacy.Hits, 1)
	require.NotNil(t, legacy.Hits[0].PublishedNote)
	for _, selector := range []*api.PublishedNoteSelector{nil, {}, {Publication: "notebook"}} {
		typed, err := daemonconn.New(ts.URL, testAPIKey).ListDocuments(t.Context(), api.DocumentQuery{PublishedNotes: selector, PageSize: 1})
		require.NoError(t, err, "typed list preserves nil versus explicit empty collection")
		require.Equal(t, selector, typed.PublishedNotes)
	}
	request := api.DocumentSearchRequest{Query: "alpha", Mode: "lexical", Profile: "private", Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{PublishedNotes: &api.PublishedNoteSelector{Publication: "notebook"}}}}
	response, body = do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var report api.DocumentSearchReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.Results, 1)
	require.NotNil(t, report.Results[0].PublishedNote)
	require.Equal(t, accepted.Source.Identity.ContentVersionID, report.Results[0].ContentVersionID)
	require.Equal(t, accepted.Source.Identity.SourceRef, report.Results[0].PublishedNote.SourceRef)
	_, err = s.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
	require.NoError(t, err)
	// A new empty successful inventory retires membership and invalidates cursors.
	scan, err = s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "notebook", Destination: "/notes", PolicyFingerprint: testHash("policy"), OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	_, err = s.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
	require.NoError(t, err)
	response, body = get(t, ts, prefix+"&cursor="+url.QueryEscape(page.NextCursor), nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Empty(t, report.Results)
	request.Scope.Filters = nil
	response, body = do(t, ts, http.MethodPost, "/api/v1/search", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.Results, 2, "ordinary archive semantics survive source absence")
	for _, hit := range report.Results {
		require.Nil(t, hit.PublishedNote)
	}
}
