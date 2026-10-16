package store

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func burstPhotos(t *testing.T, s *Store, count int) []PhotoBrowseRow {
	t.Helper()
	run, err := s.BeginIngest(t.Context(), "cli", "synthetic bursts")
	require.NoError(t, err)
	for i := range count {
		name := fmt.Sprintf("frame-%04d.jpg", i)
		node := ingestMtimePhoto(t, s, run, name, "")
		stamp := time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		browsePhotoMetadata(t, s, node, name, exifCapture(stamp))
	}
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`), PageSize: 1000}, nil)
	require.NoError(t, err)
	result := page.Items
	for page.Next != nil {
		page, err = s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`), PageSize: 1000}, page.Next)
		require.NoError(t, err)
		result = append(result, page.Items...)
	}
	return result
}

func TestPhotoBurstPagesAndGap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows := burstPhotos(t, s, 4)
	ids := []string{rows[0].AssetID, rows[1].AssetID, rows[2].AssetID, rows[3].AssetID}
	require.NotEmpty(t, rows[0].SourceFolderKey)
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`), Stacks: true, PageSize: 1, Facets: []string{"flag"}}
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 4)
	require.Nil(t, page.Next)
	require.Equal(t, int64(4), page.Total)
	request.Query.Sort.Direction = "desc"
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, ids[3], page.Items[0].AssetID)
	request.Query.Sort.Field = "name"
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Next)
	request.Query.Sort = query.Sort{Field: "capture_time", Direction: "asc"}
	request.Query.Filters.AssetIDs = []string{rows[0].AssetID, rows[2].AssetID}
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 2) // Missing middle frame still chains at the exact 2s gap.
	_, err = s.SetPhotoBurstGap(t.Context(), 1, 1)
	require.NoError(t, err)
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotNil(t, page.Next)
	_, err = s.SetPhotoSettings(t.Context(), 2, new("image"))
	require.NoError(t, err)
	_, err = s.ListPhotoAssets(t.Context(), request, page.Next)
	require.NoError(t, err)
	_, err = s.SetPhotoBurstGap(t.Context(), 3, 3)
	require.NoError(t, err)
	_, err = s.ListPhotoAssets(t.Context(), request, page.Next)
	require.ErrorIs(t, err, ErrInvalidPhotoCursor)
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
		{"representative", "pick", []string{"", "pick", ""}},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			rows := burstPhotos(t, s, 3)
			preparation, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: []string{rows[0].AssetID}})
			require.NoError(t, err)
			_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{preparation.Targets[0].PhotoAuthoredEditTarget}, Patch: PhotoAuthoredPatch{Flag: new(tc.seed)}})
			require.NoError(t, err)
			ids := rowIDs(rows)
			preparation, err = s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
			require.NoError(t, err)
			targets := make([]PhotoAuthoredEditTarget, len(preparation.Targets))
			for i, target := range preparation.Targets {
				targets[i] = target.PhotoAuthoredEditTarget
			}
			request := PhotoStackDecision{AssetIDs: ids, Targets: targets, Decision: tc.decision, CurrentID: ids[1], SelectedIDs: []string{ids[2]}}
			receipt, err := s.DecidePhotoStack(t.Context(), request)
			require.NoError(t, err)
			require.NotEmpty(t, receipt.ReceiptID)
			var receiptCount int
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts WHERE operation='authored'`).Scan(&receiptCount))
			require.Equal(t, 2, receiptCount)
			page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`)}, nil)
			require.NoError(t, err)
			for i, row := range page.Items {
				require.Equal(t, tc.want[i], row.Flag)
			}
			if tc.decision == "all" {
				fresh, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
				require.NoError(t, err)
				for i, target := range fresh.Targets {
					request.Targets[i] = target.PhotoAuthoredEditTarget
				}
				noop, err := s.DecidePhotoStack(t.Context(), request)
				require.NoError(t, err)
				require.Empty(t, noop.ReceiptID)
			}
			_, err = s.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
			require.NoError(t, err)
			page, err = s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`)}, nil)
			require.NoError(t, err)
			require.Equal(t, []string{tc.seed, "", ""}, []string{page.Items[0].Flag, page.Items[1].Flag, page.Items[2].Flag})
			require.NoError(t, validatePhotoMetadataState(t.Context(), s.db, currentStorageSchemaVersion))
		})
	}
}

