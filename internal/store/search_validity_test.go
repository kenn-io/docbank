package store

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkSearchValiditySQLite(t *testing.T) {
	t.Helper()
	checkSearchValidity(t, newTestStore(t), nil)
}

func checkSearchValidityPostgres(t *testing.T) {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	checkSearchValidity(t, s, func(old, next int64) {
		_, err := setup.ExecContext(t.Context(), `INSERT INTO documents.node_supersessions(successor_node_id,predecessor_node_id,note,recorded_at) VALUES($1,$2,'','2025-01-01T00:00:00Z')`, next, old)
		require.NoError(t, err)
		_, err = setup.ExecContext(t.Context(), `INSERT INTO documents.node_effective_windows(node_id,effective_from,effective_to) VALUES($1,'2025-01-01',NULL)`, next)
		require.NoError(t, err)
	})
}

func checkSearchValiditySnapshotPostgres(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	scope, err := s.Mkdir(ctx, s.RootID(), "leases")
	require.NoError(t, err)
	old, err := s.CreateFile(ctx, scope.ID, "old.txt", fakeHash("old"), 1, "text/plain")
	require.NoError(t, err)
	next, err := s.CreateFile(ctx, scope.ID, "new.txt", fakeHash("new"), 1, "text/plain")
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, `INSERT INTO documents.node_supersessions(successor_node_id,predecessor_node_id,note,recorded_at) VALUES($1,$2,'','2025-01-01T00:00:00Z')`, next.ID, old.ID)
	require.NoError(t, err)

	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var config *pgx.ConnConfig
	err = conn.Raw(func(driverConn any) error {
		pgConn, ok := driverConn.(*stdlib.Conn)
		require.True(t, ok)
		config = pgConn.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	var moveErr error
	var movedPath string
	// Commit the move after eligibility is read, before successor paths are read.
	config.Tracer = &afterFirstSelect{run: func() {
		_, movedPath, moveErr = s.Move(ctx, next.ID, s.RootID(), "new.txt", next.Revision)
	}}
	readDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, readDB.Close()) })
	reader := &Store{db: readDB, rootID: s.rootID, postgresSchema: s.postgresSchema}
	filter := SearchOptions{UnderNodeID: scope.ID}
	got, err := reader.SearchResultValidity(ctx, filter, []int64{old.ID})
	require.NoError(t, moveErr)
	require.Equal(t, "/new.txt", movedPath)
	require.NoError(t, err)
	require.Equal(t, []SearchValiditySuccessor{{NodeID: next.ID, Path: "/leases/new.txt"}}, got[old.ID].Successors)

	after, err := s.SearchResultValidity(ctx, filter, []int64{old.ID})
	require.NoError(t, err)
	require.Empty(t, after[old.ID].Successors)
}

func checkSearchValidity(t *testing.T, s *Store, seed func(int64, int64)) {
	t.Helper()
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "leases")
	require.NoError(t, err)
	old, err := s.CreateFile(ctx, scope.ID, "old.txt", fakeHash("old"), 1, "text/plain; charset=utf-8")
	require.NoError(t, err)
	next, err := s.CreateFile(ctx, s.RootID(), "new.txt", fakeHash("new"), 1, "text/plain")
	require.NoError(t, err)
	if seed != nil {
		seed(old.ID, next.ID)
	} else {
		_, err = s.PutSupersession(ctx, next.ID, old.ID, old.Revision, new(""))
		require.NoError(t, err)
		_, err = s.PutEffectiveWindow(ctx, next.ID, next.Revision, "2025-01-01", "")
		require.NoError(t, err)
	}
	ids := []int64{old.ID, next.ID}
	got, err := s.SearchResultValidity(ctx, SearchOptions{}, ids)
	require.NoError(t, err)
	require.True(t, got[old.ID].Declared)
	require.Equal(t, []SearchValiditySuccessor{{NodeID: next.ID, Path: "/new.txt", EffectiveFrom: "2025-01-01"}}, got[old.ID].Successors)
	require.Equal(t, "2025-01-01", got[next.ID].Window.From)
	for _, filter := range []SearchOptions{{ContentVersionIDs: []string{old.CurrentVersionID}}, {UnderNodeID: scope.ID}} {
		got, err = s.SearchResultValidity(ctx, filter, ids)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Empty(t, got[old.ID].Successors)
		require.False(t, got[old.ID].Declared)
	}
	next, err = s.NodeByID(ctx, next.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, next.ID, next.Revision)
	require.NoError(t, err)
	got, err = s.SearchResultValidity(ctx, SearchOptions{}, ids)
	require.NoError(t, err)
	require.Empty(t, got[old.ID].Successors)
}
