package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Recovery reads must not begin an inventory or change the namespace. The
// accepted original and current archive head have independent identities.
func TestNotePublicationSourceSnapshot(t *testing.T) {
	t.Parallel()
	checkNotePublicationSourceSnapshot(t, newTestStore(t))
}

func checkNotePublicationSourceSnapshot(t *testing.T, s *Store) {
	t.Helper()
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "note.md", fakeHash("ab"), 0))
	completeNoteScan(t, s, scan)
	before, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.Nil(t, before.ActiveScan)
	snapshot, err := s.NotePublicationSource(t.Context(), "notebook", "note.md")
	require.NoError(t, err)
	require.Equal(t, scan.DatabaseEpoch, snapshot.DatabaseEpoch)
	require.Equal(t, a.Source.Identity, snapshot.Source.Identity)
	after, err := s.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	require.Equal(t, before, after)
	node, err := s.NodeByID(t.Context(), a.Source.Identity.NodeID)
	require.NoError(t, err)
	_, manual, err := s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("bc"), 4, "text/markdown")
	require.NoError(t, err)
	snapshot, err = s.NotePublicationSource(t.Context(), "notebook", "note.md")
	require.NoError(t, err)
	require.Equal(t, "diverged", snapshot.Source.Conflict)
	require.Equal(t, manual.ID, snapshot.Source.ArchiveHead.ContentVersionID)
	require.Equal(t, a.Source.Identity.ContentVersionID, snapshot.Source.Identity.ContentVersionID)
	grant, err := s.AcceptLocalPublishedNote(t.Context(), NotePublicationAcceptLocalRequest{
		Name: "notebook", SourceRef: "note.md", DatabaseEpoch: snapshot.DatabaseEpoch,
		ExpectedHeadVersionID: manual.ID, ExpectedSourceRevision: snapshot.Source.Identity.SourceRevision, OperationID: noteOperationID(t),
	})
	require.NoError(t, err)
	snapshot, err = s.NotePublicationSource(t.Context(), "notebook", "note.md")
	require.NoError(t, err)
	require.Equal(t, &grant, snapshot.Source.Recovery)
	completeNoteScan(t, s, beginNoteScan(t, s))
	snapshot, err = s.NotePublicationSource(t.Context(), "notebook", "note.md")
	require.NoError(t, err)
	require.Equal(t, "absent", snapshot.Source.Presence)
	require.Equal(t, a.Source.Identity.ContentVersionID, snapshot.Source.Identity.ContentVersionID)
	node, err = s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), node.ID, node.Revision, VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	snapshot, err = s.NotePublicationSource(t.Context(), "notebook", "note.md")
	require.NoError(t, err)
	require.Equal(t, "unavailable", snapshot.Source.Availability)
	require.Equal(t, a.Source.Identity.ContentVersionID, snapshot.Source.Identity.ContentVersionID)
	require.Equal(t, manual.ID, snapshot.Source.ArchiveHead.ContentVersionID)
	for _, pair := range [][2]string{{"notebook", "missing.md"}, {"unknown", "note.md"}} {
		got, err := s.NotePublicationSource(t.Context(), pair[0], pair[1])
		require.ErrorIs(t, err, ErrNotFound)
		require.Zero(t, got)
	}
	for _, pair := range [][2]string{{"INVALID", "note.md"}, {"notebook", "../escape.md"}, {"notebook", ""}} {
		got, err := s.NotePublicationSource(t.Context(), pair[0], pair[1])
		require.Error(t, err)
		require.Zero(t, got)
	}
}

func checkNotePublicationSourceSnapshotSQLite(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNotePublicationSourceSnapshot(t, newTestStore(t))
}

func checkNotePublicationSourceSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNotePublicationSourceSnapshot(t, newPostgresQueryStore(t))
}
