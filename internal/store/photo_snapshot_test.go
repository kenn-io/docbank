package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestPhotoSnapshotOrderAndFlagsMatchBrowse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var assetIDs []string
	for _, file := range []struct {
		name, mime                    string
		size, captureDay, modifiedDay int
	}{
		{"harbor.jpg", "image/jpeg", 40, 2, 3},
		{"harbor harbor.png", "image/png", 10, 4, 2},
		{"harbor plain.gif", "image/gif", 30, 1, 4},
		{"harbor plain-two.webp", "image/webp", 20, 3, 1},
	} {
		node, err := s.CreateFile(t.Context(), s.RootID(), file.name, browseHash(file.name), int64(file.size), file.mime)
		require.NoError(t, err)
		_, err = s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, fmt.Sprintf("2024-01-%02dT00:00:00.000000000Z", file.modifiedDay), node.ID)
		require.NoError(t, err)
		stamp := fmt.Sprintf("2024-01-%02dT00:00:00Z", file.captureDay)
		browsePhotoMetadata(t, s, node, file.name, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneUTC, "")))
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		assetIDs = append(assetIDs, asset.ID)
		_, err = s.db.Exec(`UPDATE photo_files SET flag='pick' WHERE file_id=?`, asset.DisplayFileID)
		require.NoError(t, err)
	}
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	for _, field := range []string{"name", "relevance", "capture_time", "import_time", "modified_at", "size", "media_type", "added_time"} {
		value := snapshotTestQuery(t, fmt.Sprintf(`{"text":"harbor","sort":{"field":%q,"direction":"desc"}}`, field))
		if field == "added_time" {
			album, err := s.CreatePhotoSet(t.Context(), "Ordered")
			require.NoError(t, err)
			_, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: assetIDs})
			require.NoError(t, err)
			value.Filters.SetIDs = []string{album.ID}
		}
		browse, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: value}, nil)
		require.NoError(t, err)
		page, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: true, Query: value})
		require.NoError(t, err)
		require.Len(t, page.Rows, 4)
		for i, row := range page.Rows {
			require.Equal(t, browse.Items[i].AssetID, row.AssetID, field)
			require.Equal(t, new("pick"), row.Flag, field)
		}
	}
}

