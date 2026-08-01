package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestPlacementPlanUsesRetainedSubtreeAndCompleteReferenceClosure(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	archive, err := s.Mkdir(ctx, s.RootID(), "archive")
	require.NoError(t, err)
	firstHash := fakeHash("a1")
	secondHash := fakeHash("b2")
	file, err := s.CreateFile(ctx, archive.ID, "report.txt", firstHash, 5, "text/plain")
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(
		ctx, file.ID, file.Revision, secondHash, 6, "text/plain",
	)
	require.NoError(t, err)
	trashed, err := s.CreateFile(
		ctx, archive.ID, "retained.txt", fakeHash("c3"), 8, "text/plain",
	)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	_, err = s.CreateFile(
		ctx, s.RootID(), "shared.txt", firstHash, 5, "text/plain",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(
		ctx, archive.ID, "duplicate.txt", firstHash, 5, "text/plain",
	)
	require.NoError(t, err)
	destination, err := s.PrepareSecondaryBlobStore("archive-store", "filesystem", "archive")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, destination))

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: archive.ID, SourceStoreID: s.primaryStoreID,
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 3)
	assert.Equal(t, int64(4), plan.SelectedVersions)
	assert.Equal(t, int64(24), plan.LogicalBytes)
	byHash := placementHashesByID(plan.Hashes)
	assert.Equal(t, int64(2), byHash[firstHash].SelectedReferences)
	assert.Equal(t, int64(3), byHash[firstHash].TotalReferences)
	assert.True(t, byHash[firstHash].SharedReference)
	assert.False(t, byHash[firstHash].RetireSource)
	assert.True(t, byHash[secondHash].RetireSource)
	assert.True(t, byHash[fakeHash("c3")].RetireSource)
}

func TestPlacementCommitCannotExpandRetirementBeyondPreview(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	archive, err := s.Mkdir(ctx, s.RootID(), "archive")
	require.NoError(t, err)
	hash := fakeHash("a2")
	_, err = s.CreateFile(ctx, archive.ID, "selected.txt", hash, 5, "text/plain")
	require.NoError(t, err)
	outside, err := s.CreateFile(ctx, s.RootID(), "outside.txt", hash, 5, "text/plain")
	require.NoError(t, err)
	destination, err := s.PrepareSecondaryBlobStore("archive-store", "filesystem", "archive")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, destination))
	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: archive.ID, SourceStoreID: s.primaryStoreID,
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	assert.False(t, plan.Hashes[0].RetireSource)
	_, _, err = s.Trash(ctx, outside.ID, outside.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: storageOperationKindPlace, StoreReferences: []StorageOperationStoreReference{
			{StoreID: s.primaryStoreID, Role: storageOperationRoleSource},
			{StoreID: destination.ID, Role: storageOperationRoleDestination},
		},
		RequestDigest: plan.Digest, RequestJSON: `{}`, PlanJSON: `{}`,
		TotalObjects: 1,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)
	receipt := packstore.ReadLocation{
		StoreID:    packstore.StoreID(destination.ID),
		Generation: "40000000-0000-4000-8000-000000000011",
		Loose: &packstore.LooseLocation{
			Encoding: packstore.LooseEncodingRaw, LogicalSize: 5, StoredSize: 5,
		},
	}
	committed, err := s.CommitPlacement(
		ctx, operation.ID, plan.Request, plan.Hashes[0], receipt,
	)
	require.NoError(t, err)
	assert.False(t, committed.SourceRevoked)
	resolution, err := s.ResolveBlobLocations(ctx, packstore.Hash(hash))
	require.NoError(t, err)
	require.Len(t, resolution.Candidates, 2)
}

