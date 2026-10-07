package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoSetMembershipSurvivesEmptyAssets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	live := albumAsset(t, s, "live.jpg")
	first := albumAsset(t, s, "first.jpg")
	second := albumAsset(t, s, "second.jpg")
	raw := browsePhotoNode(t, s, "capture.raw", browseHash("capture-raw"), "application/octet-stream")
	jpeg := browsePhotoNode(t, s, "capture.jpg", browseHash("capture-jpeg"), "image/jpeg")
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	pair, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	pair, err = s.AttachPhotoFile(ctx, pair.ID, pair.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	var recipe string
	for _, asset := range []PhotoAsset{live, first} {
		node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
		require.NoError(t, err)
		grid := visualPreviewRecipe()
		grid.MaxEdgePixels = 512
		generation, err := s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("preview-"+asset.ID), 9, grid), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
		recipe = generation.RecipeFingerprint
	}
	set, err := s.CreatePhotoSet(ctx, "Shared")
	require.NoError(t, err)
	members := []string{live.ID, first.ID, second.ID, pair.ID}
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: members})
	require.NoError(t, err)
	cover := &first.ID
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, new(true), &cover)
	require.NoError(t, err)
	var firstAdded, secondAdded string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT added_at FROM photo_set_members WHERE set_id=? AND asset_id=?`, set.ID, first.ID).Scan(&firstAdded))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT added_at FROM photo_set_members WHERE set_id=? AND asset_id=?`, set.ID, second.ID).Scan(&secondAdded))
	var receiptsBefore int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts WHERE set_id=?`, set.ID).Scan(&receiptsBefore))
	detached, err := s.DetachPhotoFile(ctx, first.ID, first.Revision, first.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Equal(t, int64(3), summary.MemberCount)
	require.Equal(t, int64(3), summary.IncludedCount)
	require.Equal(t, live.ID, *summary.EffectiveCoverAssetID)
	require.Equal(t, set, summary.PhotoSet)
	for _, nodeID := range []int64{second.Files[0].NodeID, raw.ID} {
		node, err := s.NodeByID(ctx, nodeID)
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, node.ID, node.Revision)
		require.NoError(t, err)
	}
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	summary, err = s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Equal(t, int64(2), summary.MemberCount)
	require.Equal(t, int64(2), summary.IncludedCount)
	require.Equal(t, live.ID, *summary.EffectiveCoverAssetID)
	require.Equal(t, set, summary.PhotoSet)
	ids, err := photoSetMemberIDs(ctx, s.db, set.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, members, ids)
	paired, err := s.PhotoAssetByID(ctx, pair.ID)
	require.NoError(t, err)
	require.Len(t, paired.Files, 1)
	require.Equal(t, jpeg.ID, paired.Files[0].NodeID)
	value := snapshotTestQuery(t, sprintfPhotoSort("added_time", "asc"))
	value.Filters.SetIDs = []string{set.ID}
	page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: value}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total)
	noop, err := s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{second.ID}})
	require.NoError(t, err)
	require.Equal(t, set, noop)
	_, err = s.AttachPhotoFile(ctx, first.ID, detached.Revision, first.Files[0].NodeID, PhotoRoleImage, nil)
	require.NoError(t, err)
	replacement := albumAsset(t, s, "replacement.jpg")
	_, err = s.DetachPhotoFile(ctx, replacement.ID, replacement.Revision, replacement.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	emptied, err := s.PhotoAssetByID(ctx, second.ID)
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, second.ID, emptied.Revision, replacement.Files[0].NodeID, PhotoRoleImage, nil)
	require.NoError(t, err)
	summary, err = s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Equal(t, int64(4), summary.MemberCount)
	require.Equal(t, int64(4), summary.IncludedCount)
	require.Equal(t, first.ID, *summary.EffectiveCoverAssetID)
	require.Equal(t, set, summary.PhotoSet)
	var firstAfter, secondAfter string
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT added_at FROM photo_set_members WHERE set_id=? AND asset_id=?`, set.ID, first.ID).Scan(&firstAfter))
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT added_at FROM photo_set_members WHERE set_id=? AND asset_id=?`, set.ID, second.ID).Scan(&secondAfter))
	require.Equal(t, firstAdded, firstAfter)
	require.Equal(t, secondAdded, secondAfter)
	var receiptsAfter int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts WHERE set_id=?`, set.ID).Scan(&receiptsAfter))
	require.Equal(t, receiptsBefore, receiptsAfter)
}
