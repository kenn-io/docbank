package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestNoteContextRoutePendingAndHistoricalVersion(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	scan, err := catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
		Name: "notebook", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String(),
	})
	require.NoError(t, err)
	receipt, err := catalog.AcceptPublishedNote(t.Context(), store.NotePublicationUploadRequest{
		Name: "notebook", ScanID: scan.ScanID, SourceRef: "note.md", FileName: "note.md", ParentPath: "/notes",
		OperationID: uuid.NewV4().String(), Hash: strings.Repeat("b", 64), Size: 4,
	}, store.BlobPhysical{Encoding: "raw", StoredBytes: 4, PackEligible: true, Created: true})
	require.NoError(t, err)
	_, err = catalog.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{
		Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true,
	})
	require.NoError(t, err)
	route := "/api/v1/notes/context?publication=notebook&source_ref=note.md&direction=outgoing"
	response, _ := do(t, ts, http.MethodGet, route, map[string]string{"X-Api-Key": ""}, nil)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	response, body := do(t, ts, http.MethodGet, route, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.NoteContextPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, receipt.Source.Identity.ContentVersionID, page.Source.ContentVersionID)
	require.Equal(t, "pending", page.Readiness.Parse.State)
	require.Equal(t, "pending", page.Readiness.Graph.State)
	require.Nil(t, page.Readiness.Coverage)
	require.Empty(t, page.Items)
	response, body = do(t, ts, http.MethodGet, route+"&content_version_id="+uuid.NewV4().String(), nil, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "note_graph_unavailable")
}
