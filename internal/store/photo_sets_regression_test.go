package store

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhotoSetScopePrecedesDuplicateCollapse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hash := browseHash("duplicate-photo")
	outside := browsePhotoNode(t, s, "outside.jpg", hash, "image/jpeg")
	inside := browsePhotoNode(t, s, "inside.jpg", hash, "image/jpeg")
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET modified_at='2020-01-01T00:00:00.000000000Z' WHERE id=?`, outside.ID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, inside.ID)
	require.NoError(t, err)
	set, err := s.CreatePhotoSet(ctx, "Selected")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	for _, request := range []PhotoBrowseRequest{
		{SetID: set.ID, Query: snapshotTestQuery(t, `{"filters":{"collapse_duplicates":true}}`)},
		{Query: snapshotTestQuery(t, `{"filters":{"set_ids":["`+set.ID+`"],"collapse_duplicates":true}}`)},
		{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"set:`+set.ID+`","filters":{"collapse_duplicates":true}}`)},
	} {
		page, err := s.ListPhotoAssets(ctx, request, nil)
		require.NoError(t, err)
		require.Equal(t, int64(1), page.Total)
		require.Len(t, page.Items, 1)
		require.Equal(t, asset.ID, page.Items[0].AssetID)
	}
	copy, err := s.CreatePhotoSet(ctx, "Copied selection")
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"filters":{"set_ids":["`+set.ID+`"],"collapse_duplicates":true},"sort":{"field":"added_time","direction":"desc"}}`)
	copy, err = s.ChangePhotoSetMembers(ctx, copy.ID, copy.Revision, true, PhotoSetSelection{Query: &value})
	require.NoError(t, err)
	ids, err := photoSetMemberIDs(ctx, s.db, copy.ID)
	require.NoError(t, err)
	require.Equal(t, []string{asset.ID}, ids)
	copy, err = s.ChangePhotoSetMembers(ctx, copy.ID, copy.Revision, false, PhotoSetSelection{Query: &value})
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, copy.ID, "")
	require.NoError(t, err)
	require.Zero(t, summary.MemberCount)
	require.Equal(t, int64(3), summary.Revision)
}

func TestPhotoSetPurgeRemovesOnlyRetiredMembersOnce(t *testing.T) {
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
	set, err := s.CreatePhotoSet(ctx, "Shared")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{live.ID, first.ID, second.ID, pair.ID}})
	require.NoError(t, err)
	cover := &first.ID
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, nil, &cover)
	require.NoError(t, err)
	copy, err := s.DuplicatePhotoSet(ctx, set.ID, set.Revision, "Shared copy")
	require.NoError(t, err)
	for _, nodeID := range []int64{first.Files[0].NodeID, second.Files[0].NodeID, raw.ID} {
		node, err := s.NodeByID(ctx, nodeID)
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, node.ID, node.Revision)
		require.NoError(t, err)
	}
	beforePurge, err := s.PhotoSet(ctx, set.ID, "")
	require.NoError(t, err)
	require.Equal(t, int64(4), beforePurge.MemberCount)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	for _, album := range []PhotoSet{set, copy} {
		summary, err := s.PhotoSet(ctx, album.ID, "")
		require.NoError(t, err)
		require.Equal(t, int64(2), summary.MemberCount)
		require.Equal(t, album.Revision+1, summary.Revision)
		require.Nil(t, summary.CoverAssetID)
		var before, after int64
		var receipt string
		require.NoError(t, s.db.QueryRowContext(ctx, `SELECT before_revision,after_revision,after_json FROM photo_change_receipts WHERE set_id=? AND operation='set_remove'`, album.ID).Scan(&before, &after, &receipt))
		require.Equal(t, album.Revision, before)
		require.Equal(t, album.Revision+1, after)
		var state struct {
			AssetIDs []string `json:"asset_ids"`
		}
		require.NoError(t, json.Unmarshal([]byte(receipt), &state))
		require.ElementsMatch(t, []string{first.ID, second.ID}, state.AssetIDs)
	}
	paired, err := s.PhotoAssetByID(ctx, pair.ID)
	require.NoError(t, err)
	require.Len(t, paired.Files, 1)
	require.Equal(t, jpeg.ID, paired.Files[0].NodeID)
	destination, err := s.CreatePhotoSet(ctx, "Destination")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(ctx, destination.ID, destination.Revision, true, PhotoSetSelection{AssetIDs: []string{live.ID, first.ID}})
	require.ErrorIs(t, err, ErrInvalidPhotoAlbum)
	summary, err := s.PhotoSet(ctx, destination.ID, "")
	require.NoError(t, err)
	require.Zero(t, summary.MemberCount)
	require.Equal(t, destination.Revision, summary.Revision)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(ctx, &roundTrip))
	require.Equal(t, exported.String(), roundTrip.String())
}
