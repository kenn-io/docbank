package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// stackPhoto imports one synthetic frame from source. An empty stamp leaves
// only the file's modified time.
func stackPhoto(t *testing.T, s *Store, run IngestRun, source, stamp string, precision document.SourceMetadataTimestampPrecision) string {
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
		browsePhotoMetadata(t, s, node, source, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, precision, zone, offset)))
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

func stackTargets(t *testing.T, s *Store, ids []string) []PhotoAuthoredEditTarget {
	t.Helper()
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	require.NoError(t, err)
	targets := make([]PhotoAuthoredEditTarget, len(prepared.Targets))
	for i, target := range prepared.Targets {
		targets[i] = PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision}
	}
	return targets
}

func stackIndexes(t *testing.T, s *Store, ids []string) [][]int {
	t.Helper()
	groups, unavailable, _, err := s.PhotoStackGroups(t.Context(), ids, false)
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

func TestPhotoStackTimeRules(t *testing.T) {
	t.Parallel()
	var fixtures []struct {
		Name string `json:"name"`
		Gap  int    `json:"gap"`
		Rows []struct {
			Time      string `json:"time"`
			Precision string `json:"precision"`
			Folder    string `json:"folder"`
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
				ids[i] = stackPhoto(t, s, run, fmt.Sprintf("/%s/frame-%d.jpg", row.Folder, i), row.Time, document.SourceMetadataTimestampPrecision(row.Precision))
			}
			_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(fixture.Gap)})
			require.NoError(t, err)
			require.Equal(t, fixture.Stacks, stackIndexes(t, s, ids))
			// Library pages probe neighbors in SQL; they must find the same stacks.
			for _, direction := range []string{"asc", "desc"} {
				_, stacks := stackPages(t, s, captureRequest(t, direction))
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
				require.Equal(t, fixture.Stacks, paged, direction)
			}
		})
	}
}

