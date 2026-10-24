package store

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// stackPhoto imports one synthetic frame from source. An empty stamp leaves
// only the file's modified time.
func stackPhoto(t *testing.T, s *Store, run IngestRun, source, stamp string, precision document.SourceMetadataTimestampPrecision, fields ...document.SourceMetadataFieldV1) string {
	t.Helper()
	name := browseHash(source)[:6] + "-" + path.Base(strings.ReplaceAll(source, `\`, "/"))
	node, err := s.IngestFileExact(t.Context(), run, s.RootID(), name, browseHash(source), 20, "image/jpeg", source, "2024-01-01T00:00:00Z")
	require.NoError(t, err)
	if stamp != "" {
		zone, offset := document.SourceMetadataTimezoneOmitted, ""
		if strings.HasSuffix(stamp, "Z") {
			zone = document.SourceMetadataTimezoneUTC
		} else if len(stamp) > 19 && (stamp[len(stamp)-6] == '+' || stamp[len(stamp)-6] == '-') {
			zone, offset = document.SourceMetadataTimezoneOffset, stamp[len(stamp)-6:]
		}
		fields = append(fields, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, precision, zone, offset)))
		browsePhotoMetadata(t, s, node, source, fields...)
	}
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	return asset.ID
}

func burstPhotos(t *testing.T, s *Store, count int) []string {
	t.Helper()
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic bursts")
	require.NoError(t, err)
	ids := make([]string, count)
	for i := range ids {
		stamp := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		ids[i] = stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%04d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
	}
	return ids
}

func stackIndexes(t *testing.T, s *Store, ids []string) [][]int {
	t.Helper()
	groups, unavailable, _, _, err := s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	result := [][]int{}
	for _, group := range groups {
		indexes := make([]int, len(group))
		for i, id := range group {
			indexes[i] = slices.Index(ids, id)
		}
		result = append(result, indexes)
	}
	return result
}

func captureRequest(t *testing.T, direction string) PhotoBrowseRequest {
	t.Helper()
	return PhotoBrowseRequest{Stacks: true, PageSize: 1, Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"`+direction+`"}}`)}
}

// stackPages reads every page and returns each page's items and stacks.
func stackPages(t *testing.T, s *Store, request PhotoBrowseRequest) ([][]string, [][]string) {
	t.Helper()
	var items, stacks [][]string
	var boundary *PhotoBrowsePosition
	for {
		page, err := s.ListPhotoAssets(t.Context(), request, boundary)
		require.NoError(t, err)
		items = append(items, rowIDs(page.Items))
		for _, stack := range page.Stacks {
			stacks = append(stacks, stack.AssetIDs)
		}
		if page.Next == nil {
			return items, stacks
		}
		boundary = page.Next
	}
}

func rowIDs(rows []PhotoBrowseRow) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.AssetID
	}
	return ids
}

func TestPhotoStackRelevancePage(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic ranked bursts")
	require.NoError(t, err)
	first := stackPhoto(t, s, run, "/shots/frame-1.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond)
	single := stackPhoto(t, s, run, "/other/frame-2.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond)
	last := stackPhoto(t, s, run, "/shots/frame-3.jpg", "2024-06-10T12:00:02Z", document.SourceMetadataPrecisionSecond)
	request := PhotoBrowseRequest{Stacks: true, PageSize: 3, Query: snapshotTestQuery(t, `{"text":"frame","sort":{"field":"relevance","direction":"desc"}}`)}
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, []string{last, first, single}, rowIDs(page.Items))
	require.Equal(t, []PhotoStack{{AssetIDs: []string{first, last}, RepresentativeID: first}}, page.Stacks)
	require.Nil(t, page.Next)

	// Relevance has a single bounded page. A sibling outside it stays out of
	// both the shown stack and the decision membership.
	request.PageSize = 2
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, []string{last, single}, rowIDs(page.Items))
	require.Empty(t, page.Stacks)
	require.EqualValues(t, 3, page.Total)

	groups, unavailable, _, _, err := s.PhotoStackGroups(t.Context(), []string{last, single, first}, false, "")
	require.NoError(t, err)
	require.Empty(t, unavailable)
	require.Equal(t, [][]string{{last, first}}, groups)
}

func TestPhotoStackOtherKeysetSortsKeepIndividualRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	request := PhotoBrowseRequest{Stacks: true, PageSize: 1, Query: snapshotTestQuery(t, `{"sort":{"field":"name","direction":"asc"}}`)}
	pages, stacks := stackPages(t, s, request)
	require.Empty(t, stacks)
	require.Len(t, pages, 3)
	for _, page := range pages {
		require.Len(t, page, 1)
	}
	require.ElementsMatch(t, ids, slices.Concat(pages...))
}

func TestPhotoStackDescendingTimestampTies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic tied captures")
	require.NoError(t, err)
	var ids []string
	for i := range 5 {
		ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/shots/tie-%d.jpg", i), "2024-06-10T12:00:02Z", document.SourceMetadataPrecisionSecond))
	}
	slices.Sort(ids)
	setCamera := func(id, stamp, model string) {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
		require.NoError(t, err)
		_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, browseHash(id+model), photoCanonical(t,
			photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneUTC, "")),
			photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString(model))))
		require.NoError(t, err)
	}
	for i, model := range []string{"A", "B", "C", "A", "C"} {
		setCamera(ids[i], "2024-06-10T12:00:02Z", model)
	}
	older := stackPhoto(t, s, run, "/shots/older.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond,
		photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("A")))
	request := captureRequest(t, "desc")
	first, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.NotNil(t, first.Next)
	t.Run("display and pages", func(t *testing.T) {
		require.Equal(t, []string{ids[0], ids[3], older}, rowIDs(first.Items))
		require.Equal(t, []string{older, ids[0], ids[3]}, first.Stacks[0].AssetIDs)
		pages, _ := stackPages(t, s, request)
		require.Equal(t, [][]string{{ids[0], ids[3], older}, {ids[1]}, {ids[2], ids[4]}}, pages)
	})
	t.Run("frontier preserves future ties", func(t *testing.T) {
		setCamera(ids[4], "2024-06-10T12:00:02Z", "D")
		next, err := s.ListPhotoAssets(t.Context(), request, first.Next)
		require.NoError(t, err, "a future-only stack change must leave the cursor valid")
		require.Equal(t, []string{ids[1]}, rowIDs(next.Items))
	})
}

func TestPhotoStackTimeRules(t *testing.T) {
	t.Parallel()
	var fixtures []struct {
		Name string `json:"name"`
		Gap  int    `json:"gap"`
		Rows []struct {
			Time      string `json:"time"`
			Precision string `json:"precision"`
			Folder    string `json:"folder"`
			Make      string `json:"make"`
			Model     string `json:"model"`
		} `json:"rows"`
		Stacks [][]int `json:"stacks"`
	}
	data, err := os.ReadFile("testdata/photo-bursts.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &fixtures))
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic bursts")
			require.NoError(t, err)
			ids := make([]string, len(fixture.Rows))
			for i, row := range fixture.Rows {
				var fields []document.SourceMetadataFieldV1
				for _, field := range []struct{ key, source, value string }{{"camera_make", "Make", row.Make}, {"camera_model", "Model", row.Model}} {
					if field.value != "" {
						fields = append(fields, photoMetadataField("image.exif."+field.key, "image.exif", field.source, photoString(field.value)))
					}
				}
				ids[i] = stackPhoto(t, s, run, fmt.Sprintf("/%s/frame-%d.jpg", row.Folder, i), row.Time, document.SourceMetadataTimestampPrecision(row.Precision), fields...)
			}
			_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{BurstGapSeconds: new(fixture.Gap)})
			require.NoError(t, err)
			require.Equal(t, fixture.Stacks, stackIndexes(t, s, ids))
			// Library pages probe neighbors in SQL; they must find the same stacks.
			_, stacks := stackPages(t, s, captureRequest(t, "asc"))
			paged := [][]int{}
			for _, stack := range stacks {
				indexes := make([]int, len(stack))
				for i, id := range stack {
					indexes[i] = slices.Index(ids, id)
				}
				slices.Sort(indexes)
				paged = append(paged, indexes)
			}
			slices.SortFunc(paged, func(a, b []int) int { return a[0] - b[0] })
			require.Equal(t, fixture.Stacks, paged)
		})
	}
}

