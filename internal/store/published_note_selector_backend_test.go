package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func checkPublishedNoteIdentitiesSQLite(t *testing.T) {
	t.Helper()
	checkPublishedNoteIdentities(t, newTestStore(t))
}
func checkPublishedNoteIdentitiesPostgres(t *testing.T) {
	t.Helper()
	checkPublishedNoteIdentities(t, newPostgresQueryStore(t))
}

// Identity order is a native metadata operation in both engines. Query
// execution remains outside the PostgreSQL qualification boundary; SQLite
// selector paging has its own tests. Manual replacement fails this exact fence.
func checkPublishedNoteIdentities(t *testing.T, s *Store) {
	t.Helper()
	scan := beginNoteScan(t, s)
	a := acceptNote(t, s, noteUpload(t, scan, "a.md", fakeHash("ab"), 0)).Source.Identity
	b := acceptNote(t, s, noteUpload(t, scan, "b.md", fakeHash("cd"), 0)).Source.Identity
	completeNoteScan(t, s, scan)
	ids := []DocumentCatalogIdentity{{NodeID: b.NodeID, ContentVersionID: b.ContentVersionID}, {NodeID: a.NodeID, ContentVersionID: a.ContentVersionID}}
	for _, selector := range []PublishedNoteSelector{{}, {Publication: "notebook"}} {
		got, err := s.ResolvePublishedNoteIdentities(t.Context(), selector, ids)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "b.md", got[0].SourceRef)
		require.Equal(t, "a.md", got[1].SourceRef)
	}
	node, err := s.NodeByID(t.Context(), a.NodeID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(t.Context(), a.NodeID, node.Revision, a.Hash, a.Size, "text/markdown")
	require.NoError(t, err)
	_, err = s.ResolvePublishedNoteIdentities(t.Context(), PublishedNoteSelector{}, ids)
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion)
}
