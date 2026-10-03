package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"slices"
	"strings"
	"testing"
)

func browsePhotoNode(t *testing.T, s *Store, name, hash, mime string) Node {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), name, hash, 20, mime)
	require.NoError(t, err)
	return node
}

func browsePhotoMetadata(t *testing.T, s *Store, node Node, fingerprint string, fields ...document.SourceMetadataFieldV1) {
	t.Helper()
	_, err := s.PublishSourceMetadata(t.Context(), node.BlobHash, browseHash(fingerprint), photoCanonical(t, fields...))
	require.NoError(t, err)
}

func browsePhotoPage(t *testing.T, s *Store, raw string) PhotoBrowsePage {
	t.Helper()
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, raw)}, nil)
	require.NoError(t, err)
	return page
}

func TestCompilePhotoPredicates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ typed, expression string }{
		{`{"kinds":["photo"]}`, `kind:photo`}, {`{"cameras":["Camera A"]}`, `camera:"Camera A"`}, {`{"lenses":["Lens B"]}`, `lens:"Lens B"`}, {`{"iso_min":0}`, `iso_min:0`}, {`{"iso_max":400}`, `iso_max:400`}, {`{"capture_after":"2024-01-01"}`, `capture_after:2024-01-01`}, {`{"capture_before":"2025-01-01"}`, `capture_before:2025-01-01`}, {`{"gps_bounds":{"south":"-1","west":"170","north":"1","east":"-170"}}`, `gps:"-1,170,1,-170"`}, {`{"asset_ids":["00000000-0000-4000-8000-000000000001"]}`, `asset:00000000-0000-4000-8000-000000000001`},
	} {
		typed, err := compileQuery(t.Context(), snapshotTestQuery(t, `{"filters":`+tc.typed+`}`), nil)
		require.NoError(t, err)
		expression, err := compileQuery(t.Context(), query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: tc.expression, Sort: query.Sort{Field: "name", Direction: "asc"}}, nil)
		require.NoError(t, err)
		require.Equal(t, typed.predicate.args, expression.predicate.args)
		require.Contains(t, typed.predicate.sql, "?")
	}
	for _, text := range []string{`camera:A*`, `kind:image`, `iso:1.0`, `capture_after:2023-02-29`, `gps:"0,0,91,0"`, `asset:bad`, `camera:foo NEAR/2 lens:bar`} {
		_, err := compileQuery(t.Context(), query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: text, Sort: query.Sort{Field: "name", Direction: "asc"}}, nil)
		require.Error(t, err)
	}
}

func TestPhotoBrowseISOAndAssetPredicates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, item := range []struct {
		name string
		iso  int64
	}{{"low.jpg", 200}, {"boundary.jpg", 400}, {"high.jpg", 800}} {
		node := browsePhotoNode(t, s, item.name, browseHash(item.name), "image/jpeg")
		browsePhotoMetadata(t, s, node, item.name, photoMetadataField("image.exif.iso", "image.exif", "ISO", photoInteger(item.iso)))
	}
	boundary := browsePhotoPage(t, s, `{"filters":{"iso_min":400,"iso_max":400}}`)
	require.Equal(t, int64(1), boundary.Total)
	require.Equal(t, "boundary.jpg", boundary.Items[0].Name)
	for _, raw := range []string{`{"filters":{"iso_max":400}}`, `{"syntax":"advanced","text":"iso_max:400"}`} {
		page := browsePhotoPage(t, s, raw)
		require.Equal(t, int64(2), page.Total, raw)
		require.ElementsMatch(t, []string{"low.jpg", "boundary.jpg"}, []string{page.Items[0].Name, page.Items[1].Name}, raw)
	}
	id := boundary.Items[0].AssetID
	for _, raw := range []string{`{"filters":{"asset_ids":["` + id + `"]}}`, `{"syntax":"advanced","text":"asset:` + id + `"}`} {
		page := browsePhotoPage(t, s, raw)
		require.Equal(t, int64(1), page.Total, raw)
		require.Equal(t, id, page.Items[0].AssetID, raw)
	}
}