func TestPhotoStackCameraSerialMatching(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, first, second string
		grouped             bool
	}{
		{"equal", "Body-001", "Body-001", true},
		{"missing", "", "", true},
		{"known and missing", "Body-001", "", false},
		{"missing and known", "", "Body-001", false},
		{"case sensitive", "Body-001", "BODY-001", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic serial matching")
			require.NoError(t, err)
			var ids []string
			for i, serial := range []string{tc.first, tc.second} {
				var fields []document.SourceMetadataFieldV1
				if serial != "" {
					fields = append(fields, photoMetadataField("image.exif.body_serial_number", "image.exif", "BodySerialNumber", photoString(serial)))
				}
				ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%d.jpg", i), fmt.Sprintf("2024-06-10T12:00:0%dZ", i), document.SourceMetadataPrecisionSecond, fields...))
			}
			groups := stackIndexes(t, s, ids)
			if tc.grouped {
				require.Equal(t, [][]int{{0, 1}}, groups)
			} else {
				require.Empty(t, groups)
			}
		})
	}
}

func TestPhotoStackFoldersFollowSourceProvenance(t *testing.T) {
	t.Parallel()
	t.Run("restored caller references stay opaque", func(t *testing.T) {
		s := newTestStore(t)
		run, err := s.BeginCallerSuppliedIngest(t.Context(), "agent", "synthetic source")
		require.NoError(t, err)
		ids := []string{
			stackPhoto(t, s, run, "/shared/frame-0.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond),
			stackPhoto(t, s, run, "/shared/frame-1.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond),
		}
		var exported bytes.Buffer
		require.NoError(t, s.ExportMetadata(t.Context(), &exported))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(t.Context(), strings.NewReader(exported.String())))
		require.Empty(t, stackIndexes(t, restored, ids))
		page, err := restored.ListPhotoAssets(t.Context(), captureRequest(t, "asc"), nil)
		require.NoError(t, err)
		require.Empty(t, page.Stacks)
		require.Len(t, page.Items, 1)
	})
	t.Run("push and watch namespaces", func(t *testing.T) {
		s := newTestStore(t)
		var ids []string
		for i, source := range []string{"camera-a", "camera-a", "camera-b", "camera-b"} {
			name := fmt.Sprintf("frame-%d.jpg", i)
			node, outcome, err := s.AcceptPush(t.Context(), PushSource{Name: source, Ref: "DCIM/" + name}, PushContent{RunID: testRunID(t), ParentPath: "/" + source, Name: name, Hash: browseHash(source + name), Size: 20, MIMEType: "image/jpeg"})
			require.NoError(t, err)
			require.Equal(t, "added", outcome)
			browsePhotoMetadata(t, s, node, source+name, exifCapture(fmt.Sprintf("2024-06-10T12:00:0%dZ", i)))
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			ids = append(ids, asset.ID)
		}
		for i, source := range []string{"watch-a", "watch-a", "watch-b"} {
			run, err := s.BeginIngest(t.Context(), "watch", source)
			require.NoError(t, err)
			ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("DCIM/watch-%d.jpg", i), "2024-06-10T13:00:00Z", document.SourceMetadataPrecisionSecond))
		}
		require.Equal(t, [][]int{{0, 1}, {2, 3}, {4, 5}}, stackIndexes(t, s, ids))
	})
}

func TestPhotoStackLibraryPagesQueryScopedBursts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic interleaved imports")
	require.NoError(t, err)
	// A1,B1,A2,B2 interleave by capture time; X is a lone frame between them.
	a1 := stackPhoto(t, s, run, "/A/harbor-1.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond)
	b1 := stackPhoto(t, s, run, "/B/field-1.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond)
	a2 := stackPhoto(t, s, run, "/A/harbor-2.jpg", "2024-06-10T12:00:02Z", document.SourceMetadataPrecisionSecond)
	b2 := stackPhoto(t, s, run, "/B/field-2.jpg", "2024-06-10T12:00:03Z", document.SourceMetadataPrecisionSecond)
	x := stackPhoto(t, s, run, "/X/lone.jpg", "2024-06-10T12:30:00Z", document.SourceMetadataPrecisionSecond)
	undated := stackPhoto(t, s, run, "/A/undated.jpg", "", "")
	asc := captureRequest(t, "asc")
	// The file-time photo sorts by its earlier modified time and never joins a stack.
	items, stacks := stackPages(t, s, asc)
	require.Equal(t, [][]string{{undated}, {a1, a2}, {b1, b2}, {x}}, items)
	require.Equal(t, [][]string{{a1, a2}, {b1, b2}}, stacks)
	items, _ = stackPages(t, s, captureRequest(t, "desc"))
	require.Equal(t, [][]string{{x}, {b2, b1}, {a2, a1}, {undated}}, items)
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, PageSize: 2, Query: asc.Query}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{undated, a1, a2}, rowIDs(page.Items), "the page finishes its last stack")
	require.Equal(t, int64(6), page.Total)
	require.Equal(t, a1, page.Stacks[0].RepresentativeID)
	// The query decides membership: excluding B leaves A's frames and no hidden siblings.
	filtered := asc
	filtered.Query = snapshotTestQuery(t, `{"text":"harbor","sort":{"field":"capture_time","direction":"asc"}}`)
	items, stacks = stackPages(t, s, filtered)
	require.Equal(t, [][]string{{a1, a2}}, items)
	require.Equal(t, [][]string{{a1, a2}}, stacks)
	album, err := s.CreatePhotoSet(t.Context(), "Burst album")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{a1, a2}})
	require.NoError(t, err)
	albumQuery := snapshotTestQuery(t, fmt.Sprintf(`{"filters":{"set_ids":[%q]},"sort":{"field":"capture_time","direction":"asc"}}`, album.ID))
	items, stacks = stackPages(t, s, PhotoBrowseRequest{Stacks: true, PageSize: 1, Query: albumQuery})
	require.Equal(t, [][]string{{a1, a2}}, items)
	require.Equal(t, [][]string{{a1, a2}}, stacks)
	// Without stacks, pages keep the plain capture-time keyset.
	plain := asc
	plain.Stacks = false
	items, stacks = stackPages(t, s, plain)
	require.Equal(t, [][]string{{undated}, {a1}, {b1}, {a2}, {b2}, {x}}, items)
	require.Empty(t, stacks)
	// A frame imported between pages joins the live result.
	first, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, PageSize: 3, Query: asc.Query}, nil)
	require.NoError(t, err)
	b3 := stackPhoto(t, s, run, "/B/field-3.jpg", "2024-06-10T12:00:04Z", document.SourceMetadataPrecisionSecond)
	second, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, PageSize: 3, Query: asc.Query}, first.Next)
	require.NoError(t, err)
	require.Equal(t, []string{b1, b2, b3}, rowIDs(second.Items))
	require.Equal(t, int64(6), second.Total)
	missing := "00000000-0000-4000-8000-000000000009"
	groups, unavailable, gap, _, err := s.PhotoStackGroups(t.Context(), []string{b2, a2, missing, b1, a1}, false, "")
	require.NoError(t, err)
	require.Equal(t, [][]string{{b2, b1}, {a2, a1}}, groups)
	require.Equal(t, []string{missing}, unavailable)
	require.Equal(t, 2, gap)
	require.NotContains(t, slices.Concat(groups...), b3)
	// Changing the gap invalidates cursors bound to the old grouping.
	_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{BurstGapSeconds: new(1)})
	require.NoError(t, err)
	_, err = s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, PageSize: 3, Query: asc.Query}, first.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
	items, _ = stackPages(t, s, asc)
	require.Equal(t, [][]string{{undated}, {a1}, {b1}, {a2}, {b2, b3}, {x}}, items)
}

