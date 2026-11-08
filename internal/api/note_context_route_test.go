package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
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

func TestNoteContextRoutePagesAndRejectsStaleCursor(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	scan, err := catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
		Name: "notebook", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String(),
	})
	require.NoError(t, err)
	type item struct {
		ref   string
		body  string
		note  notemarkdown.Note
		parse store.NoteParseRecord
	}
	items := []item{{ref: "source.md", body: "[[target]] [[target]]"}, {ref: "target.md", body: "# Target\n"}}
	for i := range items {
		item := &items[i]
		sum := sha256.Sum256([]byte(item.body))
		receipt, err := catalog.AcceptPublishedNote(t.Context(), store.NotePublicationUploadRequest{
			Name: "notebook", ScanID: scan.ScanID, SourceRef: item.ref, FileName: item.ref, ParentPath: "/notes",
			OperationID: uuid.NewV4().String(), Hash: hex.EncodeToString(sum[:]), Size: int64(len(item.body)),
		}, store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(item.body)), PackEligible: true, Created: true})
		require.NoError(t, err)
		item.note, err = notemarkdown.Parse(t.Context(), []byte(item.body))
		require.NoError(t, err)
		artifact, err := notemarkdown.CanonicalArtifact(t.Context(), item.note)
		require.NoError(t, err)
		checksum := sha256.Sum256(artifact)
		item.parse = store.NoteParseRecord{ContentVersionID: receipt.Source.Identity.ContentVersionID,
			Hash: hex.EncodeToString(sum[:]), Size: int64(len(item.body)), Recipe: notemarkdown.Recipe,
			State: "ready", Checksum: hex.EncodeToString(checksum[:]), Artifact: artifact}
		require.NoError(t, catalog.RecordNoteParse(t.Context(), item.parse))
	}
	_, err = catalog.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{
		Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true,
	})
	require.NoError(t, err)
	status, err := catalog.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	sources, err := catalog.EligibleNoteSources(t.Context(), "notebook", "", 10)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	generation, err := catalog.BeginNoteIndex(t.Context(), store.NoteIndexTarget{Name: "notebook", DatabaseEpoch: status.DatabaseEpoch, NamespaceRevision: status.NamespaceRevision}, notemarkdown.Recipe, notemarkdown.ResolverRecipe)
	require.NoError(t, err)
	for i := range items {
		require.NoError(t, catalog.StageNoteIndexSource(t.Context(), generation, sources[i], items[i].parse, store.NoteIndexKeys(items[i].ref, items[i].note)))
	}
	require.NoError(t, catalog.SealNoteIndexKeys(t.Context(), generation))
	edges := []store.NoteIndexEdge{{Ordinal: 0, Link: items[0].note.Links[0], State: "resolved", Target: &sources[1].Identity},
		{Ordinal: 1, Link: items[0].note.Links[1], State: "resolved", Target: &sources[1].Identity}}
	require.NoError(t, catalog.StageNoteIndexEdges(t.Context(), generation, "source.md", edges))
	require.NoError(t, catalog.StageNoteIndexEdges(t.Context(), generation, "target.md", nil))
	require.NoError(t, catalog.PublishNoteIndex(t.Context(), generation))
	route := "/api/v1/notes/context?publication=notebook&source_ref=source.md&direction=outgoing&limit=1"
	response, body := do(t, ts, http.MethodGet, route, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var first api.NoteContextPage
	require.NoError(t, json.Unmarshal([]byte(body), &first))
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.NextCursor)
	response, body = do(t, ts, http.MethodGet, route+"&cursor="+url.QueryEscape(first.NextCursor), nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var second api.NoteContextPage
	require.NoError(t, json.Unmarshal([]byte(body), &second))
	require.Len(t, second.Items, 1)
	require.NotEqual(t, first.Items[0].Key, second.Items[0].Key)
	require.Empty(t, second.NextCursor)
	changed, err := catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "notebook", Destination: "/notes", PolicyFingerprint: scan.PolicyFingerprint, OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	_, err = catalog.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{Name: "notebook", ScanID: changed.ScanID, PolicyFingerprint: changed.PolicyFingerprint, InventoryComplete: true})
	require.NoError(t, err)
	response, body = do(t, ts, http.MethodGet, route+"&cursor="+url.QueryEscape(first.NextCursor), nil, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "note_context_stale")
}