func TestPhotoStackRejectsStaleMembershipAndTargetsWithoutWrites(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows := burstPhotos(t, s, 3)
	ids := rowIDs(rows)
	preparation, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	targets := make([]PhotoAuthoredEditTarget, len(preparation.Targets))
	for i, target := range preparation.Targets {
		targets[i] = target.PhotoAuthoredEditTarget
	}
	request := PhotoStackDecision{AssetIDs: ids[:2], Targets: targets, Decision: "current", CurrentID: ids[0]}
	_, err = s.DecidePhotoStack(t.Context(), request)
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
	request.Targets[2].Revision++
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStaleRevision)
	for _, id := range ids {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		require.Empty(t, asset.Files[0].Flag)
		require.Equal(t, int64(1), asset.Files[0].Revision)
	}
	request.Targets[2].Revision--
	request.Targets[2].NodeRevision++
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStaleRevision)
	request.Targets[2].NodeRevision--
	request.Targets[2].AssetRevision++
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStaleRevision)
	request.Targets[2].AssetRevision--
	request.Targets[2].ContentVersionID = "00000000-0000-4000-8000-000000000001"
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStaleRevision)
	request.Targets[2].ContentVersionID = ""
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM photo_change_receipts`).Scan(&count))
	require.Zero(t, count)
	_, err = s.SetPhotoBurstGap(t.Context(), 1, 1)
	require.NoError(t, err)
	request.AssetIDs = []string{ids[0], ids[2]}
	request.Targets = []PhotoAuthoredEditTarget{targets[0], targets[2]}
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	_, err = s.SetPhotoBurstGap(t.Context(), 2, 2)
	require.NoError(t, err)
	request.Decision = "current"
	request.AssetIDs = ids[:2]
	request.Targets = targets[:2]
	_, err = s.DecidePhotoStack(t.Context(), request)
	require.NoError(t, err)
	third, err := s.PhotoAssetByID(t.Context(), ids[2])
	require.NoError(t, err)
	require.Empty(t, third.Files[0].Flag)
}

func TestPhotoBurstSettingsRestoreAndValidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	unchanged, err := s.SetPhotoBurstGap(t.Context(), 1, 2)
	require.NoError(t, err)
	require.Equal(t, int64(1), unchanged.Revision)
	_, err = s.SetPhotoBurstGap(t.Context(), 1, 30)
	require.NoError(t, err)
	_, err = s.SetPhotoBurstGap(t.Context(), 2, 31)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	settings, err := restored.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 30, settings.BurstGapSeconds)
	old := strings.ReplaceAll(exported.String(), `"burst_gap_seconds":30,`, "")
	restoredOld := newTestStore(t)
	require.NoError(t, restoredOld.ImportMetadata(t.Context(), strings.NewReader(old)))
	settings, err = restoredOld.PhotoSettings(t.Context())
	require.NoError(t, err)
	require.Equal(t, 2, settings.BurstGapSeconds)
}

func TestPhotoStackAuditsOneDecisionAndRollsBackTogether(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows := burstPhotos(t, s, 3)
	ids := rowIDs(rows)
	seedInitialAuditAuthority(t, s, s.RootID())
	preparation, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	targets := make([]PhotoAuthoredEditTarget, len(preparation.Targets))
	for i, target := range preparation.Targets {
		targets[i] = target.PhotoAuthoredEditTarget
	}
	request := PhotoStackDecision{AssetIDs: ids, Targets: targets, Decision: "current", CurrentID: ids[1]}
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
	preparation, err = s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	for i, target := range preparation.Targets {
		request.Targets[i] = target.PhotoAuthoredEditTarget
	}
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
	for _, decision := range []string{"current", "selected", "representative", "mixed"} {
		t.Run(decision, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			rows := burstPhotos(t, s, 2)
			raw, err := s.CreateFile(t.Context(), s.RootID(), "frame-0000.cr2", fakeHash("d4"), 4, "application/octet-stream")
			require.NoError(t, err)
			pair, err := s.AttachPhotoFile(t.Context(), rows[0].AssetID, rows[0].Revision, raw.ID, PhotoRoleRAW, nil)
			require.NoError(t, err)
			var rawFile PhotoFile
			for _, file := range pair.Files {
				if file.Role == PhotoRoleRAW {
					rawFile = file
				}
			}
			rawFlag, jpegFlag := "pick", "reject"
			if decision == "mixed" {
				rawFlag, jpegFlag = "reject", "pick"
			}
			_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: rawFile.ID, Revision: rawFile.Revision, Patch: PhotoAuthoredPatch{Flag: new(rawFlag)}}})
			require.NoError(t, err)
			if decision == "representative" || decision == "mixed" {
				_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: rows[0].DisplayFileID, Revision: 1, Patch: PhotoAuthoredPatch{Flag: new(jpegFlag)}}})
				require.NoError(t, err)
			}
			_, err = s.SetPhotoSettings(t.Context(), 1, new("image"))
			require.NoError(t, err)
			ids := rowIDs(rows)
			prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
			require.NoError(t, err)
			targets := make([]PhotoAuthoredEditTarget, len(prepared.Targets))
			for i, target := range prepared.Targets {
				targets[i] = target.PhotoAuthoredEditTarget
			}
			operation := decision
			if decision == "mixed" {
				operation = "current"
			}
			_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{AssetIDs: ids, Targets: targets, Decision: operation, CurrentID: ids[1], SelectedIDs: ids[1:]})
			require.NoError(t, err)
			kept, err := photoFileByIDQuery(t.Context(), s.db, rawFile.ID)
			require.NoError(t, err)
			wantRaw, wantRawRevision := "pick", int64(2)
			wantJPEG, wantJPEGRevision := "", int64(1)
			if decision == "mixed" {
				wantRaw, wantJPEG, wantJPEGRevision = "reject", "pick", 2
			}
			if decision == "representative" {
				wantRaw, wantRawRevision, wantJPEG, wantJPEGRevision = "", 3, "reject", 2
			}
			require.Equal(t, wantRaw, kept.Flag)
			require.Equal(t, wantRawRevision, kept.Revision)
			jpeg, err := photoFileByIDQuery(t.Context(), s.db, rows[0].DisplayFileID)
			require.NoError(t, err)
			require.Equal(t, wantJPEG, jpeg.Flag)
			require.Equal(t, wantJPEGRevision, jpeg.Revision)
		})
	}
}

func TestPhotoBurstPortableSourceFolders(t *testing.T) {
	t.Parallel()
	for name, sources := range map[string][]string{
		"drive": {`D:\A\one.jpg`, `D:/A/two.jpg`, `D:\B\three.jpg`},
		"UNC":   {`\\server\share\A\one.jpg`, `\\server\share\A\two.jpg`, `\\server\share\B\three.jpg`},
		"POSIX": {`/A\B/one.jpg`, `/A\B/two.jpg`, `/A/B/three.jpg`},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "POSIX" && runtime.GOOS == "windows" {
				t.Skip("backslashes are native separators on Windows")
			}
			s := newTestStore(t)
			run, err := s.BeginIngest(t.Context(), "cli", "synthetic portable import")
			require.NoError(t, err)
			for i, source := range sources {
				name := fmt.Sprintf("portable-%d.jpg", i)
				node, err := s.IngestFileExact(t.Context(), run, s.RootID(), name, browseHash(name), 20, "image/jpeg", source, "")
				require.NoError(t, err)
				browsePhotoMetadata(t, s, node, name, exifCapture(fmt.Sprintf("2024-06-10T12:00:0%dZ", i)))
			}
			page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`)}, nil)
			require.NoError(t, err)
			require.Equal(t, page.Items[0].SourceFolderKey, page.Items[1].SourceFolderKey)
			require.NotEqual(t, page.Items[0].SourceFolderKey, page.Items[2].SourceFolderKey)
		})
	}
}