func TestPhotoStackGroupsRejectInvalidInputs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	a1 := burstPhotos(t, s, 1)[0]
	_, _, gap, incomplete, err := s.PhotoStackGroups(t.Context(), []string{a1, a1}, false, "")
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.Zero(t, gap)
	require.False(t, incomplete)
	_, _, gap, incomplete, err = s.PhotoStackGroups(t.Context(), []string{a1}, true, "")
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Zero(t, gap)
	require.False(t, incomplete)
}

func TestPhotoStackCursorMembershipFrontier(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"asc", "desc"} {
		t.Run(direction+"/camera edit", func(t *testing.T) {
			s := newTestStore(t)
			ids := burstPhotos(t, s, 3)
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic cursor neighbors")
			require.NoError(t, err)
			for i, stamp := range []string{"2024-06-10T11:00:00Z", "2024-06-10T13:00:00Z"} {
				stackPhoto(t, s, run, fmt.Sprintf("/other/frame-%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond)
			}
			request := captureRequest(t, direction)
			request.PageSize = 2
			first, err := s.ListPhotoAssets(t.Context(), request, nil)
			require.NoError(t, err)
			require.NotNil(t, first.Next)
			asset, err := s.PhotoAssetByID(t.Context(), ids[1])
			require.NoError(t, err)
			node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
			require.NoError(t, err)
			_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, browseHash("camera-edit"), photoCanonical(t,
				photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp("2024-06-10T12:00:01Z", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneUTC, "")),
				photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("R6"))))
			require.NoError(t, err)
			_, err = s.ListPhotoAssets(t.Context(), request, first.Next)
			require.ErrorIs(t, err, ErrInvalidPhotoCursor)
		})
		for _, offset := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("%s/boundary-%d", direction, offset), func(t *testing.T) {
				s := newTestStore(t)
				run, err := s.BeginIngest(t.Context(), "cli", "synthetic cursor frontier")
				require.NoError(t, err)
				var ids []string
				for i := range 4 {
					ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/A/frame-%d.jpg", i), fmt.Sprintf("2024-06-10T12:00:0%dZ", i), document.SourceMetadataPrecisionSecond))
				}
				joined := ids[3]
				second := 1 + min(offset, 1)
				if direction == "desc" {
					joined = ids[0]
					second = 1 - min(offset, 1)
				}
				base := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
				stamp := base.Add(time.Duration(second)*time.Second + 500*time.Millisecond)
				other := stackPhoto(t, s, run, "/B/other.jpg", stamp.Format(time.RFC3339Nano), document.SourceMetadataPrecisionFraction)
				expected := append(slices.Clone(ids), other)
				if offset == 2 {
					step := time.Second
					if direction == "desc" {
						step = -step
					}
					expected = append(expected, stackPhoto(t, s, run, "/B/other-2.jpg", stamp.Add(step).Format(time.RFC3339Nano), document.SourceMetadataPrecisionFraction))
				}
				expected = append(expected, stackPhoto(t, s, run, "/C/end.jpg", base.Add(10*time.Minute).Format(time.RFC3339), document.SourceMetadataPrecisionSecond), stackPhoto(t, s, run, "/C/start.jpg", base.Add(-10*time.Minute).Format(time.RFC3339), document.SourceMetadataPrecisionSecond))
				_, err = s.db.Exec(`UPDATE photo_files SET flag='reject' WHERE asset_id=?`, joined)
				require.NoError(t, err)
				request := captureRequest(t, direction)
				request.PageSize = 4
				if offset == 2 {
					request.PageSize = 5
				}
				request.Query = snapshotTestQuery(t, `{"filters":{"flags":[""]},"sort":{"field":"capture_time","direction":"`+direction+`"}}`)
				first, err := s.ListPhotoAssets(t.Context(), request, nil)
				require.NoError(t, err)
				require.NotNil(t, first.Next)
				unchanged, err := s.ListPhotoAssets(t.Context(), request, first.Next)
				require.NoError(t, err)
				if offset < 2 {
					require.Contains(t, rowIDs(unchanged.Items), other)
				} else {
					require.Len(t, unchanged.Items, 1)
				}
				_, err = s.db.Exec(`UPDATE photo_files SET flag='' WHERE asset_id=?`, joined)
				require.NoError(t, err)
				_, err = s.ListPhotoAssets(t.Context(), request, first.Next)
				require.ErrorIs(t, err, ErrInvalidPhotoCursor)
				pages, stacks := stackPages(t, s, request)
				require.Contains(t, stacks, ids)
				require.ElementsMatch(t, expected, slices.Concat(pages...))
			})
		}
	}
}

func TestPhotoStackPagesMatchExplicitGrouping(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic interleaved bursts")
	require.NoError(t, err)
	// Three folders interleave with steps of 0 to 3 seconds, so chains break,
	// tie, and cross every page and batch edge below.
	steps := []int{0, 1, 3, 2, 0, 1, 1, 3, 3, 2, 1, 0, 2, 3, 1, 2, 0, 3, 1, 1}
	base := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC)
	var ids []string
	for folder, offset := range []int{0, 1, 2} {
		stamp := base.Add(time.Duration(offset) * time.Second)
		for i := range steps {
			stamp = stamp.Add(time.Duration(steps[(i+folder*7)%len(steps)]) * time.Second)
			ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/roll-%d/frame-%02d.jpg", folder, i), stamp.Format(time.RFC3339), document.SourceMetadataPrecisionSecond))
		}
	}
	groups, _, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.False(t, incomplete)
	want := make([][]string, len(groups))
	for i, group := range groups {
		want[i] = slices.Sorted(slices.Values(group))
	}
	require.NotEmpty(t, want)
	for _, size := range []int{1, 2, 3, 7, 50} {
		for _, direction := range []string{"asc", "desc"} {
			request := captureRequest(t, direction)
			request.PageSize = size
			items, stacks := stackPages(t, s, request)
			got := make([][]string, len(stacks))
			for i, stack := range stacks {
				got[i] = slices.Sorted(slices.Values(stack))
			}
			require.ElementsMatch(t, want, got, "size %d %s", size, direction)
			require.ElementsMatch(t, ids, slices.Concat(items...), "size %d %s: every photo appears once", size, direction)
		}
	}
}

func TestPhotoStackPagesCapConnectedRuns(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 501)
	require.Empty(t, stackIndexes(t, s, ids))
	for _, direction := range []string{"asc", "desc"} {
		page, err := s.ListPhotoAssets(t.Context(), captureRequest(t, direction), nil)
		require.NoError(t, err)
		require.Len(t, page.Items, 1, "a 501-frame run stays as individual photos")
		require.Empty(t, page.Stacks)
	}
	// Continuing inside the oversized run keeps every row individual.
	request := captureRequest(t, "asc")
	request.PageSize = 250
	var cursor *PhotoBrowsePosition
	var listed []string
	for {
		page, err := s.ListPhotoAssets(t.Context(), request, cursor)
		require.NoError(t, err)
		require.Empty(t, page.Stacks)
		listed = append(listed, rowIDs(page.Items)...)
		if cursor = page.Next; cursor == nil {
			break
		}
	}
	require.Equal(t, ids, listed)
	trashed, err := s.PhotoAssetByID(t.Context(), ids[500])
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	for _, direction := range []string{"asc", "desc"} {
		items, stacks := stackPages(t, s, captureRequest(t, direction))
		require.Len(t, items, 1)
		require.Len(t, items[0], 500)
		require.Len(t, stacks, 1)
	}
	receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids[:500], Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[:500], Limit: MaxPhotoStackTargets}), CurrentID: ids[0], Decision: "all"})
	require.NoError(t, err)
	require.Len(t, receipt.After, 500)
}

func TestPhotoStackFileLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image")})
	require.NoError(t, err)
	ids := burstPhotos(t, s, 334)
	for i, id := range ids {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		for j := range 2 {
			node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("companion-%d-%d.cr2", i, j), browseHash(fmt.Sprintf("companion-%d-%d", i, j)), 4, "application/octet-stream")
			require.NoError(t, err)
			asset, err = s.AttachPhotoFile(t.Context(), id, asset.Revision, node.ID, PhotoRoleRAW, nil)
			require.NoError(t, err)
		}
	}
	indexes := make([]int, 334)
	for i := range indexes {
		indexes[i] = i
	}
	require.Equal(t, [][]int{indexes}, stackIndexes(t, s, ids))
	attach := func(n int) {
		t.Helper()
		asset, err := s.PhotoAssetByID(t.Context(), ids[n%len(ids)])
		require.NoError(t, err)
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("extra-%d.cr2", n), browseHash(fmt.Sprintf("extra-%d", n)), 4, "application/octet-stream")
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, node.ID, PhotoRoleRAW, nil)
		require.NoError(t, err)
	}
	// Reach the stack's exact bound of 2,000 files through the same import API.
	for n := 1002; n < 2000; n++ {
		attach(n)
	}
	targets := preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	require.Len(t, targets, 2000)
	require.Equal(t, [][]int{indexes}, stackIndexes(t, s, ids))
	attach(2000)
	groups, _, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, "")
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Empty(t, groups)
	_, err = s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	require.ErrorIs(t, err, ErrPhotoStackFileLimit)
}

func TestPhotoStackReturnOwnership(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"replace", "rating", "selected", "membership", "undo", "undo undo", "restore", "pair", "selected barrier", "clear", "repick", "manual undo", "manual undo undo", "manual backup"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t)
			ids := burstPhotos(t, s, 3)
			if scenario == "pair" {
				asset, err := s.PhotoAssetByID(t.Context(), ids[0])
				require.NoError(t, err)
				raw, err := s.CreateFile(t.Context(), s.RootID(), "frame-0000.cr2", fakeHash("d4"), 4, "application/octet-stream")
				require.NoError(t, err)
				_, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, raw.ID, PhotoRoleRAW, nil)
				require.NoError(t, err)
				_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image")})
				require.NoError(t, err)
			}
			decide := func(decision, id string) PhotoAuthoredReceipt {
				receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: decision, CurrentID: id, SelectedIDs: ids[:1]})
				require.NoError(t, err)
				return receipt
			}
			checkFlags := func(want []string) {
				flags := make([]string, len(ids))
				for i, id := range ids {
					asset, err := s.PhotoAssetByID(t.Context(), id)
					require.NoError(t, err)
					flags[i] = asset.Files[0].Flag
				}
				require.Equal(t, want, flags)
			}
			decide("current", ids[0])
			wantFirst := "reject"
			finalCurrent := ids[1]
			switch scenario {
			case "rating":
				_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[:1]}), Patch: PhotoAuthoredPatch{Rating: new(4)}})
				require.NoError(t, err)
			case "selected":
				decide("current", ids[1])
				decide("selected", ids[0])
				wantFirst = "pick"
			case "membership":
				ids = ids[:2]
			case "undo", "undo undo", "restore":
				second := decide("current", ids[1])
				undo, err := s.UndoPhotoAuthored(t.Context(), second.ReceiptID)
				require.NoError(t, err)
				if scenario == "undo" || scenario == "undo undo" {
					_, err = s.UndoPhotoAuthored(t.Context(), undo.ReceiptID)
					require.NoError(t, err)
				}
				if scenario == "undo" {
					finalCurrent = ids[2]
				}
				if scenario == "restore" {
					var backup bytes.Buffer
					require.NoError(t, s.ExportMetadata(t.Context(), &backup))
					s = newTestStore(t)
					require.NoError(t, s.ImportMetadata(t.Context(), &backup))
				}
			case "selected barrier":
				stale := preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids})
				barrier := decide("selected", ids[0])
				require.NotEmpty(t, barrier.ReceiptID)
				for i, snapshot := range barrier.After {
					require.Equal(t, barrier.Before[i].Values.Flag, snapshot.Values.Flag)
				}
				_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stale, Decision: "current", CurrentID: ids[1]})
				require.ErrorIs(t, err, ErrStaleRevision)
				var backup bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &backup))
				s = newTestStore(t)
				require.NoError(t, s.ImportMetadata(t.Context(), &backup))
				wantFirst = "pick"
			case "clear", "repick", "manual undo", "manual undo undo", "manual backup":
				if scenario == "repick" || scenario == "manual undo undo" || scenario == "manual backup" {
					wantFirst = "pick"
				}
				flags := []string{"pick"}
				if scenario == "clear" || scenario == "repick" {
					flags = []string{""}
					if scenario == "repick" {
						flags = append(flags, "pick")
					}
				}
				var receipt PhotoAuthoredReceipt
				for _, flag := range flags {
					var err error
					receipt, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[:1]}), Patch: PhotoAuthoredPatch{Flag: new(flag)}})
					require.NoError(t, err)
				}
				require.NotEmpty(t, receipt.ReceiptID)
				switch scenario {
				case "clear", "repick":
					last := decide("current", ids[1])
					if scenario == "clear" {
						wantFirst = ""
						last = decide("current", ids[2])
						checkFlags([]string{wantFirst, "reject", "pick"})
					} else {
						checkFlags([]string{"pick", "pick", "reject"})
					}
					_, err := s.UndoPhotoAuthored(t.Context(), last.ReceiptID)
					require.NoError(t, err)
					if scenario == "clear" {
						checkFlags([]string{wantFirst, "pick", "reject"})
					} else {
						checkFlags([]string{"pick", "reject", "reject"})
					}
				case "manual backup":
					decide("current", ids[1])
					checkFlags([]string{"pick", "pick", "reject"})
				case "manual undo", "manual undo undo":
					undo, err := s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
					require.NoError(t, err)
					if scenario == "manual undo undo" {
						_, err = s.UndoPhotoAuthored(t.Context(), undo.ReceiptID)
						require.NoError(t, err)
					} else {
						wantFirst = "reject"
					}
				}
				var backup bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &backup))
				s = newTestStore(t)
				require.NoError(t, s.ImportMetadata(t.Context(), &backup))
				finalCurrent = ids[2]
			}
			decide("current", finalCurrent)
			check := make([]string, len(ids))
			for i, id := range ids {
				switch id {
				case ids[0]:
					check[i] = wantFirst
				case finalCurrent:
					check[i] = "pick"
				default:
					check[i] = "reject"
				}
			}
			checkFlags(check)
			prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
			require.NoError(t, err)
			if scenario == "rating" {
				for _, file := range prepared.Targets {
					if file.AssetID == ids[0] {
						require.Equal(t, 4, file.Values.Rating)
					}
				}
			}
			require.NoError(t, validatePhotoMetadataState(t.Context(), s.db, currentStorageSchemaVersion))
		})
	}
	t.Run("receipt order across clock ticks", func(t *testing.T) {
		t.Parallel()
		for _, advance := range []bool{false, true} {
			for _, restore := range []bool{false, true} {
				t.Run(fmt.Sprintf("advance=%t/restore=%t", advance, restore), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						s := newTestStore(t)
						ids := burstPhotos(t, s, 3)
						decide := func(decision, id string) PhotoAuthoredReceipt {
							r, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids}), Decision: decision, CurrentID: id})
							require.NoError(t, err)
							return r
						}
						first := decide("current", ids[0])
						if advance {
							time.Sleep(time.Second)
						}
						second := decide("all", ids[0])
						// Give the earlier receipt the larger UUID, a valid outcome of random allocation.
						for i, receipt := range []PhotoAuthoredReceipt{first, second} {
							id := []string{"ffffffff-ffff-4fff-bfff-ffffffffffff", "00000000-0000-4000-8000-000000000001"}[i]
							_, err := s.db.ExecContext(t.Context(), `UPDATE photo_change_receipts SET receipt_id=?,after_json=replace(after_json,?,?) WHERE receipt_id=?`, id, receipt.ReceiptID, id, receipt.ReceiptID)
							require.NoError(t, err)
						}
						if restore {
							var backup bytes.Buffer
							require.NoError(t, s.ExportMetadata(t.Context(), &backup))
							s = newTestStore(t)
							require.NoError(t, s.ImportMetadata(t.Context(), &backup))
						}
						decide("current", ids[1])
						asset, err := s.PhotoAssetByID(t.Context(), ids[0])
						require.NoError(t, err)
						require.Equal(t, "pick", asset.Files[0].Flag)
					})
				})
			}
		}
	})
	t.Run("gap merge keeps a manual pick", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "synthetic neighboring bursts")
		require.NoError(t, err)
		var ids []string
		for i, second := range []int{0, 1, 4, 5} {
			ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%d.jpg", i), fmt.Sprintf("2024-06-10T12:00:0%dZ", second), document.SourceMetadataPrecisionSecond))
		}
		decide := func(members []string, gap int, current string) {
			_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: gap, AssetIDs: members, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: members}), Decision: "current", CurrentID: current})
			require.NoError(t, err)
		}
		decide(ids[:2], 2, ids[0])
		decide(ids[2:], 2, ids[2])
		_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[:1]}), Patch: PhotoAuthoredPatch{Flag: new("pick")}})
		require.NoError(t, err)
		_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{BurstGapSeconds: new(3)})
		require.NoError(t, err)
		require.Equal(t, [][]int{{0, 1, 2, 3}}, stackIndexes(t, s, ids))
		decide(ids, 3, ids[1])
		flags := make([]string, len(ids))
		for i, id := range ids {
			asset, err := s.PhotoAssetByID(t.Context(), id)
			require.NoError(t, err)
			flags[i] = asset.Files[0].Flag
		}
		require.Equal(t, []string{"pick", "pick", "reject", "reject"}, flags)
	})
}

