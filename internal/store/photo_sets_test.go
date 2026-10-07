package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func albumAsset(t *testing.T, s *Store, name string) PhotoAsset {
	t.Helper()
	node := browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	return asset
}

func TestPhotoSetLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "first.jpg")
	b := albumAsset(t, s, "second.jpg")
	set, err := s.CreatePhotoSet(ctx, "Holiday")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID, a.ID}})
	require.NoError(t, err)
	require.Equal(t, int64(2), set.Revision)
	noop, err := s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID}})
	require.NoError(t, err)
	require.Equal(t, set, noop)
	_, err = s.ChangePhotoSetMembers(ctx, set.ID, 1, false, PhotoSetSelection{AssetIDs: []string{a.ID}})
	require.ErrorIs(t, err, ErrStaleRevision)
	cover := &a.ID
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, new("Trip"), new(true), &cover)
	require.NoError(t, err)
	_, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, nil, new(new("00000000-0000-4000-8000-000000000001")))
	require.ErrorIs(t, err, ErrInvalidPhotoAlbum)
	noop, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, new("Trip"), new(true), &cover)
	require.NoError(t, err)
	require.Equal(t, set, noop)
	copy, err := s.DuplicatePhotoSet(ctx, set.ID, set.Revision, "Copy")
	require.NoError(t, err)
	require.Equal(t, int64(1), copy.Revision)
	require.True(t, copy.Starred)
	require.Equal(t, set.CoverAssetID, copy.CoverAssetID)
	ids, err := photoSetMemberIDs(ctx, s.db, set.ID)
	require.NoError(t, err)
	copyIDs, err := photoSetMemberIDs(ctx, s.db, copy.ID)
	require.NoError(t, err)
	require.Equal(t, ids, copyIDs)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, false, PhotoSetSelection{AssetIDs: []string{a.ID}})
	require.NoError(t, err)
	require.Nil(t, set.CoverAssetID)
	deleted, err := s.DeletePhotoSet(ctx, set.ID, set.Revision)
	require.NoError(t, err)
	require.NotNil(t, deleted.DeletedAt)
	_, err = s.PhotoSet(ctx, set.ID, "")
	require.ErrorIs(t, err, ErrNotFound)
	albums, err := s.ListPhotoSets(ctx, "")
	require.NoError(t, err)
	require.Len(t, albums, 1)
	require.Equal(t, copy.ID, albums[0].ID)
	for _, asset := range []PhotoAsset{a, b} {
		actual, err := s.PhotoAssetByID(ctx, asset.ID)
		require.NoError(t, err)
		require.Equal(t, asset, actual)
	}
	require.NoError(t, validatePhotoMetadataState(ctx, s.db))
}

func TestPhotoSetSelectionRollback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "selected.jpg")
	set, err := s.CreatePhotoSet(ctx, "Selection")
	require.NoError(t, err)
	for _, selection := range []PhotoSetSelection{
		{}, {AssetIDs: []string{a.ID, "00000000-0000-4000-8000-000000000001"}},
		{AssetIDs: []string{a.ID}, Query: new(snapshotTestQuery(t, `{}`))},
		{AssetIDs: []string{a.ID}, Coverage: CoverageSelection{Configuration: "configured"}},
		{Query: new(snapshotTestQuery(t, `{"syntax":"advanced","text":"set:invalid"}`))},
		{Query: new(snapshotTestQuery(t, `{}`)), Coverage: CoverageSelection{Configuration: "configured", ProfileFingerprint: "invalid"}},
	} {
		_, err := s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, selection)
		require.Error(t, err)
		actual, err := s.PhotoSet(ctx, set.ID, "")
		require.NoError(t, err)
		require.Zero(t, actual.MemberCount)
		require.Equal(t, set.Revision, actual.Revision)
	}
}