func TestPhotoBrowseMissingDisplayMediaType(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	jpeg := browsePhotoNode(t, s, "untagged.jpg", browseHash("untagged-jpeg"), "")
	_, err := s.PhotoAssetForNode(t.Context(), jpeg.ID)
	require.NoError(t, err)
	raw := browsePhotoNode(t, s, "untagged.raw", browseHash("untagged-raw"), "")
	promoted, err := s.PromotePhotoNode(t.Context(), raw.ID, nil, PhotoRoleRAW, PhotoKindPhoto)
	require.NoError(t, err)
	for _, field := range []string{"name", "media_type"} {
		page := browsePhotoPage(t, s, fmt.Sprintf(`{"sort":{"field":%q,"direction":"asc"}}`, field))
		require.Equal(t, int64(2), page.Total)
		require.Len(t, page.Items, 2)
		for _, row := range page.Items {
			require.Empty(t, row.MediaType)
		}
		require.Contains(t, []string{page.Items[0].AssetID, page.Items[1].AssetID}, promoted.ID)
	}
}

func TestPhotoBrowsePagesPastLongSortKeys(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	shared := strings.Repeat("a", MaxPhotoSortKeyCharacters+100)
	want := map[string]bool{}
	for _, name := range []string{shared + "y.jpg", shared + "x.jpg", strings.Repeat("0", 17000) + ".jpg", "b.jpg"} {
		node := browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		want[asset.ID] = true
	}
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"name","direction":"asc"}}`), PageSize: 1}
	seen := map[string]bool{}
	var boundary *PhotoBrowsePosition
	for range want {
		page, err := s.ListPhotoAssets(t.Context(), request, boundary)
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		seen[page.Items[0].AssetID] = true
		boundary = page.Next
		if boundary != nil {
			require.LessOrEqual(t, len(boundary.Key), MaxPhotoSortKeyBytes)
		}
	}
	require.Nil(t, boundary)
	require.Equal(t, want, seen)
}

func TestPhotoBrowseMemberSemantics(t *testing.T) {
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
	browsePhotoMetadata(t, s, raw, "raw-fields", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Camera A")))
	browsePhotoMetadata(t, s, jpeg, "jpeg-fields", photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("Lens B")))
	_, err = s.CreateSavedQuery(ctx, "Camera match", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"camera:\"Camera A\""}`))
	require.NoError(t, err)
	for _, tc := range []struct {
		text  string
		count int64
	}{
		{`camera:"Camera A"`, 1}, {`lens:"Lens B"`, 1}, {`camera:"Camera A" AND lens:"Lens B"`, 0}, {`camera:"Camera A" OR lens:"Lens B"`, 1}, {`NOT camera:"Camera A"`, 1}, {`saved:"Camera match" AND lens:"Lens B"`, 0}, {`saved:"Camera match" OR lens:"Lens B"`, 1},
	} {
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: tc.text, Sort: query.Sort{Field: "name", Direction: "asc"}}}, nil)
		require.NoError(t, err, tc.text)
		require.Equal(t, tc.count, page.Total, tc.text)
		if tc.count > 0 {
			require.Len(t, page.Items, 1)
			require.Equal(t, pair.ID, page.Items[0].AssetID)
		}
	}
	sidecar := browsePhotoNode(t, s, "paired-sidecar.xmp", browseHash("paired-sidecar"), "application/octet-stream")
	rawID := fileByRole(pair.Files, PhotoRoleRAW).ID
	_, err = s.AttachPhotoFile(ctx, pair.ID, pair.Revision, sidecar.ID, PhotoRoleSidecar, &rawID)
	require.NoError(t, err)
	page := browsePhotoPage(t, s, `{"filters":{"extensions":["xmp"]}}`)
	require.Equal(t, int64(1), page.Total)
	require.NotEqual(t, sidecar.ID, page.Items[0].NodeID)
}

