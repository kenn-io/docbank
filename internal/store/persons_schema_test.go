package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentPeopleStateSurvivesReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "docbank.db")
	s, err := Open(t.Context(), path)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE document_people_state SET binding_epoch=7 WHERE singleton=1`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reopened, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	var binding int64
	require.NoError(t, reopened.db.QueryRowContext(t.Context(), `SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&binding))
	require.EqualValues(t, 7, binding)
}

func TestPristineMetadataTargetRequiresUntouchedPeopleState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return requirePristineMetadataTarget(t.Context(), tx)
	}))
	_, err := s.db.ExecContext(t.Context(), `UPDATE document_people_state SET binding_epoch=2 WHERE singleton=1`)
	require.NoError(t, err)
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return requirePristineMetadataTarget(t.Context(), tx)
	})
	require.ErrorContains(t, err, "not pristine")
}

func seedPeopleVersion(t *testing.T, s *Store) (int64, string) {
	t.Helper()
	name, err := newUUIDv4()
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), s.RootID(), name+".txt", strings.Repeat("a", 64), 1, "text/plain")
	require.NoError(t, err)
	return node.ID, node.CurrentVersionID
}
