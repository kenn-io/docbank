package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func checkEmbeddingWriteRevision(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s := open(t)
	_, unsubscribe := s.SubscribePhotoEvents()
	defer unsubscribe()
	revision := s.WriteRevision()
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		var count int
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM nodes`).Scan(&count)
	}))
	require.Equal(t, revision, s.WriteRevision(), "read-only transactions leave the vault idle")
	rollback := errors.New("synthetic rollback")
	require.ErrorIs(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET name=name WHERE id=$1`, s.RootID())
		require.NoError(t, err)
		return rollback
	}), rollback)
	require.Equal(t, revision, s.WriteRevision(), "rolled-back writes leave the vault idle")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE nodes SET name=name WHERE id=$1`, s.RootID())
		return err
	}))
	require.Greater(t, s.WriteRevision(), revision)
	revision = s.WriteRevision()
	_, _, found, err := s.ClaimNextEmbeddingWork(t.Context(), "idle-worker", time.Now(), time.Minute, []string{embeddingCatalogDescriptor().Fingerprint})
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, revision, s.WriteRevision(), "empty claims leave the vault idle")
}

func TestEmbeddingWriteRevisionIgnoresSearchBookkeeping(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	generation := vectorIndexGenerationFixture("reader-revision", source, []byte("index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
	revision := s.WriteRevision()
	lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, revision, s.WriteRevision(), "search acquisition keeps discovery settled")
	require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now))
	require.Equal(t, revision, s.WriteRevision(), "search release keeps discovery settled")
	_, err = s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "expired-reader", now, time.Minute)
	require.NoError(t, err)
	_, err = s.ReclaimVectorIndexGenerations(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, revision, s.WriteRevision(), "expired reader cleanup keeps discovery settled")
}

func TestEmbeddingWriteRevisionIgnoresUnavailableCoverageBookkeeping(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	coverage := []VectorIndexUnavailableCoverage{{
		VectorSpaceID: hashVectorIndexTest("space"), SourceManifestChecksum: hashVectorIndexTest("source"),
		Missing: []VectorIndexUnavailableSet{{EmbeddingSetID: hashVectorIndexTest("set"),
			VectorSetID: hashVectorIndexTest("vectors"), PayloadBlobHash: hashVectorIndexTest("blob")}},
		ExternalReembeddingRequired: true,
	}}
	revision := s.WriteRevision()
	for range 2 {
		require.NoError(t, s.ReplaceVectorIndexUnavailableCoverage(t.Context(), coverage))
		require.Equal(t, revision, s.WriteRevision(), "periodic coverage replacement keeps discovery settled")
	}
	var missing int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_index_unavailable_coverage`).Scan(&missing))
	require.Equal(t, 1, missing)
	require.NoError(t, s.ReplaceVectorIndexUnavailableCoverage(t.Context(), nil))
	require.Equal(t, revision, s.WriteRevision(), "clearing coverage keeps discovery settled")
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_index_unavailable_coverage`).Scan(&missing))
	require.Zero(t, missing)
}
