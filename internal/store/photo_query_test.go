package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
	"slices"
	"strconv"
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
	page, err := browsePhotoFirst(t.Context(), t, s, PhotoBrowseRequest{Query: snapshotTestQuery(t, raw)})
	require.NoError(t, err)
	return page
}

func TestCompilePhotoPredicates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ typed, expression string }{
		{`{"rating_min":3}`, `rating_min:3`}, {`{"rating_min":4}`, `rating_min:4`}, {`{"rating_max":3}`, `rating_max:3`}, {`{"flags":["pick"]}`, `flag:pick`}, {`{"labels":["red"]}`, `label:red`},
		{`{"kinds":["photo"]}`, `kind:photo`}, {`{"cameras":["Camera A"]}`, `camera:"Camera A"`}, {`{"lenses":["Lens B"]}`, `lens:"Lens B"`}, {`{"locations":["Paris"]}`, `location:Paris`}, {`{"iso_min":0}`, `iso_min:0`}, {`{"iso_max":400}`, `iso_max:400`}, {`{"capture_after":"2024-01-01"}`, `capture_after:2024-01-01`}, {`{"capture_before":"2025-01-01"}`, `capture_before:2025-01-01`}, {`{"gps_bounds":{"south":"-1","west":"170","north":"1","east":"-170"}}`, `gps:"-1,170,1,-170"`}, {`{"asset_ids":["00000000-0000-4000-8000-000000000001"]}`, `asset:00000000-0000-4000-8000-000000000001`},
	} {
		typed, err := compileQuery(t.Context(), snapshotTestQuery(t, `{"filters":`+tc.typed+`}`), nil)
		require.NoError(t, err)
		expression, err := compileQuery(t.Context(), query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: tc.expression, Sort: query.Sort{Field: "name", Direction: "asc"}}, nil)
		require.NoError(t, err)
		require.Equal(t, typed.predicate.args, expression.predicate.args)
		require.Contains(t, typed.predicate.sql, strings.Trim(expression.predicate.sql, "()"))
		require.Contains(t, typed.predicate.sql, "?")
	}
	for _, text := range []string{`rating:6`, `rating:-1`, `rating:1.0`, `rating:5*`, `flag:yes`, `label:orange`, `camera:A*`, `kind:image`, `iso:1.0`, `capture_after:2023-02-29`, `gps:"0,0,91,0"`, `asset:bad`, `camera:foo NEAR/2 lens:bar`} {
		_, err := compileQuery(t.Context(), query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: text, Sort: query.Sort{Field: "name", Direction: "asc"}}, nil)
		require.Error(t, err)
	}
}

