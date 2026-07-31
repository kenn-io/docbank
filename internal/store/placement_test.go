package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
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
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: s.primaryStoreID, RetireSource: true,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	assert.Equal(t, firstHash, plan.Hashes[0].Hash)
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
	require.NoError(t, s.BeginBlobStoreEvacuation(ctx, secondary.ID))

	plan, err := s.PlanPlacement(ctx, PlacementRequest{
		TargetNodeID: s.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: s.primaryStoreID, RetireSource: true,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), plan.TransferBytes)
	assert.Equal(t, int64(7), plan.ReadBackBytes)
	assert.Equal(t, int64(4096), plan.RemoteEgressBytes)
	assert.Equal(t, int64(4096), plan.ScratchBytes)
}

func placementHashesByID(items []PlacementHash) map[string]PlacementHash {
	result := make(map[string]PlacementHash, len(items))
	for _, item := range items {
		result[item.Hash] = item
	}
	return result
}