func TestPhotoBrowseActiveMetadata(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "active.jpg", browseHash("active-fields"), "image/jpeg")
	browsePhotoMetadata(t, s, node, "fields-a", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")))
	browsePhotoMetadata(t, s, node, "fields-b", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("B")))
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"cameras":["A"]}}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"filters":{"cameras":["B"]}}`).Total)
	shared := browsePhotoNode(t, s, "shared.jpg", node.BlobHash, "image/jpeg")
	require.NotEqual(t, node.ID, shared.ID)
	require.Equal(t, int64(2), browsePhotoPage(t, s, `{"filters":{"cameras":["B"]}}`).Total)
}

func TestPhotoBrowseEligibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	photo := browsePhotoNode(t, s, "included.jpg", browseHash("included"), "image/jpeg")
	video := browsePhotoNode(t, s, "included.mp4", browseHash("video"), "video/mp4")
	browsePhotoNode(t, s, "ordinary.txt", browseHash("ordinary"), "text/plain")
	require.Equal(t, int64(2), browsePhotoPage(t, s, `{}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"filters":{"kinds":["video"]}}`).Total)
	asset, err := s.PhotoAssetForNode(t.Context(), photo.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	page := browsePhotoPage(t, s, `{}`)
	require.Equal(t, int64(1), page.Total)
	require.Equal(t, video.ID, page.Items[0].NodeID)
	_, _, err = s.Trash(t.Context(), video.ID, video.Revision)
	require.NoError(t, err)
	require.Zero(t, browsePhotoPage(t, s, `{}`).Total)
}

func TestPhotoBrowsePagination(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range 7 {
		node := browsePhotoNode(t, s, fmt.Sprintf("capture-%d.jpg", i), browseHash(fmt.Sprintf("capture-%d", i)), "image/jpeg")
		if i < 5 {
			date := "2024-01-02T03:04:05.1234567891Z"
			if i == 3 {
				date = "2024-01-01T03:04:05.1234567891Z"
			}
			if i == 4 {
				date = "2024-01-03T03:04:05.1234567891Z"
			}
			if i == 0 {
				date = "0000-01-02T03:04:05.1234567891Z"
			}
			browsePhotoMetadata(t, s, node, fmt.Sprintf("capture-fields-%d", i), photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(date, date, document.SourceMetadataPrecisionFraction, document.SourceMetadataTimezoneUTC, "")))
		}
	}
	for _, direction := range []string{"asc", "desc"} {
		request := PhotoBrowseRequest{Query: snapshotTestQuery(t, sprintfPhotoSort("capture_time", direction)), PageSize: 2}
		seen := []string{}
		var boundary *PhotoBrowsePosition
		missingSeen := false
		previousKey := ""
		for {
			page, err := s.ListPhotoAssets(t.Context(), request, boundary)
			require.NoError(t, err)
			require.Equal(t, int64(7), page.Total)
			for _, row := range page.Items {
				require.NotContains(t, seen, row.AssetID)
				seen = append(seen, row.AssetID)
				if row.position.Missing {
					missingSeen = true
				} else {
					require.False(t, missingSeen)
					if previousKey != "" {
						if direction == "asc" {
							require.LessOrEqual(t, previousKey, row.position.Key)
						} else {
							require.GreaterOrEqual(t, previousKey, row.position.Key)
						}
					}
					previousKey = row.position.Key
				}
			}
			if page.Next == nil {
				break
			}
			boundary = page.Next
		}
		require.Len(t, seen, 7)
	}
	page := browsePhotoPage(t, s, `{"filters":{"capture_after":"2024-01-02","capture_before":"2024-01-03"}}`)
	require.Equal(t, int64(2), page.Total)
	all := browsePhotoPage(t, s, `{"sort":{"field":"capture_time"}}`)
	equalIDs := []string{}
	for _, row := range all.Items {
		if row.position.Key == "2024-01-02T03:04:05.123456789" {
			equalIDs = append(equalIDs, row.AssetID)
		}
	}
	require.True(t, slices.IsSorted(equalIDs))
	for _, field := range []string{"import_time", "name", "modified_at", "size", "media_type"} {
		require.Equal(t, int64(7), browsePhotoPage(t, s, sprintfPhotoSort(field, "desc")).Total)
	}
	for _, field := range []string{"path", "relevance"} {
		_, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, sprintfPhotoSort(field, "asc"))}, nil)
		require.Error(t, err)
	}
}

func sprintfPhotoSort(field, direction string) string {
	return fmt.Sprintf(`{"sort":{"field":%q,"direction":%q}}`, field, direction)
}

func TestPhotoBrowseSnapshotAndCursorBinding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range 3 {
		browsePhotoNode(t, s, fmt.Sprintf("saved-%d.jpg", i), browseHash(fmt.Sprintf("saved-%d", i)), "image/jpeg")
	}
	saved, err := s.CreateSavedQuery(t.Context(), "Photos", "", SavedQueryKindQuery, []byte(`{"filters":{"kinds":["photo"]}}`))
	require.NoError(t, err)
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:Photos"}`), PageSize: 1}
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.NotNil(t, page.Next)
	beyond := *page.Next
	beyond.Key = "zzzz"
	empty, err := s.ListPhotoAssets(t.Context(), request, &beyond)
	require.NoError(t, err)
	require.Empty(t, empty.Items)
	require.Equal(t, int64(3), empty.Total)
	changed := request
	changed.PageSize = 2
	_, err = s.ListPhotoAssets(t.Context(), changed, page.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
	_, err = s.UpdateSavedQuery(t.Context(), saved.ID, saved.Revision, SavedQueryPatch{Payload: new([]byte(`{"filters":{"kinds":["photo"],"extensions":["jpg"]}}`))})
	require.NoError(t, err)
	_, err = s.ListPhotoAssets(t.Context(), request, page.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
}

func TestPhotoBrowseDuplicateScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	hash := browseHash("identical")
	ordinary := browsePhotoNode(t, s, "ordinary.bin", hash, "application/octet-stream")
	_, err := s.db.ExecContext(t.Context(), `UPDATE nodes SET modified_at='2020-01-01T00:00:00.000000000Z' WHERE id=?`, ordinary.ID)
	require.NoError(t, err)
	browsePhotoNode(t, s, "one.jpg", hash, "image/jpeg")
	browsePhotoNode(t, s, "two.jpg", hash, "image/jpeg")
	require.Equal(t, int64(2), browsePhotoPage(t, s, `{}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"filters":{"collapse_duplicates":true}}`).Total)
}

