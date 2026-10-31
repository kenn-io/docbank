package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMixedCaseEmbeddedSourceSQLite(t *testing.T) {
	t.Parallel()
	checkMixedCaseEmbeddedSource(t, newTestStore(t))
}

func TestMixedCaseEmbeddedSourcePostgres(t *testing.T) {
	t.Parallel()
	checkMixedCaseEmbeddedSource(t, newPostgresQueryStore(t))
}

func checkMixedCaseEmbeddedSource(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	// Mixed-case kinds are accepted retained provenance, not operational imports.
	embedded, err := s.BeginIngest(ctx, "Embedded:agent", "Application evidence")
	require.NoError(t, err)
	evidence, err := s.IngestFileExact(ctx, embedded, s.RootID(), "evidence.jpg", fakeHash("a1"), 4, "image/jpeg", "/opaque/evidence.jpg", "")
	require.NoError(t, err)
	operational, err := s.BeginIngest(ctx, "cli", "Filesystem import")
	require.NoError(t, err)
	photo, err := s.IngestFileExact(ctx, operational, s.RootID(), "photo.jpg", fakeHash("b2"), 8, "image/jpeg", "/source/photo.jpg", "")
	require.NoError(t, err)

	collections, total, err := s.Collections(ctx, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	if assert.Len(t, collections, 1) {
		assert.Equal(t, operational.ID(), collections[0].ID)
	}
	_, err = s.CollectionByID(ctx, embedded.ID())
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.CollectionMembers(ctx, embedded.ID(), 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.CollectionLabel(ctx, embedded.ID())
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.SetCollectionLabel(ctx, embedded.ID(), 1, new("Evidence"))
	require.ErrorIs(t, err, ErrNotFound)
	label, err := s.SetCollectionLabel(ctx, operational.ID(), 1, new("Photos"))
	require.NoError(t, err)
	assert.Equal(t, new("Photos"), label.Label)

	var selected sql.NullString
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE node_id=$1`, evidence.ID).Scan(&selected))
	assert.False(t, selected.Valid, "caller evidence cannot identify a filesystem photo source")
	bindings, err := s.ProvenanceVersionBindings(ctx, photo.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT source_provenance_identity FROM photo_files WHERE node_id=$1`, photo.ID).Scan(&selected))
	assert.Equal(t, sql.NullString{String: bindings[0].ProvenanceIdentity, Valid: true}, selected)
	facts, err := s.NodeProvenance(ctx, evidence.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, facts.Items, 1)
	assert.Equal(t, "agent", facts.Items[0].SourceKind)
	assert.Equal(t, "/opaque/evidence.jpg", facts.Items[0].OriginalPath)

	// Validation must also reject the state the old PostgreSQL writes admitted.
	require.NoError(t, validateCollectionLabelMetadataState(ctx, s.db))
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO collection_labels(ingest_id,label,revision,updated_at) VALUES($1,'Evidence',1,$2)`, embedded.ID(), embedded.record.StartedAt); err != nil {
			return err
		}
		// A retained label must not bypass either collection's eligibility check.
		_, err := collectionSummaryByID(ctx, tx, embedded.ID())
		require.ErrorIs(t, err, ErrNotFound)
		_, _, err = collectionLabelTx(ctx, tx, embedded.ID())
		require.ErrorIs(t, err, ErrNotFound)
		return validateCollectionLabelMetadataState(ctx, tx)
	})
	require.ErrorContains(t, err, "collection label references a caller-supplied ingest")
	require.NoError(t, validatePhotoMetadataState(ctx, s.db, currentStorageSchemaVersion, s.postgresSchema != ""))
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET source_provenance_identity=$1 WHERE node_id=$2`, facts.Items[0].Identity, evidence.ID); err != nil {
			return err
		}
		return validatePhotoMetadataState(ctx, tx, currentStorageSchemaVersion, s.postgresSchema != "")
	})
	require.ErrorContains(t, err, "invalid photo source provenance reference")
}
