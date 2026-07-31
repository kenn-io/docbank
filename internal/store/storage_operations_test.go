package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestStorageOperationPersistsProgressAndCancellation(t *testing.T) {
	s := newTestStore(t)
	created, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "place", RequestDigest: fakeHash("a5"),
		RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":["a"]}`, TotalObjects: 3,
	})
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(created.ID))
	assert.Equal(t, StorageOperationQueued, created.State)

	claimed, err := s.ClaimStorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, StorageOperationRunning, claimed.State)
	require.NoError(t, s.AdvanceStorageOperation(
		t.Context(), created.ID, "hash-1", 1, 1, 12,
	))
	require.NoError(t, s.RequestStorageOperationCancel(t.Context(), created.ID))

	current, err := s.StorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, "hash-1", current.Cursor)
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
}

func TestStorageOperationClaimResumesInterruptedWork(t *testing.T) {
	s := newTestStore(t)
	created, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "place", RequestDigest: fakeHash("b6"),
		RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(t.Context(), created.ID)
	require.NoError(t, err)

	resumable, err := s.ResumableStorageOperations(t.Context())
	require.NoError(t, err)
	require.Len(t, resumable, 1)
	assert.Equal(t, created.ID, resumable[0].ID)

	claimed, err := s.ClaimStorageOperation(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, StorageOperationRunning, claimed.State)
}

func TestStorageOperationRejectsConcurrentEvacuationsForOneStore(t *testing.T) {
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

func TestStorageOperationRejectsCancellationAfterTerminalState(t *testing.T) {
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