func TestCompilePhotoPredicatesReuseDocumentVersion(t *testing.T) {
	t.Parallel()
	for _, text := range []string{`camera:A`, `lens:B`, `iso:100`, `capture_after:2024-01-01`, `gps:"-1,-1,1,1"`, `focus_min:0.5`, `unevaluated:true`} {
		for _, photos := range []bool{false, true} {
			compiled, err := (queryCompiler{photoDisplayMetadata: photos}).compile(t.Context(), compilerQuery(t, text), nil)
			require.NoError(t, err, text)
			if photos {
				require.Contains(t, compiled.predicate.sql, "JOIN content_versions v ON v.version_id=display_node.current_version_id", text)
			} else {
				require.NotContains(t, compiled.predicate.sql, "FROM content_versions", text)
				require.Contains(t, compiled.predicate.sql, "cv.", text)
			}
		}
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

func TestPhotoBrowseDisplayMetadata(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	raw := browsePhotoNode(t, s, "capture.raw", browseHash("raw-member"), "application/octet-stream")
	jpeg := browsePhotoNode(t, s, "capture.jpg", browseHash("jpeg-member"), "image/jpeg")
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(jpeg), document.PhotoQualitySignals{Focus: 0.1}))
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
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(raw), document.PhotoQualitySignals{Focus: 0.8}))
	_, err = s.CreateSavedQuery(ctx, "Selected focus", "", SavedQueryKindQuery, []byte(`{"filters":{"focus_min":"0.7"}}`))
	require.NoError(t, err)
	qualityQuery := `{"syntax":"advanced","text":"saved:\"Selected focus\""}`
	require.Len(t, browsePhotoPage(t, s, qualityQuery).Items, 1)
	_, err = s.CreateSavedQuery(ctx, "Camera match", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"camera:\"Camera A\""}`))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Nested camera", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Camera match\""}`))
	require.NoError(t, err)
	for _, tc := range []struct {
		text  string
		count int64
	}{
		{`camera:"Camera A"`, 1}, {`lens:"Lens B"`, 0},
		{`camera:"Camera A" AND lens:"Lens B"`, 0}, {`camera:"Camera A" OR lens:"Lens B"`, 1},
		{`NOT camera:"Camera A"`, 0}, {`saved:"Camera match" AND lens:"Lens B"`, 0},
		{`saved:"Camera match" OR lens:"Lens B"`, 1},
		{`NOT lens:"Lens B"`, 1}, {`NOT iso:400`, 1}, {`NOT gps:"-1,-1,1,1"`, 1},
		{`saved:"Nested camera" AND NOT lens:"Lens B"`, 1},
	} {
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: tc.text, Sort: query.Sort{Field: "name", Direction: "asc"}}, Facets: []string{"camera", "lens", "year"}}, nil)
		require.NoError(t, err, tc.text)
		require.Equal(t, tc.count, page.Total, tc.text)
		for _, facet := range page.Facets {
			require.True(t, facet.Available, tc.text)
			require.Equal(t, tc.count, *facet.Total, tc.text)
		}
		if tc.count > 0 {
			require.Len(t, page.Items, 1)
			require.Equal(t, pair.ID, page.Items[0].AssetID)
		}
	}
	for _, tc := range []struct {
		text  string
		count int64
	}{
		{`"Lens B"`, 1}, {`"Camera A" AND "Lens B"`, 0}, {`"Camera A" OR "Lens B"`, 1},
		{`saved:"Camera match" AND "Lens B"`, 1}, {`saved:"Camera match" AND NOT "Lens B"`, 1},
	} {
		ranked := query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: tc.text, Sort: query.Sort{Field: "relevance", Direction: "desc"}}
		page, err := browsePhotoFirst(ctx, t, s, PhotoBrowseRequest{Query: ranked, Facets: []string{"camera", "lens"}})
		require.NoError(t, err, tc.text)
		require.Equal(t, tc.count, page.Total, tc.text)
		if tc.count > 0 {
			require.Equal(t, pair.ID, page.Items[0].AssetID)
			require.Equal(t, int64(1), *page.Facets[0].Total)
			require.Equal(t, "Camera A", page.Facets[0].Values[0].Key)
			require.Equal(t, int64(1), *page.Facets[1].Missing)
		}
	}
	sidecar := browsePhotoNode(t, s, "paired-sidecar.xmp", browseHash("paired-sidecar"), "application/octet-stream")
	rawID := fileByRole(pair.Files, PhotoRoleRAW).ID
	pair, err = s.AttachPhotoFile(ctx, pair.ID, pair.Revision, sidecar.ID, PhotoRoleSidecar, &rawID)
	require.NoError(t, err)
	page := browsePhotoPage(t, s, `{"filters":{"extensions":["xmp"]}}`)
	require.Equal(t, int64(1), page.Total)
	require.NotEqual(t, sidecar.ID, page.Items[0].NodeID)
	page = browsePhotoPage(t, s, `{"syntax":"advanced","text":"NOT camera:\"Camera A\""}`)
	require.Empty(t, page.Items, "a sidecar cannot bypass display metadata negation")
	page = browsePhotoPage(t, s, `{"syntax":"advanced","text":"extension:xmp AND camera:\"Camera A\""}`)
	require.Len(t, page.Items, 1, "ordinary member predicates combine with display metadata")
	_, err = s.SetPhotoDisplay(ctx, pair.ID, pair.Revision, new(fileByRole(pair.Files, PhotoRoleImage).ID))
	require.NoError(t, err)
	page = browsePhotoPage(t, s, `{"filters":{"lenses":["Lens B"]}}`)
	require.Len(t, page.Items, 1, "changing the display changes its metadata matches")
	require.Empty(t, browsePhotoPage(t, s, qualityQuery).Items)
	require.Len(t, browsePhotoPage(t, s, `{"syntax":"advanced","text":"extension:raw AND focus_max:0.2"}`).Items, 1)
	snapshot, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, qualityQuery)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 1)
	require.Equal(t, raw.ID, snapshot.Rows[0].NodeID)
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
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`), Facets: []string{"camera"}}
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, page.Total, *page.Facets[0].Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"filters":{"kinds":["video"]}}`).Total)
	asset, err := s.PhotoAssetForNode(t.Context(), photo.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	require.Equal(t, page.Total, *page.Facets[0].Total)
	require.Equal(t, video.ID, page.Items[0].NodeID)
	_, _, err = s.Trash(t.Context(), video.ID, video.Revision)
	require.NoError(t, err)
	require.Zero(t, browsePhotoPage(t, s, `{}`).Total)
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Zero(t, *page.Facets[0].Total)
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
	for _, field := range []string{"path"} {
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

func TestPhotoBrowseSavedDuplicateScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hash := browseHash("saved-duplicate-scope")
	older := browsePhotoNode(t, s, "older.jpg", hash, "image/jpeg")
	included := browsePhotoNode(t, s, "included.jpg", hash, "image/jpeg")
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET modified_at='2020-01-01T00:00:00.000000000Z' WHERE id=?`, older.ID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, older.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	browsePhotoMetadata(t, s, included, "duplicate-camera", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Camera A")))
	definition := `{"syntax":"advanced","text":"camera:\"Camera A\"","filters":{"collapse_duplicates":true}}`
	_, err = s.CreateSavedQuery(ctx, "Photo duplicates", "", SavedQueryKindQuery, []byte(definition))
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Nested duplicates", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"saved:\"Photo duplicates\""}`))
	require.NoError(t, err)
	for _, raw := range []string{definition, `{"syntax":"advanced","text":"saved:\"Photo duplicates\""}`, `{"syntax":"advanced","text":"saved:\"Nested duplicates\""}`} {
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: snapshotTestQuery(t, raw), Facets: []string{"camera", "lens"}}, nil)
		require.NoError(t, err)
		require.Len(t, page.Items, 1, raw)
		require.Equal(t, included.ID, page.Items[0].NodeID, raw)
		require.Equal(t, int64(1), *page.Facets[0].Total, raw)
	}
	compiled := compileFixtureQuery(t, s, `saved:"Photo duplicates"`, query.Filters{})
	require.Equal(t, []int64{older.ID}, compiledFixtureIDs(t, s.db, compiled, ""),
		"document queries retain the excluded photo as their duplicate representative")
}