func TestPhotoBurstRepresentativeUsesCompanionPick(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows := burstPhotos(t, s, 2)
	raw, err := s.CreateFile(t.Context(), s.RootID(), "companion.cr2", fakeHash("c4"), 4, "application/octet-stream")
	require.NoError(t, err)
	pair, err := s.AttachPhotoFile(t.Context(), rows[1].AssetID, rows[1].Revision, raw.ID, PhotoRoleRAW, nil)
	require.NoError(t, err)
	for _, file := range pair.Files {
		if file.Role == PhotoRoleRAW {
			_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: file.Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
			require.NoError(t, err)
		}
	}
	_, err = s.SetPhotoSettings(t.Context(), 1, new("image"))
	require.NoError(t, err)
	page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`)}, nil)
	require.NoError(t, err)
	require.Empty(t, page.Items[1].Flag)
	require.True(t, page.Items[1].Picked)
	require.False(t, page.Items[0].Picked)
}

func TestPhotoBurstPushSourceNamespace(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sources := []string{"camera-a", "camera-b"}
	ids := make([][]string, 2)
	for sourceIndex, sourceName := range sources {
		for frame := range 2 {
			name := fmt.Sprintf("frame-%d.jpg", frame)
			source := PushSource{Name: sourceName, Ref: "DCIM/" + name}
			content := PushContent{RunID: testRunID(t), ParentPath: "/" + sourceName, Name: name, Hash: browseHash(sourceName + name), Size: 20, MIMEType: "image/jpeg"}
			node, outcome, err := s.AcceptPush(t.Context(), source, content)
			require.NoError(t, err)
			require.Equal(t, "added", outcome)
			browsePhotoMetadata(t, s, node, sourceName+name, exifCapture(fmt.Sprintf("2024-06-10T12:00:0%dZ", sourceIndex*2+frame)))
			asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
			require.NoError(t, err)
			ids[sourceIndex] = append(ids[sourceIndex], asset.ID)
			content.RunID = testRunID(t)
			repeated, outcome, err := s.AcceptPush(t.Context(), source, content)
			require.NoError(t, err)
			require.Equal(t, "skipped", outcome)
			require.Equal(t, node.ID, repeated.ID)
		}
	}
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`)}
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.Equal(t, page.Items[0].SourceFolderKey, page.Items[1].SourceFolderKey)
	require.NotEqual(t, page.Items[0].SourceFolderKey, page.Items[2].SourceFolderKey)
	require.Equal(t, page.Items[2].SourceFolderKey, page.Items[3].SourceFolderKey)
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids[0]})
	require.NoError(t, err)
	targets := make([]PhotoAuthoredEditTarget, len(prepared.Targets))
	for i, target := range prepared.Targets {
		targets[i] = target.PhotoAuthoredEditTarget
	}
	_, err = s.DecidePhotoStack(t.Context(), PhotoStackDecision{AssetIDs: ids[0], Targets: targets, CurrentID: ids[0][0], Decision: "current"})
	require.NoError(t, err)
	for _, id := range ids[1] {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		require.Empty(t, asset.Files[0].Flag)
		require.Equal(t, int64(1), asset.Files[0].Revision)
	}
}