func TestPhotoSetQueryReceiptsCoverCompleteLargeScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for i := range 10001 {
			node, _, err := s.createFileTx(ctx, tx, s.RootID(), fmt.Sprintf("photo-%05d.jpg", i), browseHash(fmt.Sprint(i)), 20, "image/jpeg")
			if err != nil {
				return err
			}
			if err := s.enrollNewPhotoFileTx(ctx, tx, node); err != nil {
				return err
			}
		}
		return nil
	}))
	set, err := s.CreatePhotoSet(ctx, "Large")
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{}`)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{Query: &value})
	require.NoError(t, err)
	require.Equal(t, int64(2), set.Revision)
	summary, err := s.PhotoSet(ctx, set.ID, "")
	require.NoError(t, err)
	require.Equal(t, int64(10001), summary.MemberCount)
	rows, err := s.db.QueryContext(ctx, `SELECT before_revision,after_revision,before_json,after_json FROM photo_change_receipts WHERE set_id=? AND operation='set_add'`, set.ID)
	require.NoError(t, err)
	seen := map[string]bool{}
	count := 0
	for rows.Next() {
		var before, after int64
		var beforeJSON, afterJSON string
		require.NoError(t, rows.Scan(&before, &after, &beforeJSON, &afterJSON))
		require.Equal(t, int64(1), before)
		require.Equal(t, int64(2), after)
		require.LessOrEqual(t, len(beforeJSON), maxPhotoReceiptBytes)
		require.LessOrEqual(t, len(afterJSON), maxPhotoReceiptBytes)
		var state struct {
			AssetIDs []string `json:"asset_ids"`
		}
		require.NoError(t, json.Unmarshal([]byte(afterJSON), &state))
		require.LessOrEqual(t, len(state.AssetIDs), 256)
		for _, id := range state.AssetIDs {
			require.False(t, seen[id])
			seen[id] = true
		}
		count++
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 40, count)
	require.Len(t, seen, 10001)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, false, PhotoSetSelection{Query: &value})
	require.NoError(t, err)
	require.Equal(t, int64(3), set.Revision)
	summary, err = s.PhotoSet(ctx, set.ID, "")
	require.NoError(t, err)
	require.Zero(t, summary.MemberCount)
	require.NoError(t, validatePhotoMetadataState(ctx, s.db))
}

func TestPhotoSetBrowseFiltersVisibilityAndCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	set, err := s.CreatePhotoSet(ctx, "Ordered")
	require.NoError(t, err)
	var ids []string
	for i := range 5 {
		asset := albumAsset(t, s, fmt.Sprintf("photo-%d.jpg", i))
		ids = append(ids, asset.ID)
		if i < 4 {
			node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
			require.NoError(t, err)
			browsePhotoMetadata(t, s, node, fmt.Sprint(i), photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(fmt.Sprintf("2024-01-%02d", i+1), fmt.Sprintf("2024-01-%02d", i+1), document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
		}
	}
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	otherAlbum, err := s.CreatePhotoSet(ctx, "Shared members")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(ctx, otherAlbum.ID, otherAlbum.Revision, true, PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	for i, id := range ids {
		_, err := s.db.ExecContext(ctx, `UPDATE photo_set_members SET added_at=? WHERE set_id=? AND asset_id=?`, fmt.Sprintf("2024-02-%02dT00:00:00.000000000Z", i+1), set.ID, id)
		require.NoError(t, err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET created_at='2024-01-01T00:00:00.000000000Z' WHERE kind='file'`)
	require.NoError(t, err)
	for _, sort := range []string{"added_time", "import_time", "capture_time"} {
		for _, direction := range []string{"asc", "desc"} {
			request := PhotoBrowseRequest{SetID: set.ID, Query: snapshotTestQuery(t, sprintfPhotoSort(sort, direction)), PageSize: 2}
			var boundary *PhotoBrowsePosition
			var got []string
			var keys []string
			for {
				page, err := s.ListPhotoAssets(ctx, request, boundary)
				require.NoError(t, err)
				require.Equal(t, int64(5), page.Total)
				for _, item := range page.Items {
					got = append(got, item.AssetID)
					if !item.position.Missing {
						keys = append(keys, item.position.Key)
					}
				}
				if page.Next == nil {
					break
				}
				boundary = page.Next
			}
			require.Len(t, got, 5)
			require.ElementsMatch(t, ids, got)
			want := slices.Clone(ids)
			switch sort {
			case "import_time":
				slices.Sort(want)
			case "added_time":
				if direction == "desc" {
					slices.Reverse(want)
				}
			case "capture_time":
				if direction == "desc" {
					slices.Reverse(want[:4])
				}
			}
			require.Equal(t, want, got, sort+" "+direction)
			if direction == "asc" {
				require.True(t, slices.IsSorted(keys))
			} else {
				require.True(t, slices.IsSortedFunc(keys, func(a, b string) int { return strings.Compare(b, a) }))
			}
		}
	}
	_, err = s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: snapshotTestQuery(t, sprintfPhotoSort("added_time", "asc"))}, nil)
	require.ErrorIs(t, err, ErrInvalidPhotoQuery)
	request := PhotoBrowseRequest{SetID: set.ID, Query: snapshotTestQuery(t, sprintfPhotoSort("added_time", "asc")), PageSize: 2}
	page, err := s.ListPhotoAssets(ctx, request, nil)
	require.NoError(t, err)
	require.NotNil(t, page.Next)
	other, err := s.CreatePhotoSet(ctx, "Other")
	require.NoError(t, err)
	request.SetID = other.ID
	_, err = s.ListPhotoAssets(ctx, request, page.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
	for _, raw := range []string{`{"filters":{"set_ids":["` + set.ID + `"]}}`, `{"syntax":"advanced","text":"set:` + set.ID + `"}`} {
		require.Equal(t, int64(5), browsePhotoPage(t, s, raw).Total)
	}
	asset, err := s.PhotoAssetByID(ctx, ids[0])
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	asset, err = s.PhotoAssetByID(ctx, ids[1])
	require.NoError(t, err)
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, set.ID, "")
	require.NoError(t, err)
	require.Equal(t, int64(5), summary.MemberCount)
	require.Equal(t, int64(3), summary.IncludedCount)
}

