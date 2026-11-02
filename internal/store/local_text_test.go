package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestMissingProcessingTargetsPagesCurrentLiveVersions(t *testing.T) {
	t.Parallel()
	checkMissingProcessingTargets(t, newTestStore(t))
}

func TestMissingProcessingTargetsPostgres(t *testing.T) {
	t.Parallel()
	checkMissingProcessingTargets(t, newPostgresQueryStore(t))
}

func checkMissingProcessingTargets(t *testing.T, s *Store) {
	t.Helper()
	directory, err := s.Mkdir(t.Context(), s.RootID(), "temporary")
	require.NoError(t, err)
	for i := range 3 {
		_, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("text-%02d.txt", i), fakeHash("a1"), 12, "text/plain")
		require.NoError(t, err)
	}
	hidden, err := s.CreateFile(t.Context(), directory.ID, "hidden.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	directory, err = s.NodeByID(t.Context(), directory.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), directory.ID, directory.Revision)
	require.NoError(t, err)
	first, err := s.MissingProcessingTargetsAfter(t.Context(), fakeHash("f1"), "", 1, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	last, err := s.MissingProcessingTargetsAfter(t.Context(), fakeHash("f1"), first[1].ContentVersionID, 1, 2)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Less(t, first[0].ContentVersionID, first[1].ContentVersionID)
	require.Less(t, first[1].ContentVersionID, last[0].ContentVersionID)
	for _, item := range append(first, last...) {
		require.NotEqual(t, hidden.CurrentVersionID, item.ContentVersionID)
		node, err := s.NodeByID(t.Context(), item.NodeID)
		require.NoError(t, err)
		require.Equal(t, node.Name, item.Name)
	}
	node, err := s.NodeByID(t.Context(), last[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.Move(t.Context(), node.ID, *node.ParentID, "renamed.pdf", node.Revision)
	require.NoError(t, err)
	last, err = s.MissingProcessingTargetsAfter(t.Context(), fakeHash("f1"), first[1].ContentVersionID, 1, 2)
	require.NoError(t, err)
	require.Equal(t, "renamed.pdf", last[0].Name)
	require.Equal(t, node.CurrentVersionID, last[0].ContentVersionID)
	key := EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID, BindingID: "local-text", InputKind: document.EmbeddingInputRenditionChunk}
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO derivative_purge_suppressions(source_sha256,profile_fingerprint,build_id,purged_at,active) VALUES($1,$2,$3,$4,1)`, fakeHash("a1"), derivativeBuildSuppressionProfile, embeddingPurgeScope(key, fakeHash("f1")), "2026-01-01T00:00:00.000000000Z")
	require.NoError(t, err)
	suppressed, err := s.EmbeddingPurgeSuppressedVersions(t.Context(), fakeHash("f1"), []EmbeddingHeadKey{key})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{node.CurrentVersionID: true}, suppressed)
	suppressed, err = s.EmbeddingPurgeSuppressedVersions(t.Context(), fakeHash("f2"), []EmbeddingHeadKey{key})
	require.NoError(t, err)
	require.Empty(t, suppressed, "another profile has its own suppression identity")
	require.NoError(t, s.AuthorizeEmbeddingRebuild(t.Context(), EmbeddingRebuildAuthorization{Key: key, ProcessingProfileFingerprint: fakeHash("f1"), SetID: fakeHash("f3"), AuthorizedAt: "2026-01-02T00:00:00.000000000Z"}))
	suppressed, err = s.EmbeddingPurgeSuppressedVersions(t.Context(), fakeHash("f1"), []EmbeddingHeadKey{key})
	require.NoError(t, err)
	require.Empty(t, suppressed)
}