func TestPhotoStackDecisionsAndUndo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		decision string
		seed     string
		want     []string
	}{
		{"current", "pick", []string{"pick", "pick", "reject"}},
		{"all", "reject", []string{"pick", "pick", "pick"}},
		{"selected", "pick", []string{"pick", "reject", "pick"}},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ids := burstPhotos(t, s, 3)
			_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[:1], Limit: MaxPhotoStackTargets}), Patch: PhotoAuthoredPatch{Flag: new(tc.seed)}})
			require.NoError(t, err)
			request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: tc.decision, CurrentID: ids[1], SelectedIDs: []string{ids[2]}}
			receipt, err := s.DecidePhotoStack(t.Context(), request)
			require.NoError(t, err)
			require.NotEmpty(t, receipt.ReceiptID)
			require.Empty(t, receipt.Representatives)
			var receipts int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts WHERE operation='authored'`).Scan(&receipts))
			require.Equal(t, 2, receipts)
			flags := func() []string {
				page, err := s.ListPhotoAssets(t.Context(), captureRequest(t, "asc"), nil)
				require.NoError(t, err)
				result := make([]string, len(page.Items))
				for i, row := range page.Items {
					result[i] = row.Flag
				}
				return result
			}
			require.Equal(t, tc.want, flags())
			request.Targets = preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
			noop, err := s.DecidePhotoStack(t.Context(), request)
			require.NoError(t, err)
			require.Empty(t, noop.ReceiptID)
			_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
			require.NoError(t, err)
			require.Equal(t, []string{tc.seed, "", ""}, flags())
			require.NoError(t, validatePhotoMetadataState(t.Context(), s.db, currentStorageSchemaVersion))
		})
	}
}

func TestPhotoStackRejectsStaleMembershipAndTargetsWithoutWrites(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	targets := preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids[:2], Targets: targets, Decision: "current", CurrentID: ids[0]}
	_, err := s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	request.AssetIDs = ids
	for _, tc := range []struct {
		name   string
		change func(*PhotoStackDecision)
		detail string
	}{
		{"empty selection", func(r *PhotoStackDecision) { r.SelectedIDs = nil }, "selected_ids must contain at least one ID"},
		{"foreign selection", func(r *PhotoStackDecision) { r.SelectedIDs = []string{"00000000-0000-4000-8000-000000000009"} }, "selected_ids must belong to asset_ids"},
		{"foreign current", func(r *PhotoStackDecision) { r.CurrentID = "00000000-0000-4000-8000-000000000009" }, "current_id must belong to asset_ids"},
		{"duplicate targets", func(r *PhotoStackDecision) { r.Targets[1] = r.Targets[0] }, "targets must contain unique file_id values"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: slices.Clone(targets), Decision: "selected", CurrentID: ids[0], SelectedIDs: ids[:1]}
			tc.change(&invalid)
			_, err := s.DecidePhotoStack(t.Context(), invalid)
			require.ErrorIs(t, err, ErrInvalidPhotoAsset)
			require.ErrorContains(t, err, tc.detail)
		})
	}
	request.Decision = "current"
	request.Targets = targets[:2]
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	request.Targets = targets
	request.Decision = "all"
	for _, stale := range []func(*PhotoAuthoredEditTarget){
		func(target *PhotoAuthoredEditTarget) { target.NodeRevision++ },
		func(target *PhotoAuthoredEditTarget) { target.AssetRevision++ },
		func(target *PhotoAuthoredEditTarget) {
			target.ContentVersionID = "00000000-0000-4000-8000-000000000001"
		},
	} {
		request.Targets = slices.Clone(targets)
		stale(&request.Targets[2])
		_, err = s.DecidePhotoStack(t.Context(), request)
		require.ErrorIs(t, err, ErrStaleRevision)
	}
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts`).Scan(&count))
	require.Zero(t, count)
	// A concurrent reject changes the revision the photographer saw, so Keep selected cannot overwrite it.
	_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: targets[1:2], Patch: PhotoAuthoredPatch{Flag: new("reject")}})
	require.NoError(t, err)
	staleTargets := preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	staleTargets[1].Revision = targets[1].Revision
	_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: staleTargets, Decision: "selected", CurrentID: ids[1], SelectedIDs: ids[1:2]})
	require.ErrorIs(t, err, ErrStaleRevision)
	kept, err := s.PhotoAssetByID(t.Context(), ids[1])
	require.NoError(t, err)
	require.Equal(t, "reject", kept.Files[0].Flag)
	// A wider gap leaves membership intact, so only the observed-gap precondition refuses it.
	request = PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: ids[0], Decision: "all"}
	_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{BurstGapSeconds: new(5)})
	require.NoError(t, err)
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	for i, id := range ids {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, []string{"", "reject", ""}[i], asset.Files[0].Flag)
	}
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts WHERE operation='authored'`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestPhotoStackAutomaticDecisionsPreserveCompanionFlags(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"current", "selected"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ids := burstPhotos(t, s, 2)
			first, err := s.PhotoAssetByID(t.Context(), ids[0])
			require.NoError(t, err)
			raw, err := s.CreateFile(t.Context(), s.RootID(), "frame-0000.cr2", fakeHash("d4"), 4, "application/octet-stream")
			require.NoError(t, err)
			pair, err := s.AttachPhotoFile(t.Context(), first.ID, first.Revision, raw.ID, PhotoRoleRAW, nil)
			require.NoError(t, err)
			rawFile := pair.Files[slices.IndexFunc(pair.Files, func(file PhotoFile) bool { return file.Role == PhotoRoleRAW })]
			_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image")})
			require.NoError(t, err)
			rawFlag := "pick"
			_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: rawFile.ID, Revision: rawFile.Revision, Patch: PhotoAuthoredPatch{Flag: new(rawFlag)}}})
			require.NoError(t, err)
			_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: decision, CurrentID: ids[1], SelectedIDs: ids[1:]})
			require.NoError(t, err)
			kept, err := photoFileByIDQuery(t.Context(), s.db, rawFile.ID)
			require.NoError(t, err)
			require.Equal(t, rawFlag, kept.Flag)
			require.Equal(t, int64(2), kept.Revision)
		})
	}
}

func TestPhotoStackRepresentativeFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		frames []photoStackFrame
		want   string
	}{
		{"multiple picks", []photoStackFrame{{AssetID: "a", Rejected: true}, {AssetID: "b", Picked: true}, {AssetID: "c", Picked: true}}, "b"},
		{"explicit choice rejected", []photoStackFrame{{AssetID: "a", Rank: 2, Rejected: true}, {AssetID: "b", Picked: true}}, "b"},
		{"earlier explicit choice", []photoStackFrame{{AssetID: "a", Rank: 1}, {AssetID: "b", Rank: 2, Rejected: true}, {AssetID: "c", Picked: true}}, "a"},
		{"first non-rejected", []photoStackFrame{{AssetID: "a", Rejected: true}, {AssetID: "b"}}, "b"},
		{"all rejected with choice", []photoStackFrame{{AssetID: "a", Rank: 1, Rejected: true}, {AssetID: "b", Rank: 2, Rejected: true}}, "b"},
		{"all rejected without choice", []photoStackFrame{{AssetID: "a", Rejected: true}, {AssetID: "b", Rejected: true}}, "a"},
		{"choice restored after un-reject", []photoStackFrame{{AssetID: "a", Rank: 2}, {AssetID: "b", Picked: true}}, "a"},
		{"picked companion keeps choice eligible", []photoStackFrame{{AssetID: "a", Rank: 2, Picked: true, Rejected: true}, {AssetID: "b", Picked: true}}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, newPhotoStack(tc.frames).RepresentativeID)
		})
	}
}

func TestPhotoStackRejectedRepresentativeEligibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	reject := func(ids []string) {
		_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Patch: PhotoAuthoredPatch{Flag: new("reject")}})
		require.NoError(t, err)
	}
	choose := func() (PhotoAuthoredReceipt, error) {
		return s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: "representative", CurrentID: ids[1]})
	}
	reject(ids[1:])
	var before, after bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &before))
	_, err := choose()
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.NoError(t, s.ExportMetadata(t.Context(), &after))
	require.Equal(t, before.String(), after.String(), "a refused choice must leave metadata unchanged")
	reject(ids[:1])
	receipt, err := choose()
	require.NoError(t, err)
	require.Len(t, receipt.Representatives, 1)
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, Query: captureRequest(t, "asc").Query}, nil)
	require.NoError(t, err)
	require.Equal(t, ids[1], page.Stacks[0].RepresentativeID)
	for _, row := range page.Items {
		require.Equal(t, "reject", row.Flag)
	}
}

func TestPhotoStackRepresentativePreservesFlagsAcrossUndoAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 4)
	// Two picks use the first picked frame until someone chooses.
	_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[1:3], Limit: MaxPhotoStackTargets}), Patch: PhotoAuthoredPatch{Flag: new("pick")}})
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids[3:], Limit: MaxPhotoStackTargets}), Patch: PhotoAuthoredPatch{Flag: new("reject")}})
	require.NoError(t, err)
	representative := func(s *Store) (string, []string) {
		page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, Query: captureRequest(t, "asc").Query}, nil)
		require.NoError(t, err)
		require.Len(t, page.Stacks, 1)
		flags := make([]string, len(page.Items))
		for i, row := range page.Items {
			flags[i] = row.Flag
		}
		return page.Stacks[0].RepresentativeID, flags
	}
	_, flags := representative(s)
	choose := func(s *Store, id string) PhotoAuthoredReceipt {
		receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: "representative", CurrentID: id})
		require.NoError(t, err)
		return receipt
	}
	receipt := choose(s, ids[0])
	require.Empty(t, receipt.After)
	require.Len(t, receipt.Representatives, 1)
	require.Equal(t, int64(1), *receipt.Representatives[0].After)
	require.Equal(t, int64(1), receipt.Representatives[0].BeforeRevision)
	chosen, after := representative(s)
	require.Equal(t, ids[0], chosen)
	require.Equal(t, flags, after)
	require.Equal(t, []string{"", "pick", "pick", "reject"}, after)
	rechosen := choose(s, ids[0])
	require.NotEmpty(t, rechosen.ReceiptID)
	require.Equal(t, int64(2), *rechosen.Representatives[0].After)
	receipt = rechosen
	later := choose(s, ids[1])
	require.Equal(t, int64(3), *later.Representatives[0].After)
	chosen, _ = representative(s)
	require.Equal(t, ids[1], chosen)
	// Undo restores the earlier choice only while the asset keeps the receipt's revision.
	undo, err := s.UndoPhotoAuthored(t.Context(), later.ReceiptID)
	require.NoError(t, err)
	require.Len(t, undo.Representatives, 1)
	chosen, _ = representative(s)
	require.Equal(t, ids[0], chosen)
	_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
	require.NoError(t, err)
	chosen, _ = representative(s)
	require.Equal(t, ids[0], chosen)
	latest := choose(s, ids[2])
	require.Equal(t, int64(4), *latest.Representatives[0].After)
	_, err = s.UndoPhotoAuthored(t.Context(), undo.ReceiptID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	require.Contains(t, exported.String(), `"representative_sequence":4`)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	chosen, after = representative(restored)
	require.Equal(t, ids[2], chosen)
	require.Equal(t, flags, after)
	// The greatest issued rank remains in receipts after its choice is undone.
	_, err = restored.UndoPhotoAuthored(t.Context(), latest.ReceiptID)
	require.NoError(t, err)
	exported.Reset()
	require.NoError(t, restored.ExportMetadata(t.Context(), &exported))
	restoredAgain := newTestStore(t)
	require.NoError(t, restoredAgain.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	next := choose(restoredAgain, ids[0])
	require.Equal(t, int64(5), *next.Representatives[0].After)
	stale := choose(restoredAgain, ids[2])
	_, err = restoredAgain.SetPhotoAssetExcluded(t.Context(), ids[2], stale.Representatives[0].AfterRevision, true)
	require.NoError(t, err)
	_, err = restoredAgain.UndoPhotoAuthored(t.Context(), stale.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestPhotoStackLatestChoiceWinsWhenGapMergesStacks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic bursts")
	require.NoError(t, err)
	var ids []string
	for i, offset := range []int{0, 1, 4, 5} {
		ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/A/frame-%d.jpg", i), fmt.Sprintf("2024-06-10T12:00:0%dZ", offset), document.SourceMetadataPrecisionSecond))
	}
	for _, choice := range []struct{ members []string }{{ids[:2]}, {ids[2:]}} {
		_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: choice.members, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: choice.members, Limit: MaxPhotoStackTargets}), Decision: "representative", CurrentID: choice.members[1]})
		require.NoError(t, err)
	}
	_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{BurstGapSeconds: new(3)})
	require.NoError(t, err)
	page, err := s.ListPhotoAssets(t.Context(), captureRequest(t, "asc"), nil)
	require.NoError(t, err)
	require.Equal(t, ids, page.Stacks[0].AssetIDs)
	require.Equal(t, ids[3], page.Stacks[0].RepresentativeID)
}

func TestPhotoSettingsUpdates(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	unchanged, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{BurstGapSeconds: new(2)})
	require.NoError(t, err)
	require.Equal(t, int64(1), unchanged.Revision)
	for _, invalid := range []PhotoSettingsUpdate{
		{Preference: new("image"), BurstGapSeconds: new(31)},
		{Preference: new("jpeg"), BurstGapSeconds: new(5)},
		{ResetPreference: true, Preference: new("image")},
		{BurstGapSeconds: new(0)},
		{BackupIntervalSeconds: new(int64(9223372037))},
	} {
		_, err = s.SetPhotoSettings(t.Context(), 1, invalid)
		require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	}
	both, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image"), BurstGapSeconds: new(30)})
	require.NoError(t, err)
	require.Equal(t, PhotoSettings{BurstGapSeconds: 30, Preference: new("image"), Revision: 2, UpdatedAt: both.UpdatedAt}, both)
	gap, err := s.SetPhotoSettings(t.Context(), 2, PhotoSettingsUpdate{BurstGapSeconds: new(4)})
	require.NoError(t, err)
	require.Equal(t, new("image"), gap.Preference)
	var receipts int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts WHERE operation='settings'`).Scan(&receipts))
	require.Equal(t, 2, receipts)
	reset, err := s.SetPhotoSettings(t.Context(), gap.Revision, PhotoSettingsUpdate{ResetPreference: true, BurstGapSeconds: new(5)})
	require.NoError(t, err)
	require.Nil(t, reset.Preference)
	require.Equal(t, 5, reset.BurstGapSeconds)
}