func rowIDs(rows []PhotoBrowseRow) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.AssetID
	}
	return ids
}

func TestPhotoBurstPageCap(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	burstPhotos(t, s, 501)
	for _, direction := range []string{"asc", "desc"} {
		request := PhotoBrowseRequest{Stacks: true, Query: snapshotTestQuery(t, `{"sort":{"field":"capture_time","direction":"asc"}}`), PageSize: 1}
		request.Query.Sort.Direction = direction
		first, err := s.ListPhotoAssets(t.Context(), request, nil)
		require.NoError(t, err)
		require.Len(t, first.Items, 500)
		next, err := s.ListPhotoAssets(t.Context(), request, first.Next)
		require.NoError(t, err)
		require.Len(t, next.Items, 1)
		require.Nil(t, next.Next)
		require.NotEqual(t, first.Items[499].AssetID, next.Items[0].AssetID)
	}
}

func TestPhotoBurstSharedTimeRules(t *testing.T) {
	t.Parallel()
	var fixtures []struct {
		Name string
		Gap  int
		Rows []struct {
			Time, Precision, Folder string
			Repeat                  int
		}
		Groups []int
	}
	data, err := os.ReadFile("testdata/photo-bursts.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &fixtures))
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			var rows []PhotoBrowseRow
			for _, row := range fixture.Rows {
				for range max(1, row.Repeat) {
					rows = append(rows, PhotoBrowseRow{Kind: "photo", SourceFolderKey: row.Folder, Fields: PhotoTechnicalFields{CaptureTime: new(row.Time), CaptureTimePrecision: new(row.Precision)}})
				}
			}
			var lengths []int
			start := 0
			for i := 1; i <= len(rows); i++ {
				if i == len(rows) || i-start == 500 || !photoStackNeighbors(rows[i-1], rows[i], fixture.Gap) {
					lengths = append(lengths, i-start)
					start = i
				}
			}
			require.Equal(t, fixture.Groups, lengths)
		})
	}
}

