package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type vectorIndexHeadLeaser interface {
	AcquireVectorIndexGenerationHead(ctx context.Context, space, owner string, at time.Time, duration time.Duration) (VectorIndexReaderLease, error)
}

func TestVectorIndexGenerationHeadLease(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	head, ok := any(s).(vectorIndexHeadLeaser)
	require.True(t, ok, "store must offer metadata-only reader leases")
	now := time.Now().UTC()
	first := vectorIndexGenerationFixture("head-first", source, []byte("first-bytes"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, first))
	lease, err := head.AcquireVectorIndexGenerationHead(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
	require.NoError(t, err)
	expected := first
	expected.ByteSize = len(first.Bytes)
	expected.Bytes = nil
	require.Equal(t, expected, lease.Generation)
	full, err := s.LoadVectorIndexGeneration(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, len(first.Bytes), full.ByteSize)
	require.Equal(t, first.Bytes, full.Bytes)
	full.Bytes = nil
	require.Equal(t, full, lease.Generation)
	second := vectorIndexGenerationFixture("head-second", source, []byte("second-bytes"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, second))
	_, err = s.ReclaimVectorIndexGenerations(t.Context(), now.Add(time.Second))
	require.NoError(t, err)
	require.True(t, vectorIndexGenerationExistsForTest(t, s, first.ID), "query lease must protect the exact old generation")
	require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now.Add(time.Second)))
	_, err = s.ReclaimVectorIndexGenerations(t.Context(), now.Add(2*time.Second))
	require.NoError(t, err)
	require.False(t, vectorIndexGenerationExistsForTest(t, s, first.ID))
	for _, tc := range []struct {
		space, owner string
		at           time.Time
		duration     time.Duration
	}{
		{"bad", "reader", now, time.Minute}, {source.VectorSpaceID, "", now, time.Minute},
		{source.VectorSpaceID, "reader", time.Time{}, time.Minute}, {source.VectorSpaceID, "reader", now, 0},
	} {
		_, err := head.AcquireVectorIndexGenerationHead(t.Context(), tc.space, tc.owner, tc.at, tc.duration)
		require.Error(t, err)
	}
}

func TestVectorIndexGenerationHeadRejectsCorruptMetadata(t *testing.T) {
	t.Parallel()
	for _, assignment := range []string{"byte_size=0", "byte_size=536870913", "row_count=0", "row_count=1000001", "built_at='bad'", "index_manifest_checksum='bad'", "source_manifest_checksum='bad'", "vector_space_id='bad'", "vector_space_id='" + hashVectorIndexTest("other-space") + "'"} {
		t.Run(assignment, func(t *testing.T) {
			s, _, source := newPublishedVectorIndexFixture(t)
			head, ok := any(s).(vectorIndexHeadLeaser)
			require.True(t, ok, "store must offer metadata-only reader leases")
			now := time.Now().UTC()
			record := vectorIndexGenerationFixture("head-corrupt", source, []byte("bytes"), now)
			require.NoError(t, putVectorIndexGenerationForTest(t, s, record))
			err := s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(t.Context(), "PRAGMA ignore_check_constraints=ON"); err != nil {
					return err
				}
				defer func() { _, _ = tx.ExecContext(t.Context(), "PRAGMA ignore_check_constraints=OFF") }()
				_, err := tx.ExecContext(t.Context(), "UPDATE vector_index_generations SET "+assignment+" WHERE generation_id=?", record.ID)
				return err
			})
			require.NoError(t, err)
			_, err = head.AcquireVectorIndexGenerationHead(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
			require.Error(t, err)
			var leases int
			require.NoError(t, s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_index_reader_leases").Scan(&leases))
			require.Zero(t, leases)
		})
	}
}

func TestVectorIndexGenerationSourceMismatchIsStale(t *testing.T) {
	t.Parallel()
	s, _, source := newPublishedVectorIndexFixture(t)
	now := time.Now().UTC()
	record := vectorIndexGenerationFixture("source-mismatch", source, []byte("bytes"), now)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, record))
	_, err := s.db.ExecContext(t.Context(), "UPDATE vector_index_generations SET source_manifest_checksum=? WHERE generation_id=?", hashVectorIndexTest("other-source"), record.ID)
	require.NoError(t, err)
	for _, acquire := range []func(context.Context, string, string, time.Time, time.Duration) (VectorIndexReaderLease, error){s.AcquireVectorIndexGeneration, s.AcquireVectorIndexGenerationHead} {
		_, err = acquire(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
		require.ErrorIs(t, err, ErrVectorIndexSourceStale)
	}
	var leases int
	require.NoError(t, s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_index_reader_leases").Scan(&leases))
	require.Zero(t, leases)
}