func TestSnapshotRejectsPhotoSorts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, populated := range []bool{false, true} {
		if populated {
			browsePhotoNode(t, s, "sort.jpg", browseHash("sort-rejection"), "image/jpeg")
		}
		for _, field := range []string{"capture_time", "import_time"} {
			value := snapshotTestQuery(t, sprintfPhotoSort(field, "asc"))
			_, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value})
			require.ErrorContains(t, err, fmt.Sprintf("sort %q is only supported in Photos", field))
			saved, err := s.CreateSavedQuery(t.Context(), fmt.Sprintf("%s-%t", field, populated), "", SavedQueryKindQuery, []byte(sprintfPhotoSort(field, "asc")))
			require.NoError(t, err)
			options := defaultSnapshotMaterializeOptions()
			options.SavedQuery = &savedQuerySnapshotInput{ID: saved.ID, ExpectedRevision: saved.Revision}
			_, err = s.materializeQuerySnapshot(t.Context(), SnapshotRequest{}, options)
			require.ErrorContains(t, err, fmt.Sprintf("sort %q is only supported in Photos", field))
		}
	}
}

func TestPhotoBrowseCaptureTimeTolerance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i, stamp := range []string{"2024-01-02T3", "2024-01-02T3:04", "2024-01-02T3:04:05", "2024-01-02T3:04:05.123", "2024-01-02T03:04:05", "2024-01-02T03:04:05"} {
		node := browsePhotoNode(t, s, fmt.Sprintf("tolerant-%d.jpg", i), browseHash(fmt.Sprintf("tolerant-%d", i)), "image/jpeg")
		precision := []document.SourceMetadataTimestampPrecision{document.SourceMetadataPrecisionHour, document.SourceMetadataPrecisionMinute, document.SourceMetadataPrecisionSecond, document.SourceMetadataPrecisionFraction, document.SourceMetadataPrecisionSecond, document.SourceMetadataPrecisionSecond}[i]
		browsePhotoMetadata(t, s, node, fmt.Sprintf("tolerant-fields-%d", i), photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, precision, document.SourceMetadataTimezoneOmitted, "")))
		if i == 4 {
			_, err := s.db.ExecContext(t.Context(), `UPDATE photo_technical_metadata SET capture_time='malformed' WHERE generation_id=(SELECT generation_id FROM source_metadata_heads WHERE source_sha256=?)`, node.BlobHash)
			require.NoError(t, err)
		}
		if i == 5 {
			_, err := s.db.ExecContext(t.Context(), `UPDATE photo_technical_metadata SET capture_time_precision=NULL WHERE generation_id=(SELECT generation_id FROM source_metadata_heads WHERE source_sha256=?)`, node.BlobHash)
			require.NoError(t, err)
		}
	}
	browsePhotoNode(t, s, "absent.jpg", browseHash("tolerant-absent"), "image/jpeg")
	for _, direction := range []string{"asc", "desc"} {
		request := PhotoBrowseRequest{Query: snapshotTestQuery(t, sprintfPhotoSort("capture_time", direction)), PageSize: 2}
		var boundary *PhotoBrowsePosition
		seen := map[string]bool{}
		missing := 0
		previousKey := ""
		for {
			page, err := s.ListPhotoAssets(t.Context(), request, boundary)
			require.NoError(t, err)
			require.Equal(t, int64(7), page.Total)
			for _, row := range page.Items {
				require.False(t, seen[row.AssetID])
				seen[row.AssetID] = true
				if row.position.Missing {
					missing++
				} else {
					require.Zero(t, missing)
					if previousKey != "" {
						if direction == "asc" {
							require.LessOrEqual(t, previousKey, row.position.Key)
						} else {
							require.GreaterOrEqual(t, previousKey, row.position.Key)
						}
					}
					previousKey = row.position.Key
				}
				if row.Name == "tolerant-2.jpg" {
					require.Equal(t, "2024-01-02T3:04:05", *row.Fields.CaptureTime)
					require.Equal(t, "2024-01-02T03:04:05.000000000", row.position.Key)
				}
				if row.Name == "tolerant-4.jpg" {
					require.Equal(t, "malformed", *row.Fields.CaptureTime)
				}
			}
			if page.Next == nil {
				break
			}
			boundary = page.Next
		}
		require.Len(t, seen, 7)
		require.Equal(t, 3, missing)
	}
	for _, raw := range []string{`{"filters":{"capture_after":"2024-01-02","capture_before":"2024-01-03"}}`, `{"syntax":"advanced","text":"capture_after:2024-01-02 AND capture_before:2024-01-03"}`} {
		page := browsePhotoPage(t, s, raw)
		require.Equal(t, int64(4), page.Total)
		for _, row := range page.Items {
			require.NotContains(t, []string{"tolerant-4.jpg", "tolerant-5.jpg", "absent.jpg"}, row.Name)
		}
	}
}

