package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestSecondaryBlobStoreLifecycle(t *testing.T) {
	s := newTestStore(t)
	secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))

	stores, err := s.BlobStores(t.Context())
	require.NoError(t, err)
	require.Len(t, stores, 2)
	assert.Equal(t, "primary", stores[0].Role)
	assert.Equal(t, secondary, stores[1])

	resolved, err := s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, secondary, resolved)
	resolved, err = s.BlobStoreBySelector(t.Context(), "archive")
	require.NoError(t, err)
	assert.Equal(t, secondary, resolved)

	require.NoError(t, s.DetachBlobStore(t.Context(), secondary.ID))
	detached, err := s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", detached.Lifecycle)
	require.NoError(t, s.UnregisterBlobStore(t.Context(), secondary.ID))
	_, err = s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestBlobStoreRemovalRejectsActiveDestinationOperation(t *testing.T) {
	s := newTestStore(t)
	secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))
	operation, err := s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "place", StoreReferences: []StorageOperationStoreReference{{
			StoreID: secondary.ID, Role: "destination",
		}},
		RequestDigest: fakeHash("91"), RequestJSON: `{}`, PlanJSON: `{}`,
	})
	require.NoError(t, err)
	require.ErrorIs(t, s.DetachBlobStore(t.Context(), secondary.ID), ErrBlobStoreState)
	require.NoError(t, s.FinishStorageOperation(
		t.Context(), operation.ID, StorageOperationCompleted, `{}`, "",
		time.Now().Add(time.Hour),
	))

	operation, err = s.CreateStorageOperation(t.Context(), StorageOperationCreate{
		Kind: "repair", StoreReferences: []StorageOperationStoreReference{{
			StoreID: secondary.ID, Role: "destination",
		}},
		RequestDigest: fakeHash("92"), RequestJSON: `{}`, PlanJSON: `{}`,
	})
	require.NoError(t, err)
	_, err = s.db.Exec(
		`UPDATE blob_stores SET lifecycle=? WHERE store_id=?`,
		blobStoreLifecycleDetached, secondary.ID,
	)
	require.NoError(t, err)
	require.ErrorIs(t, s.UnregisterBlobStore(t.Context(), secondary.ID), ErrBlobStoreState)
	_, err = s.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)

	_, err = s.ClaimStorageOperation(t.Context(), operation.ID)
	require.NoError(t, err)
	require.ErrorIs(t, s.UnregisterBlobStore(t.Context(), secondary.ID), ErrBlobStoreState)
	require.NoError(t, s.FinishStorageOperation(
		t.Context(), operation.ID, StorageOperationCompleted, `{}`, "",
		time.Now().Add(time.Hour),
	))
	require.NoError(t, s.UnregisterBlobStore(t.Context(), secondary.ID))
}

func TestBlobStoreRemovalRequiresEmptyDetachedSecondary(t *testing.T) {
	s := newTestStore(t)
	primary, err := s.PrimaryBlobStore(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, s.DetachBlobStore(t.Context(), primary.ID), ErrBlobStorePrimary)

	secondary, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))
	require.ErrorIs(t, s.UnregisterBlobStore(t.Context(), secondary.ID), ErrBlobStoreState)

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = s.db.Exec(
		`INSERT INTO blobs(hash, size, created_at) VALUES(?, 1, ?)`,
		hash, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.Exec(`
		INSERT INTO blob_locations(
			blob_hash, store_id, generation, kind, encoding, stored_size, pack_eligible
		) VALUES(?, ?, ?, 'loose', 'raw', 1, 1)`,
		hash,
		secondary.ID, "30000000-0000-4000-8000-000000000001",
	)
	require.NoError(t, err)
	require.ErrorIs(t, s.DetachBlobStore(t.Context(), secondary.ID), ErrBlobStoreNotEmpty)
}

