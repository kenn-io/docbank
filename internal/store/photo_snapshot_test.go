package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
		stamp := fmt.Sprintf("2024-01-%02dT00:00:00Z", file.captureDay)
		browsePhotoMetadata(t, s, node, file.name, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneUTC, "")))
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		assetIDs = append(assetIDs, asset.ID)
		display := asset.Files[0]
		_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: display.ID, Revision: display.Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
		require.NoError(t, err)
		_, err = s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, fmt.Sprintf("2024-01-%02dT00:00:00.000000000Z", file.modifiedDay), node.ID)
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

func TestPhotoRankedSnapshotFreezesModeAndAssetOrder(t *testing.T) {
	t.Parallel()
	t.Run("reversed order across the page boundary", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		var members []SnapshotMember
		for i := range 51 {
			name := fmt.Sprintf("image-%02d.jpg", i)
			node := browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			members = append(members, SnapshotMember{AssetID: asset.ID, NodeID: node.ID, ContentVersionID: node.CurrentVersionID})
		}
		slices.Reverse(members)
		ranking := &PhotoRanking{RequestedMode: "semantic", ActualMode: "semantic", CoverageIncomplete: true, Members: members}
		service := NewQuerySnapshotService(s)
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		first, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: true, PageSize: 50, Query: snapshotTestQuery(t, `{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: ranking})
		require.NoError(t, err)
		require.Equal(t, int64(51), first.Total)
		require.Equal(t, members[0].AssetID, first.Rows[0].AssetID)
		current, err := service.CurrentPhotoPage(t.Context(), first, nil)
		require.NoError(t, err)
		require.Equal(t, "image-50.jpg", current.Items[0].Name)
		next, err := service.PhotoPage(t.Context(), "owner", first.SnapshotID, first.NextCursor, SnapshotRequest{Photos: true, Query: first.Query, PageSize: 50})
		require.NoError(t, err)
		require.Equal(t, "semantic", next.PhotoRanking.ActualMode)
		require.True(t, next.PhotoRanking.CoverageIncomplete)
		require.Equal(t, members[50].AssetID, next.Rows[0].AssetID)
	})
	t.Run("facet totals count only captured members", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		var members []SnapshotMember
		for i := range 70 {
			name := fmt.Sprintf("pixels-%02d.jpg", i)
			node := browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			if i < 66 {
				members = append(members, SnapshotMember{AssetID: asset.ID, NodeID: node.ID, ContentVersionID: node.CurrentVersionID})
			}
		}
		service := NewQuerySnapshotService(s)
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		first, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: true, PageSize: 25, Facets: []string{"camera", "capture_day"}, Query: snapshotTestQuery(t, `{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: &PhotoRanking{RequestedMode: "semantic", ActualMode: "semantic", Members: members}})
		require.NoError(t, err)
		require.Len(t, first.Rows, 25)
		for _, facet := range first.Facets {
			require.True(t, facet.Available)
			require.EqualValues(t, 66, *facet.Total)
		}
	})
	t.Run("member hidden before create is omitted", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ids := burstPhotos(t, s, 3)
		service := NewQuerySnapshotService(s)
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		all := createAllPhotosSnapshot(t, service)
		require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
		asset, err := s.PhotoAssetByID(t.Context(), ids[0])
		require.NoError(t, err)
		_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
		require.NoError(t, err)
		page := createRankedPhotosSnapshot(t, service, all, false)
		require.EqualValues(t, 2, page.Total)
		for _, row := range page.Rows {
			require.NotEqual(t, ids[0], row.AssetID)
		}
	})
}

// createAllPhotosSnapshot snapshots every photo in capture order so a ranked
// snapshot can reuse its members.
func createAllPhotosSnapshot(t *testing.T, service *QuerySnapshotService) SnapshotPage {
	t.Helper()
	all, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: true, Query: captureRequest(t, "asc").Query})
	require.NoError(t, err)
	return all
}