func TestPhotoBrowsePreviewStates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "previews.jpg", browseHash("preview-source"), "image/jpeg")
	recipe := visualPreviewRecipe()
	recipe.MaxEdgePixels = 512
	canonical := readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("preview-output"), 9, recipe)
	ready, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, canonical, &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	failedRecipe := recipe
	failedRecipe.MaxEdgePixels = 2560
	failed, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, terminalVisualPreviewWithRecipe(t, node.BlobHash, failedRecipe, document.VisualPreviewFailed, "decode_failed"), nil)
	require.NoError(t, err)
	unsupported, err := s.PublishVisualPreviewGeneration(t.Context(), node.CurrentVersionID, terminalVisualPreviewWithRecipe(t, node.BlobHash, visualPreviewRecipe(), document.VisualPreviewUnsupported, "unsupported_media_type"), nil)
	require.NoError(t, err)
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`), Recipes: map[string]string{"grid": ready.RecipeFingerprint, "fit": failed.RecipeFingerprint, "large": unsupported.RecipeFingerprint}}
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, "ready", page.Items[0].Previews["grid"].State)
	require.Equal(t, "failed", page.Items[0].Previews["fit"].State)
	require.Equal(t, "unsupported", page.Items[0].Previews["large"].State)
}

func TestPhotoPreviewGenerationBinding(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "bound.jpg", browseHash("bound-source"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	generation, err := s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, readyVisualPreview(t, node.BlobHash, browseHash("bound-output"), 9), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	view, err := s.PhotoVisualPreviewByGeneration(ctx, asset.ID, generation.GenerationID)
	require.NoError(t, err)
	require.Equal(t, generation.Checksum, view.Generation.Checksum)
	other := browsePhotoNode(t, s, "other.jpg", browseHash("other-source"), "image/jpeg")
	otherAsset, err := s.PhotoAssetForNode(ctx, other.ID)
	require.NoError(t, err)
	_, err = s.PhotoVisualPreviewByGeneration(ctx, otherAsset.ID, generation.GenerationID)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("replacement-source"), 20, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PhotoVisualPreviewByGeneration(ctx, asset.ID, generation.GenerationID)
	require.ErrorIs(t, err, ErrNotFound)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.PhotoVisualPreviewByGeneration(ctx, asset.ID, generation.GenerationID)
	require.ErrorIs(t, err, ErrNotFound)
}

func browseHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestPhotoQueryUsesCompiledPopulation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "Synthetic photo collection")
	require.NoError(t, err)
	node, err := s.IngestFileExact(ctx, run, s.RootID(), "collected.jpg", browseHash("collected-photo"), 20, "image/jpeg", "collected.jpg", "")
	require.NoError(t, err)
	_, err = s.SetCollectionLabel(ctx, run.ID(), 1, new("Synthetic photo collection"))
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "photo-selection")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, node, "synthetic cobalt landscape")
	browsePhotoMetadata(t, s, node, "collected-fields", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Synthetic Camera")), photoMetadataField("image.exif.iso", "image.exif", "ISO", photoInteger(400)), photoMetadataField("image.exif.gps_latitude", "image.exif", "GPSLatitude", photoString("0")), photoMetadataField("image.exif.gps_longitude", "image.exif", "GPSLongitude", photoString("179")))
	_, err = s.CreateSavedQuery(ctx, "Photo source", "", SavedQueryKindQuery, []byte(`{"filters":{"cameras":["Synthetic Camera"]}}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Nested photo source", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Photo source\""}`))
	require.NoError(t, err)
	queryValue := snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:\"Nested photo source\" AND tag:photo-selection AND collection:\"Synthetic photo collection\" AND cobalt AND iso:400 AND gps:\"-1,170,1,-170\""}`)
	compiled, err := s.CompileQuery(ctx, queryValue)
	require.NoError(t, err)
	require.Len(t, compiled.Dependencies, 4)
	page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: queryValue}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	require.Equal(t, node.ID, page.Items[0].NodeID)
	snapshot, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: queryValue})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 1)
	require.Equal(t, node.ID, snapshot.Rows[0].NodeID)
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"iso_min":401}}`).Total)
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"gps_bounds":{"south":"-1","west":"-170","north":"1","east":"170"}}}`).Total)
}