func TestSnapshotRejectsPhotoSorts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, populated := range []bool{false, true} {
		if populated {
			browsePhotoNode(t, s, "sort.jpg", browseHash("sort-rejection"), "image/jpeg")
		}
		for _, field := range []string{"capture_time", "import_time", "added_time"} {
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
		value := photoTimestamp(stamp, stamp, precision, document.SourceMetadataTimezoneOmitted, "")
		if i >= 4 {
			// Source metadata can retain unreadable text without claiming a normalized timestamp.
			value = photoString("unreadable date")
		}
		browsePhotoMetadata(t, s, node, fmt.Sprintf("tolerant-fields-%d", i),
			photoMetadataField("created", "image.exif", "DateTimeOriginal", value))
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
					require.Nil(t, row.Fields.CaptureTime)
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

func TestPhotoBrowseLocalCaptureDate(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, tc := range []struct{ name, stamp, offset string }{
		{"january-1.jpg", "2024-01-01T23:30-08:00", "-08:00"},
		{"january-2.jpg", "2024-01-02T00:30+14:00", "+14:00"},
	} {
		node := browsePhotoNode(t, s, tc.name, browseHash(tc.name), "image/jpeg")
		browsePhotoMetadata(t, s, node, tc.name, photoMetadataField("created", "image.exif", "DateTimeOriginal",
			photoTimestamp(tc.stamp, tc.stamp, document.SourceMetadataPrecisionMinute,
				document.SourceMetadataTimezoneOffset, tc.offset)))
	}
	for _, raw := range []string{
		`{"filters":{"capture_after":"2024-01-01","capture_before":"2024-01-02"}}`,
		`{"syntax":"advanced","text":"capture_after:2024-01-01 AND capture_before:2024-01-02"}`,
	} {
		page := browsePhotoPage(t, s, raw)
		require.Len(t, page.Items, 1)
		require.Equal(t, "january-1.jpg", page.Items[0].Name)
	}
	page := browsePhotoPage(t, s, `{"sort":{"field":"capture_time","direction":"asc"}}`)
	require.Equal(t, "january-2.jpg", page.Items[0].Name, "sorting still uses the recorded offset")
}

func TestPhotoBrowseCameraAndLensIgnoreCase(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "camera.jpg", browseHash("camera-case"), "image/jpeg")
	browsePhotoMetadata(t, s, node, "camera-case",
		photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("SONY")),
		photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("Straße")))
	for _, raw := range []string{
		`{"filters":{"cameras":["sony"],"lenses":["STRASSE"]}}`,
		`{"syntax":"advanced","text":"camera:sony AND lens:STRASSE"}`,
	} {
		page := browsePhotoPage(t, s, raw)
		require.Len(t, page.Items, 1)
		require.Equal(t, node.ID, page.Items[0].NodeID)
	}
}

