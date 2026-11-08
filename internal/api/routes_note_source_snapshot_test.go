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

func TestNotePublicationCurrentSourceRoute(t *testing.T) {
	t.Parallel()
	s, _ := newTestAPIServer(t, nil)
	scan, err := s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	response := notePublicationUpload(t, s, scan.ScanID, 0, uuid.NewV4().String(), "# Alpha\n", uploadIdentity([]byte("# Alpha\n")), false)
	require.Equal(t, 201, response.Code, response.Body.String())
	_, err = s.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{Name: "research", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true})
	require.NoError(t, err)
	before, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	path := "/api/v1/note-publications/research/sources/current?source_ref=alpha.md"
	response = notePublicationJSON(t, s, http.MethodGet, path, nil)
	require.Equal(t, 200, response.Code, response.Body.String())
	var snapshot api.NotePublicationSourceSnapshot
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))
	require.Equal(t, scan.DatabaseEpoch, snapshot.DatabaseEpoch)
	require.Equal(t, "alpha.md", snapshot.Source.Identity.SourceRef)
	response = serveLoopback(t.Context(), s.Server.Handler(), http.MethodGet, path, nil, nil)
	require.Equal(t, 401, response.Code)
	for _, query := range []string{"", "?source_ref=../escape", "?source_ref=" + strings.Repeat("x", 4097)} {
		response = notePublicationJSON(t, s, http.MethodGet, "/api/v1/note-publications/research/sources/current"+query, nil)
		require.Equal(t, 422, response.Code, response.Body.String())
	}
	response = notePublicationJSON(t, s, http.MethodGet, "/api/v1/note-publications/research/sources/current?source_ref=missing.md", nil)
	require.Equal(t, 404, response.Code, response.Body.String())
	after, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.Equal(t, before, after)
}