func TestPhotoBrowseCurrentLiveMembers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "current.jpg", browseHash("current-a"), "image/jpeg")
	originalVersion := node.CurrentVersionID
	browsePhotoMetadata(t, s, node, "current-fields-a", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")))
	node, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("current-b"), 20, "image/jpeg")
	require.NoError(t, err)
	browsePhotoMetadata(t, s, node, "current-fields-b", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("B")))
	replacementVersion := node.CurrentVersionID
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"cameras":["A"]}}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"filters":{"cameras":["B"]}}`).Total)
	node, _, _, err = s.RevertContent(ctx, node.ID, node.Revision, originalVersion)
	require.NoError(t, err)
	require.NotEqual(t, originalVersion, node.CurrentVersionID)
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"cameras":["B"]}}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"filters":{"cameras":["A"]}}`).Total)
	_, err = s.PruneContentVersions(ctx, node.ID, node.Revision, VersionPruneSelector{VersionIDs: []string{replacementVersion}}, true)
	require.NoError(t, err)
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"cameras":["B"]}}`).Total)
	node, err = s.NodeByID(ctx, node.ID)
	require.NoError(t, err)
	directory, err := s.Mkdir(ctx, s.RootID(), "Trashed photos")
	require.NoError(t, err)
	_, _, err = s.Move(ctx, node.ID, directory.ID, node.Name, node.Revision)
	require.NoError(t, err)
	directory, err = s.NodeByID(ctx, directory.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, directory.ID, directory.Revision)
	require.NoError(t, err)
	require.Zero(t, browsePhotoPage(t, s, `{}`).Total)
}

func TestPhotoBrowseDisplayAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	raw := browsePhotoNode(t, s, "display.raw", browseHash("display-raw"), "application/octet-stream")
	jpeg := browsePhotoNode(t, s, "display.jpg", browseHash("display-jpeg"), "image/jpeg")
	jpegAsset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	page := browsePhotoPage(t, s, `{}`)
	require.Equal(t, raw.ID, page.Items[0].NodeID)
	require.Equal(t, raw.CreatedAt, page.Items[0].ImportTime)
	_, err = s.SetPhotoSettings(ctx, 1, new("image"))
	require.NoError(t, err)
	page = browsePhotoPage(t, s, `{}`)
	require.Equal(t, jpeg.ID, page.Items[0].NodeID)
	require.Equal(t, jpeg.CreatedAt, page.Items[0].ImportTime)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	rawID := fileByRole(asset.Files, PhotoRoleRAW).ID
	asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &rawID)
	require.NoError(t, err)
	require.Equal(t, raw.ID, browsePhotoPage(t, s, `{}`).Items[0].NodeID)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, rawID, PhotoDetachOptions{})
	require.NoError(t, err)
	require.Equal(t, jpeg.ID, browsePhotoPage(t, s, `{}`).Items[0].NodeID)
	jpegID := fileByRole(asset.Files, PhotoRoleImage).ID
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, jpegID, PhotoDetachOptions{})
	require.NoError(t, err)
	require.Zero(t, browsePhotoPage(t, s, `{}`).Total)
}

func TestPhotoBrowseReadSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node := browsePhotoNode(t, s, "snapshot.jpg", browseHash("snapshot-source"), "image/jpeg")
	canonicalA := photoCanonical(t, photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")))
	canonicalB := photoCanonical(t, photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("B")))
	_, err := s.PublishSourceMetadata(ctx, node.BlobHash, browseHash("snapshot-a"), canonicalA)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		for i := range 30 {
			canonical, fingerprint := canonicalA, browseHash("snapshot-a")
			if i%2 == 0 {
				canonical, fingerprint = canonicalB, browseHash("snapshot-b")
			}
			if _, err := s.PublishSourceMetadata(ctx, node.BlobHash, fingerprint, canonical); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"filters":{"cameras":["A"]}}`)}
	for range 60 {
		page, err := s.ListPhotoAssets(ctx, request, nil)
		require.NoError(t, err)
		require.Equal(t, int64(len(page.Items)), page.Total)
		if len(page.Items) > 0 {
			require.Equal(t, "A", *page.Items[0].Fields.CameraModel)
		}
	}
	require.NoError(t, <-done)
}