func TestBlobStoreDetachRequiresCompletedPhysicalCleanup(t *testing.T) {
	s := newTestStore(t)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))
	operation, err := s.CreateStorageOperation(
		t.Context(), StorageOperationCreate{
			Kind: "place", RequestDigest: fakeHash("91"),
			RequestJSON: `{}`, PlanJSON: `{}`,
		},
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `
		INSERT INTO storage_operation_cleanup(
			operation_id,store_id,loose_hash,loose_encoding,pack_id
		) VALUES(?,?,?,?,'')`,
		operation.ID, secondary.ID, fakeHash("92"), packstore.LooseEncodingRaw,
	)
	require.NoError(t, err)

	require.ErrorIs(
		t, s.DetachBlobStore(t.Context(), secondary.ID), ErrBlobStoreNotEmpty,
	)
}

func TestEvacuationCleanupIncludesPackAfterMappingsAreRevoked(t *testing.T) {
	s := newTestStore(t)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), secondary))
	_, err = s.db.ExecContext(t.Context(), `
		INSERT INTO blob_packs(
			store_id,pack_id,entry_count,stored_bytes,created_at
		) VALUES(?,?,1,100,?)`,
		secondary.ID, "pack-after-mapping-revocation", nowRFC3339(),
	)
	require.NoError(t, err)
	tx, err := s.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	refs, err := blobStoreObjectRefsTx(t.Context(), tx, secondary.ID)
	require.NoError(t, err)
	require.Equal(t, []packstore.ObjectRef{{
		PackID: "pack-after-mapping-revocation",
	}}, refs)
}

func TestBlobStoreInventoryReportsSoleAuthorityAndAffectedDocuments(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))

	sharedHash := fakeHash("31")
	soleHash := fakeHash("32")
	_, err = s.CreateFile(ctx, s.RootID(), "shared.txt", sharedHash, 7, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "sole.txt", soleHash, 9, "text/plain")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',7,1)`,
		sharedHash, secondary.ID, "31000000-0000-4000-8000-000000000001",
	)
	require.NoError(t, err)

	inventory, err := s.BlobStoreInventory(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), inventory[primary.ID].SoleAuthorityObjects)
	assert.Equal(t, int64(1), inventory[primary.ID].AffectedDocuments)
	assert.Zero(t, inventory[secondary.ID].SoleAuthorityObjects)
	assert.Zero(t, inventory[secondary.ID].AffectedDocuments)
}

func TestBlobStoreUnreadableObjectsAccountsForUnavailableReplicaSet(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	first, err := s.PrepareSecondaryBlobStore("first", "filesystem", "first")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, first))
	second, err := s.PrepareSecondaryBlobStore("second", "filesystem", "second")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, second))

	hash := fakeHash("41")
	_, err = s.CreateFile(ctx, s.RootID(), "remote.txt", hash, 7, "text/plain")
	require.NoError(t, err)
	for index, storeID := range []string{first.ID, second.ID} {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO blob_locations(
				blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
			) VALUES(?,?,?,'loose','raw',7,1)`,
			hash, storeID, fmt.Sprintf("41000000-0000-4000-8000-%012d", index+1),
		)
		require.NoError(t, err)
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM blob_locations WHERE blob_hash=? AND store_id=?`, hash, primary.ID)
	require.NoError(t, err)

	unreadable, err := s.BlobStoreUnreadableObjects(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unreadable[first.ID])
	assert.Equal(t, int64(1), unreadable[second.ID])

	unreadable, err = s.BlobStoreUnreadableObjects(ctx, map[string]bool{first.ID: true})
	require.NoError(t, err)
	assert.Empty(t, unreadable)
}

func TestBlobStoreRegistrationRejectsConflicts(t *testing.T) {
	s := newTestStore(t)
	first, err := s.PrepareSecondaryBlobStore("archive", "filesystem", "archive_nas")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(t.Context(), first))

	for _, candidate := range []BlobStore{
		{
			ID: first.ID, Name: "other", Kind: "filesystem", Role: "secondary",
			Lifecycle: "active", Binding: "other", OwnershipEpoch: first.OwnershipEpoch,
			CreatedAt: first.CreatedAt,
		},
		{
			ID:   "30000000-0000-4000-8000-000000000002",
			Name: first.Name, Kind: "filesystem", Role: "secondary",
			Lifecycle: "active", Binding: "other",
			OwnershipEpoch: "30000000-0000-4000-8000-000000000003",
			CreatedAt:      first.CreatedAt,
		},
	} {
		require.ErrorIs(t, s.RegisterBlobStore(t.Context(), candidate), ErrExists)
	}
}

