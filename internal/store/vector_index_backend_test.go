package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgstoretest"
)

func checkVectorIndexPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner, login := newTemplateDatabase(t)
	s, _, source := publishedVectorIndexFixture(t, openPostgresStore(t, owner, login))
	now := time.Now().UTC()
	first, found, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "builder\\one\x00", now, time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	prior := vectorIndexGenerationFixture("prior", source, []byte("prior"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), first, prior, now))
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), first, prior.ID, now))
	claim, found, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "next", now.Add(time.Second), time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	candidate := vectorIndexGenerationFixture("candidate", source, []byte("candidate"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), claim, candidate, now.Add(time.Second)))

	var role string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
	err = pgstoretest.RevokeColumnUpdates(t.Context(), setup, pgx.Identifier{role}.Sanitize(), "vector_index_build_jobs", "lease_expires_at")
	require.NoError(t, err)
	require.ErrorContains(t, s.PublishVectorIndexGeneration(t.Context(), claim, candidate.ID, now.Add(time.Second)), "permission denied")
	active, err := s.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	require.Equal(t, prior, active)
	_, err = setup.ExecContext(t.Context(), "GRANT UPDATE ON documents.vector_index_build_jobs TO "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), claim, candidate.ID, now.Add(time.Second)))
	active, err = s.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	require.Equal(t, candidate, active)
}

func checkVectorIndexGenerationSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s, _, source := publishedVectorIndexFixture(t, newPostgresQueryStore(t))
	now := time.Now().UTC()
	claim, found, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "first", now, time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	prior := vectorIndexGenerationFixture("snapshot-prior", source, []byte("old index"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), claim, prior, now))
	require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), claim, prior.ID, now))
	next, found, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "second", now.Add(time.Second), time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	replacement := vectorIndexGenerationFixture("snapshot-next", source, []byte("new index"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), next, replacement, now.Add(time.Second)))
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	var config *pgx.ConnConfig
	require.NoError(t, conn.Raw(func(driverConn any) error {
		pgConn, ok := driverConn.(*stdlib.Conn)
		require.True(t, ok)
		config = pgConn.Conn().Config().Copy()
		return nil
	}))
	require.NoError(t, conn.Close())
	replaced := false
	config.Tracer = &afterFirstSelect{run: func() {
		require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), next, replacement.ID, now.Add(time.Second)))
		removed, err := s.ReclaimVectorIndexGenerations(t.Context(), now.Add(2*time.Second))
		require.NoError(t, err)
		require.Equal(t, 1, removed)
		replaced = true
	}}
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db}
	got, err := reader.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	require.True(t, replaced)
	require.Equal(t, prior, got)
	got, err = s.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	require.Equal(t, replacement, got)
}

func checkVectorReaderInvalidationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner, login := newTemplateDatabase(t)
	s, record, source := publishedVectorIndexFixture(t, openPostgresStore(t, owner, login))
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
	var role string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT current_user`).Scan(&role))
	err := pgstoretest.RevokeColumnUpdates(t.Context(), setup, pgx.Identifier{role}.Sanitize(), "vector_membership_epochs", "revision")
	require.NoError(t, err)
	version, err := s.ContentVersionByID(t.Context(), record.ContentVersionID)
	require.NoError(t, err)
	revision := s.WriteRevision()
	_, _, err = s.Trash(t.Context(), version.NodeID, UnconditionalRev)
	require.ErrorContains(t, err, "permission denied")
	node, err := s.NodeByID(t.Context(), version.NodeID)
	require.NoError(t, err)
	require.Nil(t, node.TrashedAt)
	require.Equal(t, revision, s.WriteRevision())
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
	_, err = setup.ExecContext(t.Context(), "GRANT UPDATE ON documents.vector_membership_epochs TO "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), version.NodeID, UnconditionalRev)
	require.NoError(t, err)
	require.NoError(t, s.withReadTx(t.Context(), func(tx *sql.Tx) error {
		_, err := s.vectorIndexSourceChecksumTx(t.Context(), tx, source.VectorSpaceID)
		require.ErrorIs(t, err, ErrNotFound)
		return nil
	}))
}

func checkVectorReaderRejectsConcurrentHeadReplacementPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s, _, source := publishedVectorIndexFixture(t, newPostgresQueryStore(t))
	now := time.Now().UTC()
	prior := vectorIndexGenerationFixture("leased-old", source, []byte("old index"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, prior))
	next, found, err := s.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID, source.ManifestChecksum, "replacement", now, time.Minute)
	require.NoError(t, err)
	require.True(t, found)
	replacement := vectorIndexGenerationFixture("leased-next", source, []byte("new index"), now)
	require.NoError(t, s.StageVectorIndexGeneration(t.Context(), next, replacement, now))
	replaced := false
	reader := postgresSnapshotReader(t, s, func() {
		require.NoError(t, s.PublishVectorIndexGeneration(t.Context(), next, replacement.ID, now))
		removed, err := s.ReclaimVectorIndexGenerations(t.Context(), now)
		require.NoError(t, err)
		require.Equal(t, 1, removed)
		replaced = true
	})
	reader.writeDB = s.writeDB
	_, err = reader.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
	require.True(t, replaced)
	require.ErrorIs(t, err, ErrVectorIndexSourceStale)
	var leases int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_index_reader_leases`).Scan(&leases))
	require.Zero(t, leases)
	lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "next-reader", now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, replacement, lease.Generation)
}