func TestPhotoBrowseContinuationRetainsFirstTotal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, name := range []string{"a.jpg", "b.jpg"} {
		browsePhotoNode(t, s, name, browseHash(name), "image/jpeg")
	}
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`), PageSize: 1}
	first, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.NotNil(t, first.Next)
	browsePhotoNode(t, s, "c.jpg", browseHash("c.jpg"), "image/jpeg")
	next, err := s.ListPhotoAssets(t.Context(), request, first.Next)
	require.NoError(t, err)
	require.Equal(t, int64(2), next.Total)
	require.Equal(t, "b.jpg", next.Items[0].Name)
	fresh, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, int64(3), fresh.Total)
}

func TestPhotoHiddenSavedDuplicateScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	hash := browseHash("saved-duplicate-scope")
	older := browsePhotoNode(t, s, "older.jpg", hash, "image/jpeg")
	included := browsePhotoNode(t, s, "included.jpg", hash, "image/jpeg")
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET modified_at='2020-01-01T00:00:00.000000000Z' WHERE id=?`, older.ID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, older.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	definition := `{"filters":{"collapse_duplicates":true}}`
	_, err = s.CreateSavedQuery(ctx, "Photo duplicates", "", SavedQueryKindQuery, []byte(definition))
	require.NoError(t, err)
	for _, raw := range []string{definition, `{"syntax":"advanced","text":"saved:\"Photo duplicates\""}`} {
		page := browsePhotoPage(t, s, raw)
		require.Len(t, page.Items, 1, raw)
		require.Equal(t, included.ID, page.Items[0].NodeID, raw)
	}
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	page, err := s.ListPhotoAssets(WithPhotoHiddenToken(ctx, token), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:\"Photo duplicates\""}`), Hidden: true}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, older.ID, page.Items[0].NodeID)
	compiled := compileFixtureQuery(t, s, `saved:"Photo duplicates"`, query.Filters{})
	require.Equal(t, []int64{older.ID}, compiledFixtureIDs(t, s.db, compiled, ""),
		"document queries retain the hidden photo as their duplicate representative")
}

func TestPhotoSearchFacetsRespectVisibilityAndAuthoredDecisions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "correct"))
	for i, rating := range []int{5, 5, 1} {
		node := browsePhotoNode(t, s, fmt.Sprintf("harbor-%d.jpg", i), browseHash(fmt.Sprint("scope", i)), "image/jpeg")
		browsePhotoMetadata(t, s, node, fmt.Sprint("scope", i), photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("Canon")))
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{asset.Files[0].ID, 1, PhotoAuthoredPatch{Rating: new(rating), Flag: new("pick"), Label: new("red")}}})
		require.NoError(t, err)
		if i == 1 {
			_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			require.NoError(t, err)
		}
	}
	_, err := s.CreateSavedQuery(ctx, "Rated harbor", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"harbor AND rating:5 AND flag:pick AND label:red","filters":{"collapse_duplicates":true}}`))
	require.NoError(t, err)
	token, _, err := s.UnlockPhotoHidden(ctx, "correct")
	require.NoError(t, err)
	for _, hidden := range []bool{false, true} {
		for _, field := range []string{"name", "relevance"} {
			for _, raw := range []string{`{"filters":{"rating_min":5,"flags":["pick"],"labels":["red"]}}`, `{"syntax":"advanced","text":"saved:\"Rated harbor\""}`} {
				value := snapshotTestQuery(t, raw)
				value.Sort.Field = field
				request := PhotoBrowseRequest{Query: value, Hidden: hidden, Facets: []string{"camera"}}
				page, err := browsePhotoFirst(WithPhotoHiddenToken(ctx, token), t, s, request)
				require.NoError(t, err)
				require.Equal(t, int64(1), page.Total)
				require.Len(t, page.Items, 1)
				require.Equal(t, int64(1), *page.Facets[0].Total)
				require.Equal(t, int64(1), page.Facets[0].Values[0].Count)
			}
		}
	}
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.ListPhotoAssets(WithPhotoHiddenToken(ctx, token), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{}`), Hidden: true, Facets: []string{"camera"}}, nil)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoBrowseRelevanceAndFacets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range 8 {
		node := browsePhotoNode(t, s, fmt.Sprintf("image-%d.jpg", i), browseHash(strconv.Itoa(i)), "image/jpeg")
		camera := "Canon EOS R6"
		if i == 7 {
			camera = "Nikon Z6"
		}
		browsePhotoMetadata(t, s, node, strconv.Itoa(i), photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString(camera)), photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString(strings.Fields(camera)[0])), photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("RF 24-70mm")), photoMetadataField("created", "image.exif", "DateTimeOriginal", document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &document.SourceMetadataTimestampV1{Raw: "2024-06-15T12:00:00", Normalized: "2024-06-15T12:00:00", Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneOmitted}}))
	}
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"text":"Canon","sort":{"field":"relevance","direction":"desc"}}`), PageSize: 3, Facets: []string{"camera", "lens", "year", "location", "set"}}
	var charges int
	service := newQuerySnapshotService(s, querySnapshotServiceOptions{ChargeHook: func(context.Context, string, int64, int64) error { charges++; return nil }})
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	first, err := service.CreatePhotoRanked(t.Context(), "owner", request)
	require.NoError(t, err)
	browsePhotoNode(t, s, "Canon later.jpg", browseHash("later-ranked-member"), "image/jpeg")
	snapshot := first
	var ids []string
	for {
		page, err := s.HydratePhotoRankedPage(t.Context(), request, snapshot)
		require.NoError(t, err)
		require.Equal(t, int64(7), page.Total)
		if snapshot.PrevCursor == "" {
			require.Len(t, page.Facets, 5)
			require.Equal(t, int64(7), page.Facets[0].Values[1].Count)
			require.Equal(t, "Canon EOS R6", page.Facets[0].Values[1].Key)
			require.Equal(t, "2024", page.Facets[2].Values[0].Key)
		} else {
			require.Empty(t, page.Facets)
		}
		for _, row := range page.Items {
			ids = append(ids, row.AssetID)
		}
		if snapshot.NextCursor == "" {
			break
		}
		before := charges
		snapshot, err = service.PagePhotoRanked(t.Context(), "owner", first.SnapshotID, snapshot.NextCursor, request)
		require.NoError(t, err)
		require.Equal(t, before, charges)
	}
	require.Positive(t, charges)
	require.Len(t, ids, 7)
	require.Len(t, slices.Compact(slices.Clone(ids)), 7)
	require.True(t, slices.IsSorted(ids), "equal scores use asset ID")
	request.Facets = []string{"camera"}
	_, err = service.PagePhotoRanked(t.Context(), "owner", first.SnapshotID, first.NextCursor, request)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
	request.Query = snapshotTestQuery(t, `{"filters":{"cameras":["Absent"]},"sort":{"field":"relevance","direction":"desc"}}`)
	page, err := browsePhotoFirst(t.Context(), t, s, request)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Zero(t, page.Total)
	require.Equal(t, int64(9), *page.Facets[0].Total)
	require.Equal(t, SnapshotFacetValue{Key: "Absent", Label: "Absent", Selected: true}, page.Facets[0].Values[4])
}