func TestPhotoSettingsBackupCompatibility(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	burstPhotos(t, s, 1)
	_, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image"), BurstGapSeconds: new(4), BackupIntervalSeconds: new(int64(3600)), BackupKeepLast: new(5), HiddenUnlockLifetimeSeconds: new(int64(900))})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	require.Contains(t, exported.String(), `"burst_gap_seconds":4`)
	require.Contains(t, exported.String(), `,"representative_sequence":null`)
	restoredOverrides := newTestStore(t)
	require.NoError(t, restoredOverrides.ImportMetadata(t.Context(), strings.NewReader(exported.String())))
	overrides, err := restoredOverrides.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, new(int64(3600)), overrides.BackupIntervalSeconds)
	require.Equal(t, new(5), overrides.BackupKeepLast)
	require.Equal(t, new(int64(900)), overrides.HiddenUnlockLifetimeSeconds)
	largest := newTestStore(t)
	require.NoError(t, largest.ImportMetadata(t.Context(), strings.NewReader(strings.ReplaceAll(exported.String(), `"backup_interval_seconds":3600`, `"backup_interval_seconds":9223372036`))))
	largestSettings, err := largest.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, new(int64(9223372036)), largestSettings.BackupIntervalSeconds)
	for _, bad := range []string{
		strings.ReplaceAll(exported.String(), `"backup_interval_seconds":3600`, `"backup_interval_seconds":-1`),
		strings.ReplaceAll(exported.String(), `"backup_keep_last":5`, `"backup_keep_last":0`),
		strings.ReplaceAll(exported.String(), `"hidden_unlock_lifetime_seconds":900`, `"hidden_unlock_lifetime_seconds":86401`),
	} {
		require.ErrorContains(t, newTestStore(t).ImportMetadata(t.Context(), strings.NewReader(bad)), "invalid photo settings metadata")
	}
	missingSequence := strings.ReplaceAll(exported.String(), `,"representative_sequence":null`, "")
	require.NotContains(t, missingSequence, `"representative_sequence"`)
	require.ErrorContains(t, newTestStore(t).ImportMetadata(t.Context(), strings.NewReader(missingSequence)), "representative_sequence")
	legacySettings := strings.ReplaceAll(exported.String(), `,"burst_gap_seconds":4`, "")
	v1 := strings.ReplaceAll(strings.ReplaceAll(legacySettings, `,"representative_sequence":null`, ""), `"version":2`, `"version":1`)
	v1 = strings.ReplaceAll(v1, `"flag_owner":"",`, "")
	v1 = strings.NewReplacer(`"backup_interval_seconds":3600,`, "", `"backup_keep_last":5,`, "", `"hidden_unlock_lifetime_seconds":900,`, "").Replace(v1)
	require.Contains(t, strings.SplitN(v1, "\n", 2)[0], `"version":1`)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), strings.NewReader(v1)))
	var sequence int64
	require.NoError(t, restored.db.QueryRow(`SELECT representative_sequence FROM photo_library_settings`).Scan(&sequence))
	require.Zero(t, sequence)
	settings, err := restored.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, new("image"), settings.Preference)
	require.Equal(t, 2, settings.BurstGapSeconds)
	require.Nil(t, settings.BackupIntervalSeconds)
	require.Nil(t, settings.BackupKeepLast)
	require.Nil(t, settings.HiddenUnlockLifetimeSeconds)
	for _, sequence := range []struct {
		name string
		wire string
		want *int64
	}{
		{name: "null", wire: "null"},
		{name: "issued", wire: "7", want: new(int64(7))},
	} {
		t.Run("v2/"+sequence.name, func(t *testing.T) {
			stream := strings.ReplaceAll(legacySettings, `"representative_sequence":null`, `"representative_sequence":`+sequence.wire)
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(t.Context(), strings.NewReader(stream)))
			var actual *int64
			require.NoError(t, restored.db.QueryRow(`SELECT representative_sequence FROM photo_library_settings`).Scan(&actual))
			require.Equal(t, sequence.want, actual)
			settings, err := restored.PhotoSettings(t.Context())
			require.NoError(t, err)
			require.Equal(t, new("image"), settings.Preference)
			require.Equal(t, 2, settings.BurstGapSeconds)
		})
	}
}

