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
	s, err := Open(path)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE document_people_state SET binding_epoch=7 WHERE singleton=1`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	var binding int64
	require.NoError(t, reopened.db.QueryRow(`SELECT binding_epoch FROM document_people_state WHERE singleton=1`).Scan(&binding))
	require.EqualValues(t, 7, binding)
}

func TestPristineMetadataTargetRequiresUntouchedPeopleState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return requirePristineMetadataTarget(t.Context(), tx)
	}))
	_, err := s.db.Exec(`UPDATE document_people_state SET binding_epoch=2 WHERE singleton=1`)
	require.NoError(t, err)
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return requirePristineMetadataTarget(t.Context(), tx)
	})
	require.ErrorContains(t, err, "not pristine")
}

func seedPeopleVersion(t *testing.T, s *Store) (int64, string) {
	t.Helper()
	version, err := newUUIDv4()
	require.NoError(t, err)
	operation, err := newUUIDv4()
	require.NoError(t, err)
	hash := strings.Repeat("a", 64)
	var node int64
	require.NoError(t, s.withLogicalTx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO blobs(hash,size,created_at) VALUES(?,1,?)`, hash, nowRFC3339()); err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT INTO nodes(parent_id,name,kind,current_version_id,created_at,modified_at)
			VALUES(?,?,'file',?,?,?)`, s.RootID(), version+".txt", version, nowRFC3339(), nowRFC3339())
		if err != nil {
			return err
		}
		node, err = result.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,
			node_revision,introduced_operation_id,transition_kind) VALUES(?,?,?,1,'text/plain',?,1,?,'content_create')`,
			version, node, hash, nowRFC3339(), operation)
		return err
	}))
	return node, version
}