func mustPhotoCompiled(t *testing.T, s *Store, value query.Query) CompiledQuery {
	t.Helper()
	compiled, err := (queryCompiler{photoDisplayMetadata: true}).compile(t.Context(), value, queryResolver{q: s.db})
	require.NoError(t, err)
	return compiled
}

func TestPhotoLexicalActiveHeads(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "holiday.jpg", browseHash("holiday"), "image/jpeg")
	browsePhotoMetadata(t, s, node, "one", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Canon")), photoMetadataField("image.exif.gps_latitude", "image.exif", "GPSLatitude", photoString("48.8566")), photoMetadataField("image.exif.gps_longitude", "image.exif", "GPSLongitude", photoString("2.3522")))
	locationPage := browsePhotoPage(t, s, `{"text":"Paris","sort":{"field":"relevance"}}`)
	require.Equal(t, int64(1), locationPage.Total)
	label := *locationPage.Items[0].Fields.LocationLabel
	require.Equal(t, int64(1), browsePhotoPage(t, s, fmt.Sprintf(`{"filters":{"locations":[%q]}}`, label)).Total)
	require.Zero(t, browsePhotoPage(t, s, `{"filters":{"locations":["Paris"]}}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"text":"Canon","sort":{"field":"relevance"}}`).Total)
	browsePhotoMetadata(t, s, node, "two", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Nikon")))
	require.Zero(t, browsePhotoPage(t, s, `{"text":"Canon","sort":{"field":"relevance"}}`).Total)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"text":"Nikon","sort":{"field":"relevance"}}`).Total)
}

func TestPhotoFacetsFoldIdentityAndCountEachUnitOnce(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i, spelling := range []struct{ camera, lens string }{{"SONY", "Straße"}, {"Sony", "STRASSE"}} {
		node := browsePhotoNode(t, s, fmt.Sprintf("fold-%d.jpg", i), browseHash(fmt.Sprint("fold", i)), "image/jpeg")
		browsePhotoMetadata(t, s, node, fmt.Sprint("fold", i),
			photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString(spelling.camera)),
			photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("sony")),
			photoMetadataField("image.exif.lens_make", "image.exif", "LensMake", photoString(spelling.lens)),
			photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString("strasse")))
	}
	value := snapshotTestQuery(t, `{"filters":{"cameras":["SoNy"],"lenses":["STRASSE"]}}`)
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: value, Facets: []string{"camera", "lens"}}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total)
	{
		facets := page.Facets
		require.Equal(t, []SnapshotFacetValue{{Key: "SONY", Label: "SONY", Count: 2, Selected: true}}, facets[0].Values)
		require.Equal(t, []SnapshotFacetValue{{Key: "STRASSE", Label: "STRASSE", Count: 2, Selected: true}}, facets[1].Values)
		for _, facet := range facets {
			require.Equal(t, int64(2), *facet.Total)
			require.Zero(t, *facet.Missing)
		}
	}
	assetID := page.Items[0].AssetID
	assetIDs := []string{assetID, page.Items[1].AssetID}
	for _, name := range []string{"First album", "Second album"} {
		album, err := s.CreatePhotoSet(t.Context(), name)
		require.NoError(t, err)
		_, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: assetIDs})
		require.NoError(t, err)
	}
	value.Filters.AssetIDs = []string{assetID}
	albums, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: value, Facets: []string{"set"}}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), *albums.Facets[0].Total)
	require.Len(t, albums.Facets[0].Values, 2)
	require.Equal(t, int64(1), albums.Facets[0].Values[0].Count)
	require.Zero(t, *albums.Facets[0].Missing)
}

func TestPhotoYearFacetSelectionRequiresWholeYear(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		after, before string
		selected      bool
	}{
		{"2024-01-01", "2025-01-01", true}, {"2024-01-01", "2024-06-01", false}, {"2024-01-01", "", false}, {"9999-01-01", "", true},
	} {
		value := snapshotTestQuery(t, `{}`)
		value.Filters.CaptureAfter, value.Filters.CaptureBefore = tc.after, tc.before
		require.Equal(t, tc.selected, snapshotFacetSelected(value, "year")[tc.after[:4]])
	}
}

func TestPhotoLexicalMetadataUsesUncorrelatedPostingList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "plan.jpg", browseHash("plan"), "image/jpeg")
	browsePhotoMetadata(t, s, node, "plan", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Canon")))
	_, err := s.db.Exec(`WITH RECURSIVE synthetic(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM synthetic WHERE i<20000) INSERT INTO photo_metadata_fts(generation_id,text) SELECT 'synthetic-'||i,'Canon' FROM synthetic`)
	require.NoError(t, err)
	fragment := (queryCompiler{photoDisplayMetadata: true}).compileLexicalPredicate(`"Canon"`, true)
	statement, args, err := (CompiledQuery{predicate: fragment}).Bind("")
	require.NoError(t, err)
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT n.id FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id WHERE `+statement, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	steps := make(map[int]string)
	metadataParent := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
		steps[id] = detail
		if strings.Contains(detail, "photo_metadata_fts VIRTUAL TABLE INDEX") {
			metadataParent = parent
		}
	}
	require.NoError(t, rows.Err())
	t.Log(strings.Join(plan, "\n"))
	// SQLite materializes MATCH once; active-head lookups still correlate by blob.
	require.True(t, strings.HasPrefix(steps[metadataParent], "LIST SUBQUERY"), strings.Join(plan, "\n"))
	require.Contains(t, strings.Join(plan, "\n"), "photo_metadata_fts VIRTUAL TABLE INDEX")
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"text":"Canon"}`).Total)
}

func TestPhotoMetadataTextLeavesDocumentsUnchanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node := browsePhotoNode(t, s, "holiday.jpg", browseHash("document-text"), "image/jpeg")
	browsePhotoMetadata(t, s, node, "document-text", photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Canon")))
	value := snapshotTestQuery(t, `{"text":"Canon"}`)
	snapshot, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value})
	require.NoError(t, err)
	require.Empty(t, snapshot.Rows)
	hits, _, err := s.SearchPageWithOptions(t.Context(), "Canon", 50, SearchOptions{})
	require.NoError(t, err)
	require.Empty(t, hits)
	require.Equal(t, int64(1), browsePhotoPage(t, s, `{"text":"Canon"}`).Total)
}

func TestPhotoRankingRetainedContentEvidence(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"legacy", "active_generation"} {
		t.Run(source, func(t *testing.T) {
			s, _ := newRenditionCatalogFixture(t)
			profile := catalogProcessingProfile(t, false)
			var nodes []Node
			for i := range 2 {
				node := browsePhotoNode(t, s, fmt.Sprintf("retained-%d.jpg", i), browseHash(fmt.Sprint("retained", i)), "image/jpeg")
				nodes = append(nodes, node)
				text := "harbor " + strings.Repeat("filler ", 39)
				if i == 1 {
					text = strings.Repeat("harbor ", 20) + strings.Repeat("filler ", 20)
				}
				if source == "legacy" {
					require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: text}))
					_, err := s.db.Exec(`INSERT INTO text_searchable_versions(version_id) VALUES(?)`, node.CurrentVersionID)
					require.NoError(t, err)
				} else {
					build := lexicalSearchBuild(s, profile, browseHash(fmt.Sprint("retained-build", i)), text)
					build.SourceSHA256 = node.BlobHash
					require.NoError(t, s.StageRenditionBuild(t.Context(), build))
					attachment := RenditionAttachmentRecord{ID: browseHash(fmt.Sprint("retained-attachment", i)), VaultID: s.VaultID(), ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
					require.NoError(t, publishRenditionForTest(t, s, attachment, embeddingCatalogTime, browseHash(fmt.Sprint("retained-generation", i))))
				}
			}
			filename := browsePhotoNode(t, s, "harbor.jpg", browseHash("retained-filename"), "image/jpeg")
			request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"text":"harbor","sort":{"field":"relevance","direction":"desc"}}`), PageSize: 1}
			metadata := browsePhotoNode(t, s, "metadata.jpg", browseHash("retained-metadata"), "image/jpeg")
			browsePhotoMetadata(t, s, metadata, "retained-metadata-fields", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("harbor")))
			expected := []int64{filename.ID, metadata.ID, nodes[1].ID, nodes[0].ID}
			service := NewQuerySnapshotService(s)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			snapshot, err := service.CreatePhotoRanked(t.Context(), "owner", request)
			require.NoError(t, err)
			for _, id := range expected {
				page, err := s.HydratePhotoRankedPage(t.Context(), request, snapshot)
				require.NoError(t, err)
				require.Equal(t, int64(4), page.Total)
				require.Len(t, page.Items, 1)
				require.Equal(t, id, page.Items[0].NodeID)
				if snapshot.NextCursor != "" {
					snapshot, err = service.PagePhotoRanked(t.Context(), "owner", snapshot.SnapshotID, snapshot.NextCursor, request)
					require.NoError(t, err)
				}
			}
			require.Empty(t, snapshot.NextCursor)
			if source == "active_generation" {
				request.Coverage = CoverageSelection{Configuration: "configured", ProfileFingerprint: browseHash("other-profile")}
				page, err := browsePhotoFirst(t.Context(), t, s, request)
				require.NoError(t, err)
				require.Equal(t, int64(2), page.Total)
			}
		})
	}
}