func TestPhotoStackHiddenScopeAndUndo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(ctx, id)
		require.NoError(t, err)
		_, err = s.SetPhotoAssetHidden(ctx, id, asset.Revision, true)
		require.NoError(t, err)
	}
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	prepared, err := s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: ids, Hidden: true})
	require.NoError(t, err)
	request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, CurrentID: ids[0], Decision: "representative", Hidden: true}
	for _, target := range prepared.Targets {
		request.Targets = append(request.Targets, PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision})
	}
	_, err = s.DecidePhotoStack(ctx, request)
	require.ErrorIs(t, err, ErrHiddenLocked)
	ctx = unlocked
	request.Hidden = false
	_, err = s.DecidePhotoStack(ctx, request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	request.Hidden = true
	receipt, err := s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Hidden: true, Stacks: true, Query: captureRequest(t, "asc").Query}, nil)
	require.NoError(t, err)
	require.Equal(t, ids, page.Stacks[0].AssetIDs)
}

func TestPhotoRepresentativeImportRejectsInconsistentBackup(t *testing.T) {
	t.Parallel()
	for _, receiptOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("receipt-only=%t", receiptOnly), func(t *testing.T) {
			s := newTestStore(t)
			ids := burstPhotos(t, s, 2)
			// A retained high-water mark can outlive all current choices.
			receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: ids[1], Decision: "representative"})
			require.NoError(t, err)
			require.Equal(t, int64(1), *receipt.Representatives[0].After)
			if receiptOnly {
				_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
				require.NoError(t, err)
				var choices int
				require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_assets WHERE representative_rank IS NOT NULL`).Scan(&choices))
				require.Zero(t, choices)
			}
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &backup))
			require.Contains(t, backup.String(), `"representative_sequence":1}`)
			for _, sequence := range []string{"0", "null"} {
				corrupt := bytes.ReplaceAll(backup.Bytes(), []byte(`"representative_sequence":1}`), []byte(`"representative_sequence":`+sequence+`}`))
				restored := newTestStore(t)
				require.ErrorContains(t, restored.ImportMetadata(t.Context(), bytes.NewReader(corrupt)), "photo representative sequence is below retained ranks")
			}
			// Receipts hold the asset ID escaped inside their JSON; asset rows hold it plain.
			corrupt := bytes.ReplaceAll(backup.Bytes(), []byte(`\"asset_id\":\"`+ids[1]+`\"`), []byte(`\"asset_id\":\"00000000-0000-4000-8000-000000000001\"`))
			require.NotEqual(t, backup.Bytes(), corrupt)
			require.ErrorContains(t, newTestStore(t).ImportMetadata(t.Context(), bytes.NewReader(corrupt)), "representative receipts reference missing photo assets")
		})
	}
}

