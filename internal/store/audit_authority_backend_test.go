package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestAuditStatusPostgres(t *testing.T) {
	t.Parallel()
	source, reader, publish, scopeID, child, outside := newAuditHistoryPostgresFixture(t)
	ctx := t.Context()
	dormant, err := reader.AuditStatus(ctx, new(reader.RootID()))
	require.NoError(t, err)
	require.False(t, dormant.Enabled)
	require.Equal(t, source.vaultID, dormant.VaultID)
	require.Empty(t, dormant.Scopes)
	require.False(t, dormant.Membership.Protected)
	require.Equal(t, "/", dormant.Membership.Path)
	publish()

	for _, id := range []*int64{nil, &child.ID, &outside.ID} {
		want, err := source.AuditStatus(ctx, id)
		require.NoError(t, err)
		got, err := reader.AuditStatus(ctx, id)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	byPath, err := reader.AuditStatusPath(ctx, "/Documents/renamed")
	require.NoError(t, err)
	require.Equal(t, []string{scopeID}, byPath.Membership.ScopeIDs)
	require.True(t, byPath.Membership.Protected)
	_, err = reader.AuditStatus(ctx, new(int64(999)))
	require.ErrorIs(t, err, ErrNotFound)

	_, _, err = source.Trash(ctx, child.ID, child.Revision)
	require.NoError(t, err)
	publish()
	want, err := source.AuditStatus(ctx, &child.ID)
	require.NoError(t, err)
	got, err := reader.AuditStatus(ctx, &child.ID)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.True(t, got.Membership.Trashed)
	require.True(t, got.Membership.Protected)
	require.Empty(t, got.Membership.Path)
	_, err = reader.AuditStatusPath(ctx, "/Documents/renamed")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAuditStatusSnapshotPostgres(t *testing.T) {
	t.Parallel()
	source, reader, publish, _, child, _ := newAuditHistoryPostgresFixture(t)
	publish()
	ctx := t.Context()
	before, err := reader.AuditStatusPath(ctx, "/Documents/renamed")
	require.NoError(t, err)
	conn, err := reader.db.Conn(ctx)
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
	config.Tracer = &afterFirstSelect{run: func() {
		_, _, err := source.Move(ctx, child.ID, *child.ParentID, "later", child.Revision)
		require.NoError(t, err)
		publish()
	}}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	snapshot := &Store{db: db, rootID: reader.rootID, vaultID: reader.vaultID, postgresSchema: reader.postgresSchema}
	during, err := snapshot.AuditStatusPath(ctx, "/Documents/renamed")
	require.NoError(t, err)
	require.Equal(t, before, during)
	after, err := reader.AuditStatus(ctx, &child.ID)
	require.NoError(t, err)
	require.NotEqual(t, before, after)
	want, err := source.AuditStatus(ctx, &child.ID)
	require.NoError(t, err)
	require.Equal(t, want, after)
}

func TestNodeAllocationAfterDeletePostgres(t *testing.T) {
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	const recordedAt = "2026-01-01T00:00:00.000000000Z"
	var deleted int64
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		deleted, err = s.insertNodeTx(ctx, tx, s.RootID(), "deleted", nodeKindDir, nil, recordedAt)
		return err
	}))
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id=$1`, deleted)
		return err
	}))
	sequence, err := s.auditNodeSequence(ctx, s.db)
	require.NoError(t, err)
	require.Equal(t, deleted, sequence)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		next, err := s.insertNodeTx(ctx, tx, s.RootID(), "retained", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.Equal(t, deleted+1, next)
		return nil
	}))
}

func TestAuditAuthorityAfterRollbackPostgres(t *testing.T) {
	t.Parallel()
	source, reader, publish, _, child, _ := newAuditHistoryPostgresFixture(t)
	publish()
	ctx := t.Context()
	discard := errors.New("discard new node")
	err := reader.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := reader.insertNodeTx(ctx, tx, child.ID, "rolled-back", nodeKindDir, nil, child.ModifiedAt)
		if err != nil {
			return err
		}
		return discard
	})
	require.ErrorIs(t, err, discard)
	from, err := source.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = from.Rollback() }()
	wantAuthority, wantScopes, wantSequence, err := source.loadAuditedNodeAuthority(ctx, from, child.ID)
	require.NoError(t, err)
	to, err := reader.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = to.Rollback() }()
	authority, scopes, sequence, err := reader.loadAuditedNodeAuthority(ctx, to, child.ID)
	require.NoError(t, err)
	require.Equal(t, wantAuthority, authority)
	require.Equal(t, wantScopes, scopes)
	require.Equal(t, wantSequence, sequence)
	// Replay the validated source history with PostgreSQL's allocation value.
	// Full PostgreSQL metadata validation awaits the remaining catalogs.
	require.NoError(t, validateAuditAuthority(ctx, from, source.vaultID, sequence))
}