func TestPhotoFacetRawOperandsAndBudgets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	raw := strings.Repeat("ß", 200)
	var invalidAssetID string
	var assetIDs []string
	for i, model := range []string{raw, strings.Repeat("ss", 200)} {
		node := browsePhotoNode(t, s, fmt.Sprintf("raw-operand-%d.jpg", i), browseHash(fmt.Sprint("raw-operand", i)), "image/jpeg")
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		assetIDs = append(assetIDs, asset.ID)
		date := "2024-02-01"
		if i == 1 {
			date = "2024-08-01"
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			invalidAssetID = asset.ID
		}
		browsePhotoMetadata(t, s, node, fmt.Sprint("raw-operand", i), photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString(model)), photoMetadataField("image.exif.lens_model", "image.exif", "LensModel", photoString(model)), photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(date, date, document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
	}
	oversized := strings.Repeat("x", 300)
	node := browsePhotoNode(t, s, "oversized.jpg", browseHash("oversized"), "image/jpeg")
	browsePhotoMetadata(t, s, node, "oversized", photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString(oversized)), photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp("2025-01-01", "2025-01-01", document.SourceMetadataPrecisionDate, document.SourceMetadataTimezoneOmitted, "")))
	value := snapshotTestQuery(t, `{}`)
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: value, Facets: []string{"camera", "year"}}, nil)
	require.NoError(t, err)
	facets := page.Facets
	require.Equal(t, SnapshotFacetValue{Key: raw, Label: raw, Count: 2}, facets[0].Values[0])
	require.Equal(t, int64(3), *facets[1].Total)
	page, err = s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: value, Facets: []string{"camera"}}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Facets[0].Values[0].Count)
	require.Equal(t, raw, page.Facets[0].Values[0].Key)
	require.Equal(t, SnapshotFacetValue{Key: oversized, Label: oversized, Count: 1}, page.Facets[0].Values[1])
	require.Zero(t, *page.Facets[0].Missing)
	options := defaultSnapshotMaterializeOptions()
	options.FacetMemberLimit = 1
	facets, err = materializeSharedPhotoFacets(t.Context(), s.db, mustPhotoCompiled(t, s, value), "", CoverageSelection{}, []string{"camera", "year"}, options)
	require.NoError(t, err)
	require.False(t, facets[0].Available)
	require.Equal(t, "member_budget_exceeded", facets[0].Reason)
	options.FacetMemberLimit = 100
	options.MaxSerializedBytes = 1
	facets, err = materializeSharedPhotoFacets(t.Context(), s.db, mustPhotoCompiled(t, s, value), "", CoverageSelection{}, []string{"camera", "year"}, options)
	require.NoError(t, err)
	require.Equal(t, "byte_budget_exceeded", facets[0].Reason)
	require.Equal(t, int64(2), browsePhotoPage(t, s, fmt.Sprintf(`{"filters":{"cameras":[%q]}}`, raw)).Total)
	for _, tc := range []struct{ dimension, filter string }{{"camera", "cameras"}, {"lens", "lenses"}} {
		selected := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"asset_ids":[%q],%q:[%q]}}`, invalidAssetID, tc.filter, raw))
		page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: selected, Facets: []string{tc.dimension}}, nil)
		require.NoError(t, err)
		facets := page.Facets
		require.Equal(t, SnapshotFacetValue{Key: raw, Label: raw, Count: 1, Selected: true}, facets[0].Values[0])
		require.Equal(t, int64(1), browsePhotoPage(t, s, fmt.Sprintf(`{"filters":{"asset_ids":[%q],%q:[%q]}}`, invalidAssetID, tc.filter, facets[0].Values[0].Key)).Total)
	}
	for _, name := range []string{"First album", "Second album"} {
		album, err := s.CreatePhotoSet(t.Context(), name)
		require.NoError(t, err)
		_, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: assetIDs})
		require.NoError(t, err)
	}
	budgetQuery := snapshotTestQuery(t, `{}`)
	budgetQuery.Filters.AssetIDs = assetIDs
	compiled := mustPhotoCompiled(t, s, budgetQuery)
	options = defaultSnapshotMaterializeOptions()
	options.FacetMemberLimit = 3
	for _, dimensions := range [][]string{{"camera"}, {"camera", "set"}, {"set", "camera"}, {"camera", "lens", "year", "location", "set"}} {
		facets, err := materializePhotoFacets(t.Context(), s.db, compiled, "", CoverageSelection{}, dimensions, options, nil)
		require.NoError(t, err)
		require.Len(t, facets, len(dimensions))
		for i, facet := range facets {
			require.Equal(t, dimensions[i], facet.Dimension)
			if facet.Dimension == "set" {
				require.False(t, facet.Available)
				require.Equal(t, "member_budget_exceeded", facet.Reason)
			} else {
				require.True(t, facet.Available, dimensions)
				require.Equal(t, int64(2), *facet.Total)
			}
		}
	}
	options.FacetMemberLimit = 4
	facets, err = materializePhotoFacets(t.Context(), s.db, compiled, "", CoverageSelection{}, []string{"set"}, options, nil)
	require.NoError(t, err)
	require.True(t, facets[0].Available)
	require.Len(t, facets[0].Values, 2)
	for _, value := range facets[0].Values {
		require.Equal(t, int64(2), value.Count)
	}
}

func TestPhotoOrdinarySeekAvoidsPopulationMaterialization(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	browsePhotoNode(t, s, "seek.jpg", browseHash("seek"), "image/jpeg")
	match, err := photoBrowseMatch(mustPhotoCompiled(t, s, snapshotTestQuery(t, `{}`)), "", CoverageSelection{})
	require.NoError(t, err)
	from, key, _ := photoBrowseOrder("name")
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT a.asset_id FROM `+from+` WHERE `+photoBrowseLiveDisplay+` AND `+match.sql+` AND `+key+`>? ORDER BY `+key+`,a.asset_id LIMIT 51`, append(match.args, "seek")...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NotContains(t, strings.Join(plan, "\n"), "MATERIALIZE")
	require.Contains(t, strings.Join(plan, "\n"), "SEARCH n USING INDEX nodes_photo_name")
}

func browsePhotoFirst(ctx context.Context, t *testing.T, s *Store, request PhotoBrowseRequest) (PhotoBrowsePage, error) {
	t.Helper()
	if request.Query.Sort.Field != "relevance" {
		return s.ListPhotoAssets(ctx, request, nil)
	}
	service := NewQuerySnapshotService(s)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	snapshot, err := service.CreatePhotoRanked(ctx, "owner", request)
	if err != nil {
		return PhotoBrowsePage{}, err
	}
	return s.HydratePhotoRankedPage(ctx, request, snapshot)
}
