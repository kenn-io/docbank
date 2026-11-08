package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func checkNotePublicationScanSQLite(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNotePublicationScanBackend(t, newTestStore(t))
}

func checkNotePublicationScanPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNotePublicationScanBackend(t, newPostgresQueryStore(t))
}

func checkNotePublicationScanBackend(t *testing.T, s *Store) {
	t.Helper()
	scan := beginNoteScan(t, s)
	request := noteUpload(t, scan, "nested/note.md", fakeHash("bc"), 0)
	first := acceptNote(t, s, request)
	require.NoError(t, requirePublishedNoteVersionTx(t.Context(), s.db, first.Source.Identity.ContentVersionID))
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
	require.ErrorIs(t, requirePublishedNoteVersionTx(t.Context(), s.db, first.Source.Identity.ContentVersionID), ErrRenditionJobStaleAuthority)
}

func checkNotePublicationLifecycleSQLite(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNotePublicationLifecycleBackend(t, newTestStore(t))
}

func checkNotePublicationLifecyclePostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNotePublicationLifecycleBackend(t, newPostgresQueryStore(t))
}

func checkNotePublicationLifecycleBackend(t *testing.T, s *Store) {
	t.Helper()
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	node, b, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	require.Equal(t, "diverged", publishedSource(t, s, scan).Conflict)
	_, err = s.AcceptLocalPublishedNote(t.Context(), NotePublicationAcceptLocalRequest{Name: "notebook", DatabaseEpoch: scan.DatabaseEpoch, SourceRef: "note.md", ExpectedHeadVersionID: b.ID, ExpectedSourceRevision: a.Source.Identity.SourceRevision, OperationID: noteOperationID(t)})
	require.NoError(t, err)
	recovered := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), a.Source.Identity.SourceRevision))
	require.NotEqual(t, a.Source.Identity.ContentVersionID, recovered.Source.Identity.ContentVersionID)
	require.Nil(t, recovered.Source.Recovery)
	node, err = s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	node, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("cd"), 4, "text/markdown")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), node.ID, node.Revision, VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	require.Equal(t, "unavailable", publishedSource(t, s, scan).Availability)
}
