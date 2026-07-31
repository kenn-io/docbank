package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func placementHashesByID(items []PlacementHash) map[string]PlacementHash {
	result := make(map[string]PlacementHash, len(items))
	for _, item := range items {
		result[item.Hash] = item
	}
	return result
}
