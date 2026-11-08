package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func checkPublishedNoteProcessingCoverage(t *testing.T, makeStore func(*testing.T) *Store) {
	t.Helper()
	s := makeStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "note.md", fakeHash("ab"), 4, "text/markdown",
		BlobPhysical{Encoding: "raw", StoredBytes: 4, PackEligible: true, Created: true})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO note_publications
 (name,destination,policy_fingerprint,namespace_revision,active,last_complete_at,last_accepted_at)
 VALUES('notebook','/',$1,3,1,$2,$3)`, fakeHash("cd"), nowRFC3339(), nowRFC3339())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `INSERT INTO published_note_sources
 (publication_name,source_ref,node_id,content_version_id,blob_hash,size,source_revision,presence,availability,accepted_at)
 VALUES('notebook','note.md',$1,$2,$3,$4,2,'present','retained',$5)`, node.ID, node.CurrentVersionID, node.BlobHash, node.Size, nowRFC3339())
	require.NoError(t, err)
	snapshot, err := s.ProcessingCoverage(ctx, ProcessingCoverageScope{
		PublishedNotes: &PublishedNoteSelector{Publication: "notebook"}, ProcessingProfileFingerprint: fakeHash("ef"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, snapshot.Renditions.Total)
	require.Equal(t, 1, snapshot.Renditions.Unavailable)
	empty, err := s.ProcessingCoverage(ctx, ProcessingCoverageScope{
		PublishedNotes: &PublishedNoteSelector{Publication: "other"}, ProcessingProfileFingerprint: fakeHash("ef"),
	})
	require.NoError(t, err)
	require.Zero(t, empty.Renditions.Total)
}