func TestPlacementCommitRejectsPlannedDestinationDrift(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("a3")
	file, err := s.CreateFile(ctx, s.RootID(), "document.txt", hash, 7, "text/plain")
	require.NoError(t, err)
	destination, err := s.PrepareSecondaryBlobStore("archive-store", "filesystem", "archive")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, destination))
	const firstGeneration = "40000000-0000-4000-8000-000000000012"
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',7,1)`,
		hash, destination.ID, firstGeneration,
	)
	require.NoError(t, err)
	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: s.primaryStoreID,
		DestinationStoreID: destination.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, plan.Hashes[0].Destination)
	const secondGeneration = "40000000-0000-4000-8000-000000000013"
	_, err = s.db.ExecContext(ctx, `
		UPDATE blob_locations SET generation=?
		WHERE blob_hash=? AND store_id=?`,
		secondGeneration, hash, destination.ID,
	)
	require.NoError(t, err)
	operation, err := s.CreateStorageOperation(ctx, StorageOperationCreate{
		Kind: storageOperationKindPlace, StoreReferences: []StorageOperationStoreReference{
			{StoreID: s.primaryStoreID, Role: storageOperationRoleSource},
			{StoreID: destination.ID, Role: storageOperationRoleDestination},
		},
		RequestDigest: plan.Digest, RequestJSON: `{}`, PlanJSON: `{}`,
		TotalObjects: 1,
	})
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(ctx, operation.ID)
	require.NoError(t, err)
	actual := *plan.Hashes[0].Destination
	actual.Generation = secondGeneration
	_, err = s.CommitPlacement(ctx, operation.ID, plan.Request, plan.Hashes[0], actual)
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestPlacementPlanPinsAuditedContentToPrimaryByDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	archive, err := s.Mkdir(ctx, s.RootID(), "archive")
	require.NoError(t, err)
	hash := fakeHash("a4")
	_, err = s.CreateFile(ctx, archive.ID, "record.txt", hash, 7, "text/plain")
	require.NoError(t, err)
	auditPlan, err := s.PreviewInitialAudit(ctx, archive.ID, "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, auditPlan)
	require.NoError(t, err)
	destination, err := s.PrepareSecondaryBlobStore("cold", "filesystem", "cold")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, destination))

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: archive.ID, SourceStoreID: s.primaryStoreID,
		DestinationStoreID: destination.ID, RetireSource: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	assert.True(t, plan.Hashes[0].AuditPinned)
	assert.False(t, plan.Hashes[0].RetireSource)
	assert.Equal(t, int64(7), plan.AuditPinnedBytes)

	remoteOnly, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: archive.ID, SourceStoreID: s.primaryStoreID,
		DestinationStoreID: destination.ID, RetireSource: true,
		AllowAuditedRemoteOnly: true,
	})
	require.NoError(t, err)
	assert.True(t, remoteOnly.Hashes[0].RetireSource)
}

func TestPlacementPlanRejectsContentMissingFromBothRequestedStores(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	file, err := s.CreateFile(
		ctx, s.RootID(), "elsewhere.txt", fakeHash("ac"), 9, "text/plain",
	)
	require.NoError(t, err)
	first, err := s.PrepareSecondaryBlobStore("first", "filesystem", "first")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, first))
	second, err := s.PrepareSecondaryBlobStore("second", "filesystem", "second")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, second))

	_, err = s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: file.ID, SourceStoreID: first.ID,
		DestinationStoreID: second.ID,
	})
	require.ErrorIs(t, err, packstore.ErrPhysicalAuthorityMissing)
}

func TestEvacuationPlansOnlyAuthorityHeldBySource(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	firstHash := fakeHash("d4")
	secondHash := fakeHash("e5")
	_, err := s.CreateFile(
		ctx, s.RootID(), "first.txt", firstHash, 4, "text/plain",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(
		ctx, s.RootID(), "second.txt", secondHash, 5, "text/plain",
	)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',4,1)`,
		firstHash, secondary.ID, "40000000-0000-4000-8000-000000000004",
	)
	require.NoError(t, err)

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: s.primaryStoreID, RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	assert.Equal(t, firstHash, plan.Hashes[0].Hash)
}

func TestEvacuationPlansRetainedBlobsWithoutVersionReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("ab")
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", "filesystem", "archive",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO blobs(hash,size,created_at) VALUES(?,12,?)`,
		hash, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',12,1)`,
		hash, secondary.ID,
		"40000000-0000-4000-8000-000000000012",
	)
	require.NoError(t, err)

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: s.primaryStoreID, RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	assert.Equal(t, hash, plan.Hashes[0].Hash)
	assert.Zero(t, plan.Hashes[0].SelectedReferences)
	assert.Zero(t, plan.Hashes[0].TotalReferences)
	assert.True(t, plan.Hashes[0].RetireSource)
}

