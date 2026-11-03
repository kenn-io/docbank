package store

import (
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSemanticWarmedAcquisitionDoesNotRecaptureSets(t *testing.T) {
	t.Parallel()
	s, record, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	require.NoError(t, putVectorIndexGenerationForTest(t, s, vectorIndexGenerationFixture("warm-search", source, []byte("index"), now)))
	var queries atomic.Int64
	release := make(chan struct{})
	close(release)
	for _, pool := range []**sql.DB{&s.db, &s.writeDB} {
		db := *pool
		wrapped := sql.OpenDB(photoSearchBeginConnector{db: db, start: func() {}, release: release, query: func(query string) {
			if strings.Contains(query, "FROM embedding_heads") || strings.Contains(query, "JOIN embedding_vector_sets") {
				queries.Add(1)
			}
		}})
		wrapped.SetMaxOpenConns(1)
		*pool = wrapped
		t.Cleanup(func() { require.NoError(t, wrapped.Close()); require.NoError(t, db.Close()) })
	}
	acquire := func() {
		t.Helper()
		lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "warm-search", now, time.Minute)
		require.NoError(t, err)
		current, rows, wholeSpace, err := s.semanticSearchAuthorityFence(t.Context(), record.ProcessingProfileFingerprint, record.BindingID, record.InputKind, source.VectorSpaceID, SearchOptions{}, nil)
		require.NoError(t, err)
		require.Equal(t, lease.Generation.SourceManifestChecksum, current)
		require.True(t, wholeSpace)
		require.Nil(t, rows)
		require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now))
	}
	acquire()
	require.Positive(t, queries.Load(), "the cold acquisition must exercise the query counter")
	queries.Store(0)
	for range 3 {
		acquire()
	}
	require.Zero(t, queries.Load(), "warmed pre-search acquisition must reuse source authority without querying every set")
}
