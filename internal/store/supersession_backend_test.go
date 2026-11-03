package store

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkSupersessionMetadataGraphValidationSQLite(t *testing.T) {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()

	predecessor, err := s.CreateFile(ctx, s.RootID(), "before.txt", fakeHash("beef"), 1, "text/plain")
	require.NoError(t, err)
	successor, err := s.CreateFile(ctx, s.RootID(), "after.txt", fakeHash("cafe"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.PutSupersession(ctx, successor.ID, predecessor.ID, predecessor.Revision, "replacement")
	require.NoError(t, err)

	require.NoError(t, s.ValidateMetadata(ctx))
}

func checkSupersessionMetadataGraphValidationPostgres(t *testing.T) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	s := newPostgresSchemaStore(t, setup, cfg)
	ctx := t.Context()

	predecessor, err := s.CreateFile(ctx, s.RootID(), "before.txt", fakeHash("beef"), 1, "text/plain")
	require.NoError(t, err)
	successor, err := s.CreateFile(ctx, s.RootID(), "after.txt", fakeHash("cafe"), 1, "text/plain")
	require.NoError(t, err)
	// PostgreSQL validates retained declaration state, but the declaration write
	// operation is not ported there. Seed it with the schema setup role.
	_, err = setup.ExecContext(ctx, `INSERT INTO documents.node_supersessions(successor_node_id,predecessor_node_id,note,recorded_at) VALUES($1,$2,$3,$4)`,
		successor.ID, predecessor.ID, "replacement", nowRFC3339())
	require.NoError(t, err)

	require.NoError(t, s.ValidateMetadata(ctx))
}
