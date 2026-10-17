package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestStorageOperationPersistsProgressAndCancellation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	created, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "place", RequestDigest: fakeHash("a5"),
		RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":["a"]}`, TotalObjects: 3,
	})
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(created.ID))
	assert.Equal(t, StorageOperationQueued, created.State)
	require.NoError(t, s.NoteQueuedStorageOperation(t.Context(), created.ID, strings.Repeat("界", 2000)))
	queued, err := s.StorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("界", 4096/3), queued.Error)

	claimed, err := s.ClaimStorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, StorageOperationRunning, claimed.State)
	resumable, err := s.ResumableStorageOperations(t.Context())
	require.NoError(t, err)
	require.Equal(t, []StorageOperation{claimed}, resumable)
	claimed, err = s.ClaimStorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, StorageOperationRunning, claimed.State)
	require.NoError(t, s.AdvanceStorageOperation(
		t.Context(), created.ID, "hash-1", 1, 1, 12,
	))
	require.NoError(t, s.RequestStorageOperationCancel(t.Context(), created.ID))

	current, err := s.StorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "hash-1", current.Cursor)
	assert.Equal(t, int64(1), current.CopiedObjects)
	assert.Equal(t, int64(1), current.CompletedObjects)
	assert.Equal(t, int64(12), current.CopiedBytes)
	assert.True(t, current.CancelRequested)

	require.NoError(t, s.FinishStorageOperation(
		t.Context(), created.ID, StorageOperationCancelled, `{"cancelled":true}`, "",
		time.Now().Add(24*time.Hour),
	))
	items, err := s.StorageOperations(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, StorageOperationCancelled, items[0].State)
	assert.NotNil(t, items[0].FinishedAt)
	require.ErrorIs(t, s.RequestStorageOperationCancel(t.Context(), created.ID), ErrStorageOperationTerminal)
}

func TestStorageOperationRejectsConcurrentEvacuationsForOneStore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))

	first, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: storageOperationKindEvacuate, SourceStoreID: secondary.ID,
		RequestDigest: fakeHash("b7"),
		RequestJSON:   `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	_, err = s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: storageOperationKindEvacuate, SourceStoreID: secondary.ID,
		RequestDigest: fakeHash("b8"),
		RequestJSON:   `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.ErrorIs(t, err, ErrBlobStoreState)

	require.NoError(t, s.FinishStorageOperation(
		t.Context(), first.ID, StorageOperationCompleted, `{}`, "",
		time.Now().Add(24*time.Hour),
	))
	_, err = s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: storageOperationKindEvacuate, SourceStoreID: secondary.ID,
		RequestDigest: fakeHash("b9"),
		RequestJSON:   `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
}

func TestStorageOperationRetryPreservesEvacuationFinalization(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"queued", "reclaimed"} {
		t.Run(phase, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			primary, err := s.PrimaryBlobStore(ctx)
			require.NoError(t, err)
			secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
			require.NoError(t, err)
			require.NoError(t, s.RegisterBlobStore(ctx, secondary))
			require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))
			op, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
				Kind: storageOperationKindEvacuate, SourceStoreID: secondary.ID,
				RequestDigest: fakeHash("ba"), RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":[]}`,
			})
			require.NoError(t, err)
			_, err = s.ClaimStorageOperation(ctx, op.ID)
			require.NoError(t, err)
			hash := fakeHash("bb")
			_, err = s.db.ExecContext(ctx, `INSERT INTO blobs(hash,size,created_at) VALUES(?,4,?)`, hash, nowRFC3339())
			require.NoError(t, err)
			for _, location := range []struct{ storeID, generation string }{
				{secondary.ID, "40000000-0000-4000-8000-000000000001"},
				{primary.ID, "40000000-0000-4000-8000-000000000002"},
			} {
				_, err = s.db.ExecContext(ctx, `INSERT INTO blob_locations(blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible) VALUES(?,?,?,'loose','raw',4,1)`, hash, location.storeID, location.generation)
				require.NoError(t, err)
			}
			finalized, err := s.FinalizeBlobStoreEvacuation(ctx, op.ID, secondary.ID, primary.ID)
			require.NoError(t, err)
			require.False(t, finalized.Detached)
			require.Equal(t, int64(1), finalized.RevokedLocations)
			remaining, err := s.ResolveBlobLocations(ctx, packstore.Hash(hash))
			require.NoError(t, err)
			require.Len(t, remaining.Candidates, 1)
			require.Equal(t, packstore.StoreID(primary.ID), remaining.Candidates[0].StoreID)
			cleanups, err := s.StorageOperationCleanups(ctx, op.ID)
			require.NoError(t, err)
			require.Equal(t, []StorageOperationCleanup{{StoreID: secondary.ID, Ref: packstore.ObjectRef{LooseHash: packstore.Hash(hash), LooseEncoding: packstore.LooseEncodingRaw}}}, cleanups)
			draining, err := s.BlobStoreBySelector(ctx, secondary.ID)
			require.NoError(t, err)
			require.Equal(t, "draining", draining.Lifecycle)
			require.ErrorIs(t, s.RequestStorageOperationCancel(ctx, op.ID), ErrStorageOperationTerminal)

			// Physical cleanup can fail after source authority has been revoked.
			require.NoError(t, s.DeferStorageOperation(ctx, op.ID, errors.New("cleanup unavailable")))
			queued, err := s.StorageOperation(ctx, op.ID)
			require.NoError(t, err)
			require.Equal(t, StorageOperationQueued, queued.State)
			require.Equal(t, "@finalizing", queued.Cursor)

			if phase == "reclaimed" {
				retried, err := s.ClaimStorageOperation(ctx, op.ID)
				require.NoError(t, err)
				assert.Equal(t, "@finalizing", retried.Cursor)
			}
			require.ErrorIs(t, s.RequestStorageOperationCancel(ctx, op.ID), ErrStorageOperationTerminal)
			current, err := s.StorageOperation(ctx, op.ID)
			require.NoError(t, err)
			assert.False(t, current.CancelRequested, "retry must preserve the finalizing evacuation's cancellation boundary")
		})
	}
}