func TestPhotoStackFoldersFollowSourceProvenance(t *testing.T) {
	t.Parallel()
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
	t.Run("claims for the displayed bytes", func(t *testing.T) {
		s := newTestStore(t)
		run, err := s.BeginIngest(t.Context(), "cli", "synthetic bursts")
		require.NoError(t, err)
		ids := []string{
			stackPhoto(t, s, run, "/A/one.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond),
			stackPhoto(t, s, run, "/A/two.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond),
		}
		asset, err := s.PhotoAssetByID(t.Context(), ids[1])
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
		require.NoError(t, err)
		// An older version imported elsewhere does not move the displayed bytes.
		original := node.CurrentVersionID
		other, err := s.BeginIngest(t.Context(), "cli", "older import")
		require.NoError(t, err)
		_, _, err = s.ReplaceContentForIngest(t.Context(), other, node.ID, node.Revision, browseHash("older bytes"), 20, "image/jpeg", "/B/two.jpg", "")
		require.NoError(t, err)
		node = revertTo(t, s, node.ID, original)
		require.Equal(t, browseHash("/A/two.jpg"), node.BlobHash)
		require.Equal(t, [][]int{{0, 1}}, stackIndexes(t, s, ids))
		_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: ids[0], Decision: "current"})
		require.NoError(t, err)
		// A second current claim from another folder makes the frame's folder ambiguous.
		node, err = s.NodeByID(t.Context(), node.ID)
		require.NoError(t, err)
		_, _, err = s.ReplaceContentForIngest(t.Context(), other, node.ID, node.Revision, node.BlobHash, 20, "image/jpeg", "/C/two.jpg", "")
		require.NoError(t, err)
		require.Equal(t, [][]int{}, stackIndexes(t, s, ids))
		_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: ids[0], Decision: "all"})
		require.ErrorIs(t, err, ErrStalePhotoStack)
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
	// Changing the gap invalidates cursors bound to the old grouping.
	_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(1)})
	require.NoError(t, err)
	_, err = s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, PageSize: 3, Query: asc.Query}, first.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
	items, _ = stackPages(t, s, asc)
	require.Equal(t, [][]string{{undated}, {a1}, {b1}, {a2}, {b2, b3}, {x}}, items)
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
	groups, _, _, err := s.PhotoStackGroups(t.Context(), ids, false)
	require.NoError(t, err)
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
	receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids[:500], Targets: stackTargets(t, s, ids[:500]), CurrentID: ids[0], Decision: "all"})
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
	// All 1,002 file revisions travel in one stack decision and receipt.
	indexes := make([]int, 334)
	for i := range indexes {
		indexes[i] = i
	}
	require.Equal(t, [][]int{indexes}, stackIndexes(t, s, ids))
	_, err = s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
	require.ErrorIs(t, err, ErrPhotoEditFileLimit)
	targets := stackTargets(t, s, ids)
	require.Len(t, targets, 1002)
	receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: targets, CurrentID: ids[0], Decision: "all"})
	require.NoError(t, err)
	require.Len(t, receipt.After, 1002)
	_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
	require.NoError(t, err)
	// Add files through the same import API to reach the stack's exact bound.
	for i := 1002; i <= 2000; i++ {
		asset, err := s.PhotoAssetByID(t.Context(), ids[(i-1002)%len(ids)])
		require.NoError(t, err)
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("extra-%d.cr2", i), browseHash(fmt.Sprintf("extra-%d", i)), 4, "application/octet-stream")
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, node.ID, PhotoRoleRAW, nil)
		require.NoError(t, err)
		if i == 1999 {
			for chunk := range slices.Chunk(stackTargets(t, s, ids), 1000) {
				edits := make([]PhotoAuthoredTarget, len(chunk))
				for j, target := range chunk {
					edits[j] = PhotoAuthoredTarget{FileID: target.FileID, Revision: target.Revision, Patch: PhotoAuthoredPatch{Creator: new(strings.Repeat("c", 100)), Copyright: new(strings.Repeat("r", 100))}}
				}
				_, err = editPhotoAuthored(t.Context(), s, edits)
				require.NoError(t, err)
			}
			targets = stackTargets(t, s, ids)
			require.Len(t, targets, 2000)
			receipt, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: targets, CurrentID: ids[0], Decision: "all"})
			require.NoError(t, err)
			require.Len(t, receipt.After, 2000)
			_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
			require.NoError(t, err)
		}
	}
	groups, _, _, err := s.PhotoStackGroups(t.Context(), ids, false)
	require.NoError(t, err)
	require.Empty(t, groups)
	_, err = s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	require.ErrorIs(t, err, ErrPhotoStackFileLimit)
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
			_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: stackTargets(t, s, ids[:1]), Patch: PhotoAuthoredPatch{Flag: new(tc.seed)}})
			require.NoError(t, err)
			request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), Decision: tc.decision, CurrentID: ids[1], SelectedIDs: []string{ids[2]}}
			receipt, err := s.DecidePhotoStack(t.Context(), request)
			require.NoError(t, err)
			require.NotEmpty(t, receipt.ReceiptID)
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
			request.Targets = stackTargets(t, s, ids)
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
	targets := stackTargets(t, s, ids)
	request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids[:2], Targets: targets, Decision: "current", CurrentID: ids[0]}
	_, err := s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	request.AssetIDs = ids
	request.Decision = "selected"
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	request.Decision = "current"
	request.Targets = targets[:2]
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	request.Targets = targets
	request.Decision = "all"
	for _, stale := range []func(*PhotoAuthoredEditTarget){
		func(target *PhotoAuthoredEditTarget) { target.Revision++ },
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
	_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: targets, Decision: "selected", CurrentID: ids[1], SelectedIDs: ids[1:2]})
	require.ErrorIs(t, err, ErrStaleRevision)
	kept, err := s.PhotoAssetByID(t.Context(), ids[1])
	require.NoError(t, err)
	require.Equal(t, "reject", kept.Files[0].Flag)
	// A wider gap leaves membership intact, so only the observed-gap precondition refuses it.
	request = PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: ids[0], Decision: "all"}
	_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(5)})
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