func TestS3PackedPlacementReportsContainerScratchAndEgress(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("f6")
	_, err := s.CreateFile(
		ctx, s.RootID(), "packed.txt", hash, 7, "text/plain",
	)
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore("cold", "s3", "cold")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	packID := pack.NewPackID()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at)
		VALUES(?,?,1,4096,?)`,
		secondary.ID, packID, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_pack_entries(
			blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
		) VALUES(?,?,?,?,7,7,0,0)`,
		hash, secondary.ID, packID, pack.MinEntryOffset,
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,stored_size,pack_eligible
		) VALUES(?,?,?,'packed',7,1)`,
		hash, secondary.ID, "40000000-0000-4000-8000-000000000005",
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM blob_locations WHERE blob_hash=? AND store_id=?`,
		hash, s.primaryStoreID,
	)
	require.NoError(t, err)
	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: s.primaryStoreID, RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), plan.TransferBytes)
	assert.Equal(t, int64(7), plan.ReadBackBytes)
	assert.Equal(t, int64(4096), plan.RemoteEgressBytes)
	assert.Equal(t, int64(4096), plan.ScratchBytes)
}

func TestPlacementExistingLocalDestinationReportsVerificationRead(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("f7")
	_, err := s.CreateFile(ctx, s.RootID(), "present.txt", hash, 11, "text/plain")
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore(
		"archive", blobStoreKindFilesystem, "archive",
	)
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','raw',11,1)`,
		hash, secondary.ID, "40000000-0000-4000-8000-000000000006",
	)
	require.NoError(t, err)

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: s.primaryStoreID,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(11), plan.AlreadyPresentBytes)
	assert.Equal(t, int64(11), plan.ReadBackBytes)
	assert.Zero(t, plan.TransferBytes)
	assert.Zero(t, plan.RemoteEgressBytes)
}

func TestPlacementExistingS3PackReportsVerificationEgressAndScratch(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("f8")
	_, err := s.CreateFile(ctx, s.RootID(), "remote.txt", hash, 13, "text/plain")
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore("cold", blobStoreKindS3, "cold")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	packID := pack.NewPackID()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_packs(store_id,pack_id,entry_count,stored_bytes,created_at)
		VALUES(?,?,1,8192,?)`,
		secondary.ID, packID, nowRFC3339(),
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_pack_entries(
			blob_hash,store_id,pack_id,pack_offset,stored_len,raw_len,flags,crc32c
		) VALUES(?,?,?,?,13,13,0,0)`,
		hash, secondary.ID, packID, pack.MinEntryOffset,
	)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,stored_size,pack_eligible
		) VALUES(?,?,?,'packed',13,1)`,
		hash, secondary.ID, "40000000-0000-4000-8000-000000000007",
	)
	require.NoError(t, err)

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: s.primaryStoreID,
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(13), plan.AlreadyPresentBytes)
	assert.Equal(t, int64(13), plan.ReadBackBytes)
	assert.Zero(t, plan.TransferBytes)
	assert.Equal(t, int64(8192), plan.RemoteEgressBytes)
	assert.Equal(t, int64(8192), plan.ScratchBytes)
}

func TestPlacementExistingS3LooseReportsPhysicalVerificationEgress(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("f9")
	_, err := s.CreateFile(ctx, s.RootID(), "compressed.txt", hash, 100, "text/plain")
	require.NoError(t, err)
	secondary, err := s.PrepareSecondaryBlobStore("cold", blobStoreKindS3, "cold")
	require.NoError(t, err)
	require.NoError(t, s.RegisterBlobStore(ctx, secondary))
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
		) VALUES(?,?,?,'loose','zstd',25,1)`,
		hash, secondary.ID, "40000000-0000-4000-8000-000000000008",
	)
	require.NoError(t, err)

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: s.primaryStoreID,
		DestinationStoreID: secondary.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(100), plan.AlreadyPresentBytes)
	assert.Equal(t, int64(100), plan.ReadBackBytes)
	assert.Equal(t, int64(25), plan.RemoteEgressBytes)
}

func placementHashesByID(items []PlacementHash) map[string]PlacementHash {
	result := make(map[string]PlacementHash, len(items))
	for _, item := range items {
		result[item.Hash] = item
	}
	return result
}