func TestStorageOperationRejectsCancellationAfterTerminalState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	created, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "place", RequestDigest: fakeHash("c7"),
		RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	require.NoError(t, s.FinishStorageOperation(
		t.Context(), created.ID, StorageOperationCompleted, `{}`, "",
		time.Now().Add(24*time.Hour),
	))

	err = s.RequestStorageOperationCancel(t.Context(), created.ID)
	require.ErrorIs(t, err, ErrStorageOperationTerminal)
}

func TestStorageOperationCleanupPersistsUntilCompleted(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	created, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "place", RequestDigest: fakeHash("d8"),
		RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	ref := packstore.ObjectRef{
		LooseHash:     packstore.Hash(fakeHash("e9")),
		LooseEncoding: packstore.LooseEncodingRaw,
	}
	err = s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return recordStorageOperationCleanupTx(
			t.Context(), tx, created.ID, s.primaryStoreID, []packstore.ObjectRef{ref},
		)
	})
	require.NoError(t, err)

	items, err := s.StorageOperationCleanups(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, []StorageOperationCleanup{{
		StoreID: s.primaryStoreID, Ref: ref,
	}}, items)
	require.NoError(t, s.CompleteStorageOperationCleanup(
		t.Context(), created.ID, items[0],
	))
	items, err = s.StorageOperationCleanups(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestPruneExpiredStorageOperationsPreservesPendingCleanup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	createTerminal := func() StorageOperation {
		created, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
			Kind: "place", RequestDigest: fakeHash("fa"),
			RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":[]}`,
		})
		require.NoError(t, err)
		require.NoError(t, s.FinishStorageOperation(
			ctx, created.ID, StorageOperationCompleted, `{}`, "", now.Add(24*time.Hour),
		))
		return created
	}
	expired := createTerminal()
	atBoundary := createTerminal()
	future := createTerminal()
	protected := createTerminal()
	_, err := s.db.ExecContext(ctx, `
		UPDATE storage_operations
		SET retention_until=CASE operation_id
			WHEN ? THEN ? WHEN ? THEN ? WHEN ? THEN ? WHEN ? THEN ? END
		WHERE operation_id IN (?,?,?,?)`,
		expired.ID, now.Add(-time.Hour).Format(timestampLayout),
		atBoundary.ID, now.Format(timestampLayout),
		future.ID, now.Add(time.Hour).Format(timestampLayout),
		protected.ID, now.Add(-time.Hour).Format(timestampLayout),
		expired.ID, atBoundary.ID, future.ID, protected.ID,
	)
	require.NoError(t, err)
	ref := packstore.ObjectRef{
		LooseHash:     packstore.Hash(fakeHash("fb")),
		LooseEncoding: packstore.LooseEncodingRaw,
	}
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return recordStorageOperationCleanupTx(
			ctx, tx, protected.ID, s.primaryStoreID, []packstore.ObjectRef{ref},
		)
	}))

	pruned, err := s.PruneExpiredStorageOperations(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), pruned)
	_, err = s.StorageOperation(ctx, expired.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.StorageOperation(ctx, atBoundary.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.StorageOperation(ctx, future.ID)
	require.NoError(t, err)
	_, err = s.StorageOperation(ctx, protected.ID)
	require.NoError(t, err)

	require.NoError(t, s.CompleteStorageOperationCleanup(
		ctx, protected.ID, StorageOperationCleanup{StoreID: s.primaryStoreID, Ref: ref},
	))
	pruned, err = s.PruneExpiredStorageOperations(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), pruned)
	_, err = s.StorageOperation(ctx, protected.ID)
	require.ErrorIs(t, err, ErrNotFound)
}