func TestPhotoSetCoverAndBackup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a := albumAsset(t, s, "first.jpg")
	b := albumAsset(t, s, "last.jpg")
	var recipe string
	for _, asset := range []PhotoAsset{a, b} {
		node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
		require.NoError(t, err)
		grid := visualPreviewRecipe()
		grid.MaxEdgePixels = 512
		g, err := s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("preview-"+asset.ID), 9, grid), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
		recipe = g.RecipeFingerprint
	}
	set, err := s.CreatePhotoSet(ctx, "Covers")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{a.ID, b.ID}})
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE photo_set_members SET added_at='2024-01-01T00:00:00.000000000Z' WHERE set_id=? AND asset_id=?`, set.ID, a.ID)
	require.NoError(t, err)
	summary, err := s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Equal(t, b.ID, *summary.EffectiveCoverAssetID)
	wrongRecipe, err := s.PhotoSet(ctx, set.ID, browseHash("old-recipe"))
	require.NoError(t, err)
	require.Nil(t, wrongRecipe.EffectiveCoverAssetID)
	cover := &a.ID
	set, err = s.UpdatePhotoSet(ctx, set.ID, set.Revision, nil, nil, &cover)
	require.NoError(t, err)
	summary, err = s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Equal(t, a.ID, *summary.EffectiveCoverAssetID)
	_, err = s.SetPhotoAssetExcluded(ctx, a.ID, a.Revision, true)
	require.NoError(t, err)
	summary, err = s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Equal(t, b.ID, *summary.EffectiveCoverAssetID)
	require.Equal(t, a.ID, *summary.CoverAssetID)
	node, err := s.NodeByID(ctx, b.Files[0].NodeID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("new-display"), 20, "image/jpeg")
	require.NoError(t, err)
	summary, err = s.PhotoSet(ctx, set.ID, recipe)
	require.NoError(t, err)
	require.Nil(t, summary.EffectiveCoverAssetID)
	deleted, err := s.CreatePhotoSet(ctx, "Deleted")
	require.NoError(t, err)
	_, err = s.DeletePhotoSet(ctx, deleted.ID, deleted.Revision)
	require.NoError(t, err)
	var original bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &original))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(original.Bytes())))
	var restored bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &restored))
	require.Equal(t, original.String(), restored.String())
	for _, replacement := range []string{
		strings.Replace(original.String(), `"cover_asset_id":"`+a.ID+`"`, `"cover_asset_id":"00000000-0000-4000-8000-000000000001"`, 1),
		strings.Replace(original.String(), `"type":"photo_set_member","set_id":"`+set.ID+`"`, `"type":"photo_set_member","set_id":"00000000-0000-4000-8000-000000000001"`, 1),
	} {
		invalid := newTestStore(t)
		require.Error(t, invalid.ImportMetadata(ctx, strings.NewReader(replacement)))
	}
}

func TestPhotoSetSelectionMatchesGroupedAndCollapsedBrowse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	raw := browsePhotoNode(t, s, "capture.raw", browseHash("raw-member"), "application/octet-stream")
	jpeg := browsePhotoNode(t, s, "capture.jpg", browseHash("jpeg-member"), "image/jpeg")
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	pair, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	pair, err = s.AttachPhotoFile(ctx, pair.ID, pair.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	browsePhotoMetadata(t, s, raw, "raw-camera", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Camera A")))
	browsePhotoMetadata(t, s, jpeg, "jpeg-lens", photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("Lens B")))
	hash := browseHash("duplicate")
	browsePhotoNode(t, s, "one.jpg", hash, "image/jpeg")
	browsePhotoNode(t, s, "two.jpg", hash, "image/jpeg")
	_, err = s.CreateSavedQuery(ctx, "Camera", "", SavedQueryKindQuery, []byte(`{"filters":{"cameras":["Camera A"]}}`))
	require.NoError(t, err)
	for _, text := range []string{
		`{"syntax":"advanced","text":"extension:jpg AND camera:\"Camera A\""}`,
		`{"filters":{"lenses":["Lens B"]}}`,
		`{"syntax":"advanced","text":"saved:Camera"}`,
		`{"filters":{"collapse_duplicates":true}}`,
	} {
		value := snapshotTestQuery(t, text)
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: value}, nil)
		require.NoError(t, err)
		want := make([]string, 0, len(page.Items))
		for _, row := range page.Items {
			want = append(want, row.AssetID)
		}
		set, err := s.CreatePhotoSet(ctx, "Selection")
		require.NoError(t, err)
		set, err = s.ChangePhotoSetMembers(ctx, set.ID, set.Revision, true, PhotoSetSelection{Query: &value})
		require.NoError(t, err)
		ids, err := photoSetMemberIDs(ctx, s.db, set.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, want, ids, text)
		if strings.Contains(text, "extension:jpg") {
			require.Equal(t, []string{pair.ID}, ids)
		}
		for _, filter := range []string{`{"filters":{"set_ids":["` + set.ID + `"]}}`, `{"syntax":"advanced","text":"set:` + set.ID + `"}`} {
			setPage := browsePhotoPage(t, s, filter)
			require.Equal(t, int64(len(want)), setPage.Total)
		}
	}
}
