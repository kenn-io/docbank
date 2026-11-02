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