func TestPhotoRepresentativeDecisionKeepsDefaultSettings(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_library_settings`).Scan(&count))
	require.Zero(t, count)
	_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: ids[0], Decision: "representative"})
	require.NoError(t, err)
	settings, err := s.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Empty(t, settings.UpdatedAt)
	require.Equal(t, int64(1), settings.Revision)
}

func TestPhotoStackIncompleteMetadataScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	fingerprint := browseHash("current-extractor")
	request := captureRequest(t, "asc")
	request.PageSize = 10
	request.SourceMetadataFingerprint = fingerprint
	publish := func(id string) {
		t.Helper()
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
		require.NoError(t, err)
		generation, _, err := s.ActiveSourceMetadata(t.Context(), node.BlobHash)
		require.NoError(t, err)
		_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, fingerprint, generation.CanonicalJSON)
		require.NoError(t, err)
	}
	publish(ids[0])
	publish(ids[1])
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.True(t, page.GroupingIncomplete)
	require.Len(t, page.Items, 3)
	require.Len(t, page.Stacks, 1)
	groups, _, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, fingerprint)
	require.NoError(t, err)
	require.True(t, incomplete)
	require.Equal(t, [][]string{ids}, groups)
	groups, _, _, incomplete, err = s.PhotoStackGroups(t.Context(), ids[:2], false, fingerprint)
	require.NoError(t, err)
	require.False(t, incomplete)
	require.Len(t, groups, 1)
	request.assetIDs = ids[:2]
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.False(t, page.GroupingIncomplete)
	require.Len(t, page.Stacks, 1)
	request.assetIDs = nil
	publish(ids[2])
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.False(t, page.GroupingIncomplete)
	require.Len(t, page.Stacks, 1)
}

// After an extractor upgrade every stored generation is stale. A burst from
// one body still stacks, and browsing, Cull grouping, and decisions agree.
func TestPhotoStackAfterExtractorUpgrade(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic upgraded library")
	require.NoError(t, err)
	var ids []string
	for i := range 4 {
		stamp := time.Date(2024, 6, 10, 12, 0, i, 0, time.UTC).Format(time.RFC3339)
		ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond,
			photoMetadataField("image.exif.body_serial_number", "image.exif", "BodySerialNumber", photoString("body-a"))))
	}
	fingerprint := browseHash("upgraded-extractor")
	groups, _, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, fingerprint)
	require.NoError(t, err)
	require.True(t, incomplete)
	require.Equal(t, [][]string{ids}, groups)
	for _, request := range []PhotoBrowseRequest{
		captureRequest(t, "asc"),
		{Stacks: true, PageSize: 10, Query: snapshotTestQuery(t, `{"text":"frame","sort":{"field":"relevance","direction":"asc"}}`)},
	} {
		request.SourceMetadataFingerprint = fingerprint
		var found [][]string
		var cursor *PhotoBrowsePosition
		for {
			page, err := s.ListPhotoAssets(t.Context(), request, cursor)
			require.NoError(t, err)
			for _, stack := range page.Stacks {
				found = append(found, stack.AssetIDs)
			}
			if cursor = page.Next; cursor == nil {
				break
			}
		}
		require.Equal(t, [][]string{ids}, found)
	}
	_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: ids[1], Decision: "representative", SourceMetadataFingerprint: fingerprint})
	require.NoError(t, err)
}

// Until backfill reads the serials, frames from two bodies of one model look
// like one stack, so a flag change would pick or reject the other body's frames.
func TestPhotoStackDecisionsRefuseStaleMetadata(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic mixed bodies")
	require.NoError(t, err)
	var ids []string
	for i, serial := range []string{"body-a", "body-b", "body-a", "body-b"} {
		stamp := time.Date(2024, 6, 10, 12, 0, i, 0, time.UTC).Format(time.RFC3339)
		ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond,
			photoMetadataField("image.exif.camera_make", "image.exif", "Make", photoString("Canon")),
			photoMetadataField("image.exif.camera_model", "image.exif", "Model", photoString("R5")),
			photoMetadataField("image.exif.body_serial_number", "image.exif", "BodySerialNumber", photoString(serial))))
	}
	fingerprint := browseHash("upgraded-extractor")
	targets := preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	for _, decision := range []string{"current", "all", "selected"} {
		_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: slices.Clone(targets), Decision: decision, CurrentID: ids[0], SelectedIDs: ids[:1], SourceMetadataFingerprint: fingerprint})
		require.ErrorIs(t, err, ErrStalePhotoStack, decision)
	}
	var receipts int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts`).Scan(&receipts))
	require.Zero(t, receipts)
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		require.Empty(t, asset.Files[0].Flag)
	}
}

func TestPhotoStackPartialMetadataCandidates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, staleStamp, serial string
		candidate                bool
	}{
		{"unknown serial bridge", "2024-06-10T12:00:02Z", "", true},
		{"stale serial becomes unknown", "2024-06-10T12:00:02Z", "old-body", true},
		{"known body stays separate", "2024-06-10T12:00:02Z", "known-body", false},
		{"missing EXIF", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic partial bursts")
			require.NoError(t, err)
			fingerprint := browseHash("current-extractor")
			var ids []string
			for i, stamp := range []string{"2024-06-10T12:00:00Z", "2024-06-10T12:00:04Z", "2024-06-10T12:00:20Z", "2024-06-10T12:00:21Z", test.staleStamp} {
				var fields []document.SourceMetadataFieldV1
				if i < 2 && test.serial == "known-body" || i == 4 && test.serial != "" {
					fields = append(fields, photoMetadataField("image.exif.body_serial_number", "image.exif", "BodySerialNumber", photoString(test.serial)))
				}
				if i == 1 && !test.candidate {
					stamp = "2024-06-10T12:00:01Z"
				}
				id := stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%d.jpg", i), stamp, document.SourceMetadataPrecisionSecond, fields...)
				ids = append(ids, id)
				if i < 4 {
					asset, err := s.PhotoAssetByID(t.Context(), id)
					require.NoError(t, err)
					node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
					require.NoError(t, err)
					generation, _, err := s.ActiveSourceMetadata(t.Context(), node.BlobHash)
					require.NoError(t, err)
					_, err = s.PublishSourceMetadata(t.Context(), node.BlobHash, fingerprint, generation.CanonicalJSON)
					require.NoError(t, err)
				}
			}
			// A frame read by an outdated extractor groups as one without a
			// serial, so it bridges only frames that have none.
			expected, byRequest := [][]string{ids[2:4]}, [][]string{ids[2:4]}
			if test.candidate {
				expected = append([][]string{{ids[0], ids[4], ids[1]}}, expected...)
				byRequest = append([][]string{{ids[0], ids[1], ids[4]}}, byRequest...)
			} else {
				expected = append([][]string{ids[:2]}, expected...)
				byRequest = append([][]string{ids[:2]}, byRequest...)
			}
			groups, unavailable, _, incomplete, err := s.PhotoStackGroups(t.Context(), ids, false, fingerprint)
			require.NoError(t, err)
			require.Empty(t, unavailable)
			require.True(t, incomplete)
			require.Equal(t, byRequest, groups)
			for _, sort := range []string{"capture_time", "relevance"} {
				request := PhotoBrowseRequest{Stacks: true, PageSize: 10, SourceMetadataFingerprint: fingerprint, Query: snapshotTestQuery(t, `{"text":"frame","sort":{"field":"`+sort+`","direction":"asc"}}`)}
				page, err := s.ListPhotoAssets(t.Context(), request, nil)
				require.NoError(t, err)
				require.True(t, page.GroupingIncomplete)
				var got [][]string
				for _, stack := range page.Stacks {
					got = append(got, stack.AssetIDs)
				}
				require.ElementsMatch(t, expected, got)
			}
			request := captureRequest(t, "asc")
			request.SourceMetadataFingerprint = fingerprint
			var cursor *PhotoBrowsePosition
			var found [][]string
			for {
				page, err := s.ListPhotoAssets(t.Context(), request, cursor)
				require.NoError(t, err)
				for _, stack := range page.Stacks {
					found = append(found, stack.AssetIDs)
				}
				if slices.Contains(rowIDs(page.Items), ids[2]) {
					require.False(t, page.GroupingIncomplete, "stale metadata outside this page leaves the healthy burst ready")
				}
				cursor = page.Next
				if cursor == nil {
					break
				}
			}
			require.ElementsMatch(t, expected, found)
		})
	}
}
