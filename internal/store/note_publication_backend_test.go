package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func checkNotePublicationScanSQLite(t *testing.T) {
	t.Parallel()
	checkNotePublicationScanBackend(t, newTestStore(t))
}

func checkNotePublicationScanPostgres(t *testing.T) {
	t.Parallel()
	checkNotePublicationScanBackend(t, newPostgresQueryStore(t))
}

func checkNotePublicationScanBackend(t *testing.T, s *Store) {
	t.Helper()
	scan := beginNoteScan(t, s)
	request := noteUpload(t, scan, "nested/note.md", fakeHash("bc"), 0)
	first := acceptNote(t, s, request)
	_, err := s.AcceptPublishedNote(t.Context(), noteUpload(t, scan, "nested/note.md", fakeHash("cd"), 0))
	require.ErrorIs(t, err, ErrPublicationSourceStale)
	replayed, err := s.AcceptPublishedNote(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, first, replayed)
	before := completeNoteScan(t, s, scan)
	scan = beginNoteScan(t, s)
	page, err := s.NotePublicationSources(t.Context(), NotePublicationSourcesRequest{Name: "notebook", ScanID: scan.ScanID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	_, err = s.AcknowledgePublishedNotes(t.Context(), NotePublicationSeenRequest{Name: "notebook", ScanID: scan.ScanID, Bindings: []PublishedNoteIdentity{page.Items[0].Identity}})
	require.NoError(t, err)
	require.Equal(t, before.NamespaceRevision, completeNoteScan(t, s, scan).NamespaceRevision)
	status := completeNoteScan(t, s, beginNoteScan(t, s))
	require.EqualValues(t, 1, status.Absent)
	require.EqualValues(t, 0, status.Present)
}