func TestPhotoSnapshotPageChecksRequestedMembershipAndPreservesFrozenRows(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"excluded", "moved node to hidden asset", "benign edits"} {
		t.Run(mutation, func(t *testing.T) {
			s := newTestStore(t)
			original := authoredPair(t, s)
			assetIDs := make([]string, 51)
			assetIDs[0] = original.ID
			for i := 1; i < len(assetIDs); i++ {
				name := fmt.Sprintf("target-%02d.jpg", i)
				node := browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
				asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				assetIDs[i] = asset.ID
			}
			service := NewQuerySnapshotService(s)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			first, err := service.Create(t.Context(), "owner", SnapshotRequest{
				Photos: true, PageSize: 50, Query: snapshotTestQuery(t, `{"sort":{"field":"name","direction":"asc"}}`),
			})
			require.NoError(t, err)
			require.NotEmpty(t, first.NextCursor)
			if mutation == "benign edits" {
				next, err := service.Page(t.Context(), "owner", first.SnapshotID, first.NextCursor)
				require.NoError(t, err)
				var frozenFile PhotoFile
				for _, file := range original.Files {
					if file.NodeID == first.Rows[0].NodeID {
						frozenFile = file
					}
				}
				require.NotEmpty(t, frozenFile.ID)
				_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: frozenFile.ID, Revision: frozenFile.Revision, Patch: PhotoAuthoredPatch{Caption: new("Updated caption"), Flag: new("reject")}}})
				require.NoError(t, err)
				tag, err := s.CreateTag(t.Context(), "Later tag")
				require.NoError(t, err)
				display, err := s.NodeByID(t.Context(), frozenFile.NodeID)
				require.NoError(t, err)
				_, err = s.AssignTag(t.Context(), tag.ID, display.ID, display.Revision)
				require.NoError(t, err)
				display, err = s.NodeByID(t.Context(), display.ID)
				require.NoError(t, err)
				_, _, err = s.ReplaceContent(t.Context(), display.ID, display.Revision, browseHash("replacement-display"), 20, display.MimeType)
				require.NoError(t, err)
				page, err := service.Page(t.Context(), "owner", first.SnapshotID, next.PrevCursor)
				require.NoError(t, err)
				require.Equal(t, first.Rows, page.Rows)
				return
			}
			if mutation == "moved node to hidden asset" {
				next, err := service.Page(t.Context(), "owner", first.SnapshotID, first.NextCursor)
				require.NoError(t, err)
				var frozenFile PhotoFile
				for _, file := range original.Files {
					if file.NodeID == first.Rows[0].NodeID {
						frozenFile = file
					}
				}
				require.NotEmpty(t, frozenFile.ID)
				original, err = s.DetachPhotoFile(t.Context(), original.ID, original.Revision, frozenFile.ID, PhotoDetachOptions{ClearDependentSidecars: true})
				require.NoError(t, err)
				require.NotNil(t, original.DisplayFileID)
				node := browsePhotoNode(t, s, "destination.jpg", browseHash("destination"), "image/jpeg")
				destination, err := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				destination, err = s.AttachPhotoFile(t.Context(), destination.ID, destination.Revision, frozenFile.NodeID, frozenFile.Role, nil)
				require.NoError(t, err)
				moved, err := service.Page(t.Context(), "owner", first.SnapshotID, next.PrevCursor)
				require.NoError(t, err)
				require.Equal(t, first.Rows, moved.Rows)
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(t.Context(), destination.ID, destination.Revision, true)
				require.NoError(t, err)
				page, err := service.Page(t.Context(), "owner", first.SnapshotID, next.PrevCursor)
				require.ErrorIs(t, err, ErrPhotoSnapshotMemberNotInLibrary)
				require.Empty(t, page.Rows)
				return
			}
			for _, id := range []string{assetIDs[0], assetIDs[50]} {
				asset, err := s.PhotoAssetByID(t.Context(), id)
				require.NoError(t, err)
				_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
				require.NoError(t, err)
				page, err := service.Page(t.Context(), "owner", first.SnapshotID, first.NextCursor)
				if id == assetIDs[0] {
					require.NoError(t, err)
					require.Len(t, page.Rows, 1)
					require.Equal(t, assetIDs[50], page.Rows[0].AssetID)
				} else {
					require.ErrorIs(t, err, ErrPhotoSnapshotMemberNotInLibrary)
				}
			}
		})
	}
}

func TestPhotoSnapshotRunRejectsMemberLeavingLibrary(t *testing.T) {
	t.Parallel()
	for name, leaving := range map[string]int{"first page": 0, "beyond first page": 50} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			assets := make([]PhotoAsset, 51)
			for i := range assets {
				assets[i] = albumAsset(t, s, fmt.Sprintf("photo-%02d.jpg", i))
			}
			saved, err := s.CreateSavedQuery(t.Context(), "Photos", "", SavedQueryKindQuery, []byte(`{}`))
			require.NoError(t, err)
			service := newQuerySnapshotService(s, querySnapshotServiceOptions{
				SavedRunBeforeCommitHook: func(ctx context.Context) error {
					asset := assets[leaving]
					_, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
					return err
				},
			})
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			_, _, err = service.RunSaved(t.Context(), "owner", saved.ID, saved.Revision, SnapshotRequest{Photos: true, PageSize: 50}, nil)
			require.ErrorIs(t, err, ErrPhotoSnapshotMemberNotInLibrary)
			require.Zero(t, savedQueryRunCount(t, s, saved.ID, true))
		})
	}
}