// createRankedPhotosSnapshot ranks the members of all by a semantic query.
func createRankedPhotosSnapshot(t *testing.T, service *QuerySnapshotService, all SnapshotPage, group bool) SnapshotPage {
	t.Helper()
	ranked, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: true, GroupPhotoStacks: group, Query: snapshotTestQuery(t, `{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: &PhotoRanking{RequestedMode: "semantic", ActualMode: "semantic", Members: snapshotMembers(all.Rows)}})
	require.NoError(t, err)
	return ranked
}

func TestPhotoSnapshotCapturePreservesMembersWithCurrentFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode string
		replace    bool
	}{
		{"replaced lexical content", "lexical", true},
		{"replaced ranked content", "semantic", true},
		{"current ranked flags", "semantic", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			var members []SnapshotMember
			var nodes []Node
			for _, name := range []string{"sunset-a.jpg", "sunset-b.jpg"} {
				node := browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
				asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				nodes = append(nodes, node)
				members = append(members, SnapshotMember{AssetID: asset.ID, NodeID: node.ID, ContentVersionID: node.CurrentVersionID})
			}
			_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: []string{members[1].AssetID}}), Patch: PhotoAuthoredPatch{Flag: new("pick")}})
			require.NoError(t, err)
			service := NewQuerySnapshotService(s)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			request := SnapshotRequest{Photos: true, Query: snapshotTestQuery(t, `{"text":"sunset","sort":{"field":"relevance","direction":"desc"}}`)}
			if tc.mode == "semantic" {
				request.Query.Mode = "semantic"
				request.PhotoRanking = &PhotoRanking{RequestedMode: "semantic", ActualMode: "semantic", Members: members}
			}
			first, err := service.Create(t.Context(), "owner", request)
			require.NoError(t, err)
			require.EqualValues(t, 2, first.Total)
			if tc.replace {
				_, _, err = s.ReplaceContent(t.Context(), nodes[0].ID, nodes[0].Revision, browseHash("replacement"), 20, nodes[0].MimeType)
				require.NoError(t, err)
				frozen, err := service.FirstPage(t.Context(), "owner", first.SnapshotID)
				require.NoError(t, err)
				require.Equal(t, first.Rows, frozen.Rows)
			} else {
				for i, flag := range []string{"pick", ""} {
					_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: []string{members[i].AssetID}}), Patch: PhotoAuthoredPatch{Flag: new(flag)}})
					require.NoError(t, err)
				}
			}
			capture, err := service.CapturePhotoSnapshot(t.Context(), "owner", first.SnapshotID, SnapshotRequest{PageSize: 50})
			require.NoError(t, err)
			require.Equal(t, first.Total, capture.Total)
			require.Equal(t, snapshotMembers(first.Rows), snapshotMembers(capture.Rows))
			require.Equal(t, first.MemberHash, capture.MemberHash)
			if !tc.replace {
				require.NotNil(t, capture.Rows[0].Flag)
				require.NotNil(t, capture.Rows[1].Flag)
				require.Equal(t, "pick", *capture.Rows[0].Flag)
				require.Empty(t, *capture.Rows[1].Flag)
			}
		})
	}
	t.Run("capture pages a burst crossing 250 rows", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "synthetic long burst")
		require.NoError(t, err)
		for i := range 251 {
			stamp := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			stackPhoto(t, s, run, fmt.Sprintf("/burst/frame-%03d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
		}
		browse, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`), PageSize: 1000}, nil)
		require.NoError(t, err)
		members := make([]SnapshotMember, len(browse.Items))
		for i, row := range browse.Items {
			members[i] = SnapshotMember{AssetID: row.AssetID, NodeID: row.NodeID, ContentVersionID: row.ContentVersionID}
		}
		service := NewQuerySnapshotService(s)
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		first, err := service.Create(t.Context(), "owner", SnapshotRequest{Photos: true, GroupPhotoStacks: true, PageSize: 250, Query: snapshotTestQuery(t, `{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: &PhotoRanking{RequestedMode: "semantic", ActualMode: "semantic", Members: members}})
		require.NoError(t, err)
		require.Len(t, first.Rows, 251)
		require.Empty(t, first.NextCursor)
		captured, err := service.CapturePhotoSnapshot(t.Context(), "owner", first.SnapshotID, SnapshotRequest{PageSize: 250})
		require.NoError(t, err)
		require.Len(t, captured.Rows, 250)
		require.False(t, captured.GroupPhotoStacks)
		require.NotEmpty(t, captured.NextCursor)
		last, err := service.Page(t.Context(), "owner", captured.SnapshotID, captured.NextCursor)
		require.NoError(t, err)
		require.Len(t, last.Rows, 1)
		require.Empty(t, last.NextCursor)
	})
}

func TestPhotoRankedSnapshotKeepsWholeStacksAtTheirBestRank(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fingerprint string
		incomplete        bool
	}{
		{"best rank across pages", "", false},
		{"outdated extractor metadata discloses incomplete grouping", browseHash("current-extractor"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic ranked stacks")
			require.NoError(t, err)
			a := stackPhoto(t, s, run, "/burst/a.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond)
			b := stackPhoto(t, s, run, "/other/b.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond)
			c := stackPhoto(t, s, run, "/burst/c.jpg", "2024-06-10T12:00:02Z", document.SourceMetadataPrecisionSecond)
			var members []SnapshotMember
			for _, id := range []string{c, b, a} {
				page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"asset_ids":[%q]}}`, id)), PageSize: 1}, nil)
				require.NoError(t, err)
				row := page.Items[0]
				members = append(members, SnapshotMember{AssetID: id, NodeID: row.NodeID, ContentVersionID: row.ContentVersionID})
			}
			service := NewQuerySnapshotService(s)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			first, err := service.Create(t.Context(), "owner", SnapshotRequest{SourceMetadataFingerprint: tc.fingerprint, Photos: true, GroupPhotoStacks: true, PageSize: 1, Query: snapshotTestQuery(t, `{"mode":"semantic","text":"sunset","sort":{"field":"relevance","direction":"desc"}}`), PhotoRanking: &PhotoRanking{RequestedMode: "semantic", ActualMode: "semantic", Members: members}})
			require.NoError(t, err)
			require.Equal(t, tc.incomplete, first.PhotoGroupingIncomplete)
			require.Len(t, first.Rows, 2)
			require.Equal(t, c, first.Rows[0].AssetID)
			require.Equal(t, a, first.Rows[1].AssetID)
			next, err := service.Page(t.Context(), "owner", first.SnapshotID, first.NextCursor)
			require.NoError(t, err)
			require.Len(t, next.Rows, 1)
			require.Equal(t, b, next.Rows[0].AssetID)
			previous, err := service.Page(t.Context(), "owner", first.SnapshotID, next.PrevCursor)
			require.NoError(t, err)
			require.Equal(t, first.Rows, previous.Rows)
		})
	}
	t.Run("representatives track choices, undo, flags, and trash", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		ids := burstPhotos(t, s, 4)
		service := NewQuerySnapshotService(s)
		t.Cleanup(func() { require.NoError(t, service.Close()) })
		ranked := createRankedPhotosSnapshot(t, service, createAllPhotosSnapshot(t, service), true)
		require.Len(t, ranked.PhotoStacks, 1)
		receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: "representative", CurrentID: ids[2]})
		require.NoError(t, err)
		stacks, err := service.PhotoStackRepresentatives(t.Context(), "owner", ranked.SnapshotID)
		require.NoError(t, err)
		require.Equal(t, ids[2], stacks[0].RepresentativeID)
		require.Equal(t, ranked.PhotoStacks[0].AssetIDs, stacks[0].AssetIDs)
		_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
		require.NoError(t, err)
		stacks, err = service.PhotoStackRepresentatives(t.Context(), "owner", ranked.SnapshotID)
		require.NoError(t, err)
		require.Equal(t, ids[0], stacks[0].RepresentativeID)
		_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[3:], Limit: MaxPhotoStackTargets}), Patch: PhotoAuthoredPatch{Rating: new(4), Flag: new("pick"), Rotation: new(90)}})
		require.NoError(t, err)
		stacks, err = service.PhotoStackRepresentatives(t.Context(), "owner", ranked.SnapshotID)
		require.NoError(t, err)
		require.Equal(t, ids[3], stacks[0].RepresentativeID)
		page, err := service.CurrentPhotoPage(t.Context(), ranked, nil)
		require.NoError(t, err)
		require.Len(t, page.Stacks, 1)
		require.Equal(t, ids[3], page.Stacks[0].RepresentativeID)
		require.Len(t, page.Items, 4)
		for _, row := range page.Items {
			if row.AssetID != ids[3] {
				continue
			}
			require.Equal(t, 4, row.Rating)
			require.Equal(t, "pick", row.Flag)
			require.Equal(t, 90, row.Rotation)
			asset, err := s.PhotoAssetByID(t.Context(), row.AssetID)
			require.NoError(t, err)
			require.Equal(t, asset.Revision, row.Revision)
		}
		after, err := service.FirstPage(t.Context(), "owner", ranked.SnapshotID)
		require.NoError(t, err)
		require.Equal(t, ranked.MemberHash, after.MemberHash)
		require.Equal(t, snapshotMembers(ranked.Rows), snapshotMembers(after.Rows))
		asset, err := s.PhotoAssetByID(t.Context(), ids[0])
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
		require.NoError(t, err)
		stacks, err = service.PhotoStackRepresentatives(t.Context(), "owner", ranked.SnapshotID)
		require.NoError(t, err)
		require.Len(t, stacks, 1)
		require.Equal(t, ids[1:], stacks[0].AssetIDs)
		require.Equal(t, ids[3], stacks[0].RepresentativeID)
		_, err = service.PhotoStackRepresentatives(t.Context(), "other", ranked.SnapshotID)
		require.ErrorIs(t, err, ErrSnapshotGone)
	})
}