func TestBlobStoreEvacuationRequiresVerifiedDestinationCoverage(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	require.ErrorIs(t, s.BeginBlobStoreEvacuation(ctx, primary.ID), ErrBlobStorePrimary)
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: storageOperationKindEvacuate, RequestDigest: fakeHash("e9"),
		SourceStoreID: secondary.ID,
		RequestJSON:   `{"version":1}`, PlanJSON: `{"hashes":[]}`, TotalObjects: 1,
	})
	require.NoError(t, err)
	operation, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)

	draining, err := s.BlobStoreBySelector(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "draining", draining.Lifecycle)

	hash := fakeHash("d8")
	_, err = s.db.Exec(
		`INSERT INTO blobs(hash,size,created_at) VALUES(?,4,?)`,
		hash, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.Exec(`
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',4,1)`,
		hash, secondary.ID, "40000000-0000-4000-8000-000000000001",
	)
	require.NoError(t, err)

	_, err = s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.ErrorIs(t, err, ErrBlobStoreNotEmpty)

	_, err = s.db.Exec(`
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',4,1)`,
		hash, primary.ID, "40000000-0000-4000-8000-000000000002",
	)
	require.NoError(t, err)

	require.NoError(t, s.RequestStorageOperationCancel(ctx, operation.ID))
	_, err = s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.ErrorIs(t, err, ErrStorageOperationCancelled)
	remaining, err := s.ResolveBlobLocations(ctx, packstore.Hash(hash))
	require.NoError(t, err)
	require.Len(t, remaining.Candidates, 2)

	_, err = s.db.Exec(
		`UPDATE storage_operations SET cancel_requested=0 WHERE operation_id=?`,
		operation.ID,
	)
	require.NoError(t, err)
	finalized, err := s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.NoError(t, err)
	assert.Equal(t, []packstore.ObjectRef{{
		LooseHash: packstore.Hash(hash), LooseEncoding: packstore.LooseEncodingRaw,
	}}, finalized.Retire)
	assert.False(t, finalized.Detached)
	cleanups, err := s.StorageOperationCleanups(ctx, operation.ID)
	require.NoError(t, err)
	require.Len(t, cleanups, 1)
	err = s.RequestStorageOperationCancel(ctx, operation.ID)
	require.ErrorIs(t, err, ErrStorageOperationTerminal)

	draining, err = s.BlobStoreBySelector(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "draining", draining.Lifecycle)
	require.NoError(t, s.CompleteStorageOperationCleanup(ctx, operation.ID, cleanups[0]))

	otherOperation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: "place", RequestDigest: fakeHash("ec"),
		RequestJSON: `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	otherRef := packstore.ObjectRef{
		LooseHash:     packstore.Hash(fakeHash("ed")),
		LooseEncoding: packstore.LooseEncodingRaw,
	}
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		return recordStorageOperationCleanupTx(
			ctx, tx, otherOperation.ID, secondary.ID, []packstore.ObjectRef{otherRef},
		)
	}))
	finalized, err = s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.NoError(t, err)
	assert.False(t, finalized.Detached)
	otherCleanups, err := s.StorageOperationCleanups(ctx, otherOperation.ID)
	require.NoError(t, err)
	assert.Empty(t, otherCleanups)
	evacuationCleanups, err := s.StorageOperationCleanups(ctx, operation.ID)
	require.NoError(t, err)
	require.Equal(t, []StorageOperationCleanup{{
		StoreID: secondary.ID, Ref: otherRef,
	}}, evacuationCleanups)
	require.NoError(t, s.CompleteStorageOperationCleanup(
		ctx, operation.ID, StorageOperationCleanup{
			StoreID: secondary.ID, Ref: otherRef,
		},
	))

	finalized, err = s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.NoError(t, err)
	assert.True(t, finalized.Detached)
	detached, err := s.BlobStoreBySelector(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", detached.Lifecycle)
}

func TestBlobStoreEvacuationBeginRejectsActiveDestinationOperation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: "repair", StoreReferences: []StorageOperationStoreReference{
			{StoreID: primary.ID, Role: "source"},
			{StoreID: secondary.ID, Role: "destination"},
		},
		RequestDigest: fakeHash("ee"), RequestJSON: `{}`, PlanJSON: `{}`,
	})
	require.NoError(t, err)

	require.ErrorIs(
		t, s.BeginBlobStoreEvacuation(ctx, secondary.ID), ErrBlobStoreState,
	)
	active, err := s.BlobStoreBySelector(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, blobStoreLifecycleActive, active.Lifecycle)

	require.NoError(t, s.FinishStorageOperation(
		ctx, operation.ID, StorageOperationCompleted, `{}`, "",
		time.Now().Add(time.Hour),
	))
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))
	draining, err := s.BlobStoreBySelector(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, blobStoreLifecycleDraining, draining.Lifecycle)
}

func TestBlobStoreEvacuationFinalizationRejectsActiveSourceOperation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))
	evacuation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: storageOperationKindEvacuate, SourceStoreID: secondary.ID,
		StoreReferences: []StorageOperationStoreReference{{
			StoreID: primary.ID, Role: "destination",
		}},
		RequestDigest: fakeHash("ef"), RequestJSON: `{}`, PlanJSON: `{}`,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, evacuation.ID)
	require.NoError(t, err)
	other, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: "repair", StoreReferences: []StorageOperationStoreReference{
			{StoreID: secondary.ID, Role: "source"},
			{StoreID: primary.ID, Role: "destination"},
		},
		RequestDigest: fakeHash("f0"), RequestJSON: `{}`, PlanJSON: `{}`,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, other.ID)
	require.NoError(t, err)

	_, err = s.FinalizeBlobStoreEvacuation(
		ctx, evacuation.ID, secondary.ID, primary.ID,
	)
	require.ErrorIs(t, err, ErrBlobStoreState)
	draining, err := s.BlobStoreBySelector(ctx, secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, blobStoreLifecycleDraining, draining.Lifecycle)

	require.NoError(t, s.FinishStorageOperation(
		ctx, other.ID, StorageOperationCompleted, `{}`, "",
		time.Now().Add(time.Hour),
	))
	finalized, err := s.FinalizeBlobStoreEvacuation(
		ctx, evacuation.ID, secondary.ID, primary.ID,
	)
	require.NoError(t, err)
	assert.True(t, finalized.Detached)
}

func TestEmptyBlobStoreEvacuationFinalizationRejectsCancellation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: storageOperationKindEvacuate, RequestDigest: fakeHash("ea"),
		SourceStoreID: secondary.ID,
		RequestJSON:   `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)

	finalized, err := s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.NoError(t, err)
	assert.True(t, finalized.Detached)
	err = s.RequestStorageOperationCancel(ctx, operation.ID)
	require.ErrorIs(t, err, ErrStorageOperationTerminal)
}

func TestDetachedBlobStoreEvacuationFinalizationRejectsCancellation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	primary, err := s.PrimaryBlobStore(ctx)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive_nas",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: storageOperationKindEvacuate, RequestDigest: fakeHash("eb"),
		SourceStoreID: secondary.ID,
		RequestJSON:   `{"version":1}`, PlanJSON: `{"hashes":[]}`,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)
	_, err = s.db.Exec(
		`UPDATE blob_stores SET lifecycle=? WHERE store_id=?`,
		blobStoreLifecycleDetached, secondary.ID,
	)
	require.NoError(t, err)

	finalized, err := s.FinalizeBlobStoreEvacuation(
		ctx, operation.ID, secondary.ID, primary.ID,
	)
	require.NoError(t, err)
	assert.True(t, finalized.Detached)
	err = s.RequestStorageOperationCancel(ctx, operation.ID)
	require.ErrorIs(t, err, ErrStorageOperationTerminal)
}
