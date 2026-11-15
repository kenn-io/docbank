package store

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkVectorIndexPublicationRollbackPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s, _, source := publishedVectorIndexFixture(t, newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner}))
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
	_, err = setup.ExecContext(t.Context(), "REVOKE UPDATE(lease_expires_at) ON documents.vector_index_build_jobs FROM "+pgx.Identifier{role}.Sanitize())
	require.NoError(t, err)
	require.ErrorContains(t, s.PublishVectorIndexGeneration(t.Context(), claim, candidate.ID, now.Add(time.Second)), "permission denied")
	active, err := s.ActiveVectorIndexGeneration(t.Context(), source.VectorSpaceID)
	require.NoError(t, err)
	require.Equal(t, prior, active)
	_, err = setup.ExecContext(t.Context(), "GRANT UPDATE(lease_expires_at) ON documents.vector_index_build_jobs TO "+pgx.Identifier{role}.Sanitize())
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