func TestPhotoStackAuditsOneDecisionAndRollsBackTogether(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	seedInitialAuditAuthority(t, s, s.RootID())
	request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), Decision: "current", CurrentID: ids[1]}
	receipt, err := s.DecidePhotoStack(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, receipt.After, 3)
	for _, snapshot := range receipt.After {
		history, err := s.AuditHistory(t.Context(), snapshot.NodeID, 10, "")
		require.NoError(t, err)
		require.NotNil(t, history.Items[0].Attachment.After.Photo)
	}
	_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
	require.NoError(t, err)
	request.Targets = stackTargets(t, s, ids)
	_, err = s.db.Exec(`CREATE TRIGGER reject_stack_audit BEFORE UPDATE ON audit_scopes BEGIN SELECT RAISE(ABORT,'forced stack audit failure'); END`)
	require.NoError(t, err)
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorContains(t, err, "forced stack audit failure")
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		require.Empty(t, asset.Files[0].Flag)
	}
}

func TestPhotoStackAutomaticDecisionsPreserveCompanionFlags(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{"current", "selected", "mixed"} {
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
			if decision == "mixed" {
				rawFlag = "reject"
				_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: *first.DisplayFileID, Revision: 1, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
				require.NoError(t, err)
			}
			_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: rawFile.ID, Revision: rawFile.Revision, Patch: PhotoAuthoredPatch{Flag: new(rawFlag)}}})
			require.NoError(t, err)
			operation := decision
			if decision == "mixed" {
				operation = "current"
			}
			_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), Decision: operation, CurrentID: ids[1], SelectedIDs: ids[1:]})
			require.NoError(t, err)
			kept, err := photoFileByIDQuery(t.Context(), s.db, rawFile.ID)
			require.NoError(t, err)
			require.Equal(t, rawFlag, kept.Flag)
			require.Equal(t, int64(2), kept.Revision)
		})
	}
}

func TestPhotoStackRepresentativePreservesFlagsAndUndoes(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 4)
	// Two picks leave the first frame as the representative until someone chooses.
	_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: stackTargets(t, s, ids[1:3]), Patch: PhotoAuthoredPatch{Flag: new("pick")}})
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: stackTargets(t, s, ids[3:]), Patch: PhotoAuthoredPatch{Flag: new("reject")}})
	require.NoError(t, err)
	representative := func() (string, []string) {
		page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Stacks: true, Query: captureRequest(t, "asc").Query}, nil)
		require.NoError(t, err)
		require.Len(t, page.Stacks, 1)
		flags := make([]string, len(page.Items))
		for i, row := range page.Items {
			flags[i] = row.Flag
		}
		return page.Stacks[0].RepresentativeID, flags
	}
	chosen, flags := representative()
	require.Equal(t, ids[0], chosen)
	choose := func(id string) PhotoAuthoredReceipt {
		receipt, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), Decision: "representative", CurrentID: id})
		require.NoError(t, err)
		return receipt
	}
	receipt := choose(ids[3])
	require.Empty(t, receipt.After)
	require.Len(t, receipt.Representatives, 1)
	require.Equal(t, int64(1), receipt.Representatives[0].BeforeRevision)
	chosen, after := representative()
	require.Equal(t, ids[3], chosen)
	require.Equal(t, flags, after)
	require.Equal(t, []string{"", "pick", "pick", "reject"}, after)
	rechosen := choose(ids[3])
	require.NotEmpty(t, rechosen.ReceiptID)
	require.Greater(t, *rechosen.Representatives[0].After, *receipt.Representatives[0].After)
	receipt = rechosen
	later := choose(ids[1])
	chosen, _ = representative()
	require.Equal(t, ids[1], chosen)
	// Undo restores the earlier choice only while the asset keeps the receipt's revision.
	undo, err := s.UndoPhotoAuthored(t.Context(), later.ReceiptID)
	require.NoError(t, err)
	require.Len(t, undo.Representatives, 1)
	chosen, _ = representative()
	require.Equal(t, ids[3], chosen)
	_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
	require.NoError(t, err)
	chosen, _ = representative()
	require.Equal(t, ids[3], chosen)
	stale := choose(ids[2])
	_, err = s.SetPhotoAssetExcluded(t.Context(), ids[2], stale.Representatives[0].AfterRevision, true)
	require.NoError(t, err)
	_, err = s.UndoPhotoAuthored(t.Context(), stale.ReceiptID)
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
	for _, choice := range []struct{ members []string }{{ids[2:]}, {ids[:2]}} {
		_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: choice.members, Targets: stackTargets(t, s, choice.members), Decision: "representative", CurrentID: choice.members[1]})
		require.NoError(t, err)
	}
	_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(3)})
	require.NoError(t, err)
	page, err := s.ListPhotoAssets(t.Context(), captureRequest(t, "asc"), nil)
	require.NoError(t, err)
	require.Equal(t, ids, page.Stacks[0].AssetIDs)
	require.Equal(t, ids[1], page.Stacks[0].RepresentativeID)
}

