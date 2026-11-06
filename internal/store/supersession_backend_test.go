package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkSupersessionMetadataGraphValidationPostgres(t *testing.T) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	s := newPostgresSchemaStore(t, setup, cfg)
	ctx := t.Context()
	source := newTestStore(t)

	predecessor, err := source.CreateFile(ctx, source.RootID(), "before.txt", fakeHash("beef"), 1, "text/plain")
	require.NoError(t, err)
	successor, err := source.CreateFile(ctx, source.RootID(), "after.txt", fakeHash("cafe"), 1, "text/plain")
	require.NoError(t, err)
	_, err = source.PutSupersession(ctx, successor.ID, predecessor.ID, predecessor.Revision, new("replacement"))
	require.NoError(t, err)
	_, err = source.PutEffectiveWindow(ctx, successor.ID, successor.Revision, "2025-01-01", "")
	require.NoError(t, err)
	copyStoreCatalogFixture(t, source, setup, postgresEnrollmentCatalogs)

	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		high, err := s.auditNodeSequence(ctx, tx)
		if err != nil {
			return err
		}
		return s.validateMetadataState(ctx, tx, high)
	}))

	// A reverse link creates a cycle that retained-state validation must reject.
	_, err = setup.ExecContext(ctx, `INSERT INTO documents.node_supersessions(successor_node_id,predecessor_node_id,note,recorded_at) VALUES($1,$2,$3,$4)`,
		predecessor.ID, successor.ID, "", nowRFC3339())
	require.NoError(t, err)
	require.ErrorIs(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		high, err := s.auditNodeSequence(ctx, tx)
		if err != nil {
			return err
		}
		return s.validateMetadataState(ctx, tx, high)
	}), ErrInvalidSupersession)
}