func TestPhotoStackHiddenScopeAndUndo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows := burstPhotos(t, s, 2)
	ids := rowIDs(rows)
	ctx := t.Context()
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	for _, row := range rows {
		_, err := s.SetPhotoAssetHidden(ctx, row.AssetID, row.Revision, true)
		require.NoError(t, err)
	}
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	ctx = WithPhotoHiddenToken(ctx, token)
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: ids, Hidden: true})
	require.NoError(t, err)
	request := PhotoStackDecision{AssetIDs: ids, CurrentID: ids[0], Decision: "current"}
	for _, target := range prepared.Targets {
		request.Targets = append(request.Targets, target.PhotoAuthoredEditTarget)
	}
	_, err = s.DecidePhotoStack(ctx, request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	request.Hidden = true
	receipt, err := s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	require.Len(t, receipt.After, 2)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	asset, err := s.PhotoAssetByID(ctx, ids[0])
	require.NoError(t, err)
	require.NotNil(t, asset.HiddenAt)
	require.Empty(t, asset.Files[0].Flag)
}

func TestPhotoBurstWatchNamespacesAndAmbiguousProvenance(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	var first Node
	for i, source := range []string{"camera-a", "camera-a", "camera-b"} {
		run, err := s.BeginIngest(ctx, "watch", source)
		require.NoError(t, err)
		name := fmt.Sprintf("watch-%d.jpg", i)
		node, err := s.IngestFileExact(ctx, run, s.RootID(), name, browseHash(name), 20, "image/jpeg", "DCIM/"+name, "")
		require.NoError(t, err)
		browsePhotoMetadata(t, s, node, name, exifCapture("2024-01-01T12:00:00Z"))
		if i == 0 {
			first = node
		}
	}
	request := PhotoBrowseRequest{Query: snapshotTestQuery(t, `{"sort":{"field":"name","direction":"asc"}}`)}
	page, err := s.ListPhotoAssets(ctx, request, nil)
	require.NoError(t, err)
	require.NotEmpty(t, page.Items[0].SourceFolderKey)
	require.Equal(t, page.Items[0].SourceFolderKey, page.Items[1].SourceFolderKey)
	require.NotEqual(t, page.Items[0].SourceFolderKey, page.Items[2].SourceFolderKey)
	// Two active origins for one display file cannot establish one source folder.
	current, err := s.NodeByID(ctx, first.ID)
	require.NoError(t, err)
	_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{NodeID: first.ID, IfRevision: current.Revision, SourceKind: "agent", SourceDescription: "second origin", OriginalPath: "other/" + first.Name})
	require.NoError(t, err)
	page, err = s.ListPhotoAssets(ctx, request, nil)
	require.NoError(t, err)
	require.Empty(t, page.Items[0].SourceFolderKey)
	ids := rowIDs(page.Items[:2])
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	targets := make([]PhotoAuthoredEditTarget, len(prepared.Targets))
	for i, target := range prepared.Targets {
		targets[i] = target.PhotoAuthoredEditTarget
	}
	_, err = s.DecidePhotoStack(ctx, PhotoStackDecision{AssetIDs: ids, Targets: targets, CurrentID: ids[0], Decision: "current"})
	require.ErrorIs(t, err, ErrStalePhotoStack)
	prepared, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: ids[:1]})
	require.NoError(t, err)
	receipt, err := s.DecidePhotoStack(ctx, PhotoStackDecision{AssetIDs: ids[:1], Targets: []PhotoAuthoredEditTarget{prepared.Targets[0].PhotoAuthoredEditTarget}, CurrentID: ids[0], Decision: "current"})
	require.NoError(t, err)
	asset, err := s.PhotoAssetByID(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, "pick", asset.Files[0].Flag)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	asset, err = s.PhotoAssetByID(ctx, ids[0])
	require.NoError(t, err)
	require.Empty(t, asset.Files[0].Flag)
}