func TestPhotoSnapshotCreatePhotoAlbum(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"trashed", "RAW detached from a kept asset", "empty result", "Documents snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			value := snapshotTestQuery(t, `{"sort":{"field":"name"}}`)
			var asset PhotoAsset
			var frozenOrder []string
			if scenario == "RAW detached from a kept asset" {
				asset = authoredPair(t, s)
				assets := []PhotoAsset{asset, albumAsset(t, s, "first.cr2"), albumAsset(t, s, "last.cr2"), albumAsset(t, s, "middle.cr2")}
				slices.SortFunc(assets, func(a, b PhotoAsset) int { return strings.Compare(a.ID, b.ID) })
				for i, member := range assets {
					for _, file := range member.Files {
						if member.ID != asset.ID || file.Role == PhotoRoleRAW {
							// Descending names deliberately reverse UUID order.
							_, err := s.db.Exec(`UPDATE nodes SET name=? WHERE id=?`, fmt.Sprintf("harbor-%d.cr2", i), file.NodeID)
							require.NoError(t, err)
						}
					}
				}
				value = snapshotTestQuery(t, `{"syntax":"advanced","text":"name:cr2","sort":{"field":"name","direction":"desc"}}`)
			} else if scenario != "empty result" {
				asset = albumAsset(t, s, "harbor.jpg")
			}
			service := NewQuerySnapshotService(s)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			page, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: scenario != "Documents snapshot", Query: value})
			require.NoError(t, err)
			switch scenario {
			case "trashed":
				_, err = s.db.Exec(`UPDATE nodes SET trashed_at=? WHERE id=?`, nowRFC3339(), asset.Files[0].NodeID)
			case "RAW detached from a kept asset":
				require.Len(t, page.Rows, 4)
				for _, row := range page.Rows {
					frozenOrder = append(frozenOrder, row.AssetID)
				}
				var raw PhotoFile
				for _, file := range asset.Files {
					if file.Role == PhotoRoleRAW {
						raw = file
					}
				}
				require.NotEmpty(t, raw.ID)
				asset, err = s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, raw.ID, PhotoDetachOptions{ClearDependentSidecars: true})
				require.NoError(t, err)
				require.NotNil(t, asset.DisplayFileID)
				require.EqualValues(t, 3, browsePhotoPage(t, s, `{"syntax":"advanced","text":"name:cr2"}`).Total)
			}
			require.NoError(t, err)
			album, err := service.CreatePhotoAlbum(t.Context(), "owner", page.SnapshotID, page.MemberHash, "Frozen", nil)
			if scenario == "Documents snapshot" {
				require.ErrorIs(t, err, ErrInvalidPhotoAlbum)
				return
			}
			if scenario == "trashed" {
				require.ErrorIs(t, err, ErrPhotoSnapshotMemberNotInLibrary)
				var albums, receipts int
				require.NoError(t, s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM photo_sets),(SELECT COUNT(*) FROM photo_change_receipts WHERE operation LIKE 'set_%')`).Scan(&albums, &receipts))
				require.Zero(t, albums)
				require.Zero(t, receipts)
				return
			}
			require.NoError(t, err)
			stored, err := s.PhotoSet(t.Context(), album.ID, "")
			require.NoError(t, err)
			require.Equal(t, "Frozen", stored.Name)
			var receipts int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_change_receipts WHERE set_id=? AND operation='set_create'`, album.ID).Scan(&receipts))
			require.Equal(t, 1, receipts)
			ids, err := photoSetMemberIDs(t.Context(), s.db, album.ID)
			require.NoError(t, err)
			switch scenario {
			case "empty result":
				require.Empty(t, ids)
			case "RAW detached from a kept asset":
				require.ElementsMatch(t, frozenOrder, ids)
				browse := browsePhotoPage(t, s, fmt.Sprintf(`{"filters":{"set_ids":[%q]},"sort":{"field":"added_time","direction":"desc"}}`, album.ID))
				var actual []string
				for _, row := range browse.Items {
					actual = append(actual, row.AssetID)
				}
				require.Equal(t, frozenOrder, actual)
			}
		})
	}
}