func TestPhotoSettingsUpdateValidatesBeforeOneRevision(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	unchanged, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(2)})
	require.NoError(t, err)
	require.Equal(t, int64(1), unchanged.Revision)
	for _, invalid := range []PhotoSettingsUpdate{
		{Preference: new("image"), BurstGapSeconds: new(31)},
		{Preference: new("jpeg"), BurstGapSeconds: new(5)},
		{KeepPreference: true, Preference: new("image")},
		{KeepPreference: true, BurstGapSeconds: new(0)},
	} {
		_, err = s.SetPhotoSettings(t.Context(), 1, invalid)
		require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	}
	both, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image"), BurstGapSeconds: new(30)})
	require.NoError(t, err)
	require.Equal(t, PhotoSettings{BurstGapSeconds: 30, Preference: new("image"), Revision: 2, UpdatedAt: both.UpdatedAt}, both)
	gap, err := s.SetPhotoSettings(t.Context(), 2, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(4)})
	require.NoError(t, err)
	require.Equal(t, new("image"), gap.Preference)
	var receipts int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts WHERE operation='settings'`).Scan(&receipts))
	require.Equal(t, 2, receipts)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	require.Contains(t, exported.String(), `"burst_gap_seconds":4`)
	old := newTestStore(t)
	require.NoError(t, old.ImportMetadata(t.Context(), strings.NewReader(strings.ReplaceAll(exported.String(), `,"burst_gap_seconds":4`, ""))))
	settings, err := old.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, settings.BurstGapSeconds)
}

func TestPhotoStackGroupsExplicitIDsInRequestOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic interleaved imports")
	require.NoError(t, err)
	a1 := stackPhoto(t, s, run, "/A/one.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond)
	b1 := stackPhoto(t, s, run, "/B/one.jpg", "2024-06-10T12:00:01Z", document.SourceMetadataPrecisionSecond)
	a2 := stackPhoto(t, s, run, "/A/two.jpg", "2024-06-10T12:00:02Z", document.SourceMetadataPrecisionSecond)
	b2 := stackPhoto(t, s, run, "/B/two.jpg", "2024-06-10T12:00:03Z", document.SourceMetadataPrecisionSecond)
	outside := stackPhoto(t, s, run, "/A/three.jpg", "2024-06-10T12:00:04Z", document.SourceMetadataPrecisionSecond)
	missing := "00000000-0000-4000-8000-000000000009"
	groups, unavailable, gap, err := s.PhotoStackGroups(t.Context(), []string{b2, a2, missing, b1, a1}, false)
	require.NoError(t, err)
	require.Equal(t, [][]string{{b2, b1}, {a2, a1}}, groups)
	require.Equal(t, []string{missing}, unavailable)
	require.Equal(t, 2, gap)
	require.NotContains(t, slices.Concat(groups...), outside)
	// A full run of 125,000 pairs retains request order.
	frames, position := make([]photoStackFrame, MaxPhotoStackGroupIDs), map[string]int{}
	for i := range frames {
		frames[i] = photoStackFrame{AssetID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), Folder: fmt.Sprintf("folder-%d", i/2), SortKey: "2024-06-10T12:00:00.000000000"}
		position[frames[i].AssetID] = i
	}
	pairs := photoStackRequestGroups(frames, 2, position)
	require.Len(t, pairs, MaxPhotoStackGroupIDs/2)
	require.Equal(t, []string{frames[2].AssetID, frames[3].AssetID}, pairs[1])
	_, _, gap, err = s.PhotoStackGroups(t.Context(), []string{a1, a1}, false)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.Zero(t, gap)
	tooMany := make([]string, MaxPhotoStackGroupIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	_, _, gap, err = s.PhotoStackGroups(t.Context(), tooMany, false)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.Zero(t, gap)
	_, _, gap, err = s.PhotoStackGroups(t.Context(), []string{a1}, true)
	require.ErrorIs(t, err, ErrHiddenLocked)
	require.Zero(t, gap)
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
	request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, CurrentID: ids[0], Decision: "representative", Hidden: true, Targets: []PhotoAuthoredEditTarget{{}}}
	_, err := s.DecidePhotoStack(ctx, request)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	ctx = WithPhotoHiddenToken(ctx, token)
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: ids, Hidden: true})
	require.NoError(t, err)
	request.Targets = nil
	for _, target := range prepared.Targets {
		request.Targets = append(request.Targets, PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision})
	}
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

func TestPhotoStackRepresentativeRefusesAuditedChoiceAndUndo(t *testing.T) {
	t.Parallel()
	for _, undo := range []bool{false, true} {
		t.Run(fmt.Sprintf("undo=%t", undo), func(t *testing.T) {
			s := newTestStore(t)
			ids := burstPhotos(t, s, 2)
			request := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: ids[1], Decision: "representative"}
			var receipt PhotoAuthoredReceipt
			var err error
			if undo {
				receipt, err = s.DecidePhotoStack(t.Context(), request)
				require.NoError(t, err)
			}
			seedInitialAuditAuthority(t, s, s.RootID())
			var beforeRank, afterRank sql.NullInt64
			var beforeRevision, afterRevision int64
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT representative_rank, revision FROM photo_assets WHERE asset_id=?`, ids[1]).Scan(&beforeRank, &beforeRevision))
			if undo {
				_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
			} else {
				_, err = s.DecidePhotoStack(t.Context(), request)
			}
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT representative_rank, revision FROM photo_assets WHERE asset_id=?`, ids[1]).Scan(&afterRank, &afterRevision))
			require.Equal(t, beforeRank, afterRank)
			require.Equal(t, beforeRevision, afterRevision)
		})
	}
}

func TestPhotoRepresentativeSequenceSurvivesUndoAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	choose := func(s *Store, id string) PhotoAuthoredReceipt {
		r, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: id, Decision: "representative"})
		require.NoError(t, err)
		return r
	}
	a, b := choose(s, ids[0]), choose(s, ids[1])
	require.Equal(t, int64(1), *a.Representatives[0].After)
	require.Equal(t, int64(2), *b.Representatives[0].After)
	undo, err := s.UndoPhotoAuthored(t.Context(), b.ReceiptID)
	require.NoError(t, err)
	c := choose(s, ids[2])
	require.Equal(t, int64(3), *c.Representatives[0].After)
	_, err = s.UndoPhotoAuthored(t.Context(), undo.ReceiptID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	page, err := restored.ListPhotoAssets(t.Context(), captureRequest(t, "asc"), nil)
	require.NoError(t, err)
	require.Equal(t, ids[2], page.Stacks[0].RepresentativeID)
	// Hide the greatest issued rank in receipts before another portable restore.
	_, err = restored.UndoPhotoAuthored(t.Context(), c.ReceiptID)
	require.NoError(t, err)
	exported.Reset()
	require.NoError(t, restored.ExportMetadata(t.Context(), &exported))
	restoredAgain := newTestStore(t)
	require.NoError(t, restoredAgain.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	later := choose(restoredAgain, ids[0])
	require.Equal(t, int64(4), *later.Representatives[0].After)
}

func TestPhotoRepresentativeReceiptRequiresAsset(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	r, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: ids[0], Decision: "representative"})
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE photo_change_receipts SET after_json=replace(after_json,?,?) WHERE receipt_id=?`, ids[0], "00000000-0000-4000-8000-000000000001", r.ReceiptID)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.ErrorContains(t, s.ExportMetadata(t.Context(), &exported), "representative receipts reference missing photo assets")
}

func TestPhotoRepresentativeKeepsDefaultSettingsTimestamp(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_library_settings`).Scan(&count))
	require.Zero(t, count)
	_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: stackTargets(t, s, ids), CurrentID: ids[0], Decision: "representative"})
	require.NoError(t, err)
	settings, err := s.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Empty(t, settings.UpdatedAt)
	require.Equal(t, int64(1), settings.Revision)
}
