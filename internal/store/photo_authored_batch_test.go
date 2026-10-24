package store

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func TestPhotoPreparedSkipsTrashedMembersAndRefusesStaleSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		displayTrashed bool
		scope          string
		stale          bool
		targets        int
		assets         int
	}{
		{"member/asset", false, "asset", false, 1, 1},
		{"member/assets", false, "assets", false, 2, 0},
		{"member/query", false, "query", false, 2, 0},
		{"display/asset", true, "asset", true, 0, 0},
		{"display/assets", true, "assets", true, 1, 0},
		{"display/query", true, "query", false, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			member := fileByRole(asset.Files, PhotoRoleRAW)
			if (member.ID == *asset.DisplayFileID) != tc.displayTrashed {
				member = fileByRole(asset.Files, PhotoRoleImage)
			}
			_, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: member.ID, Revision: member.Revision, Patch: PhotoAuthoredPatch{Rating: new(5), Flag: new("pick")}}})
			require.NoError(t, err)
			node, err := s.NodeByID(ctx, member.NodeID)
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, node.ID, node.Revision)
			require.NoError(t, err)
			other := albumAsset(t, s, "other.jpg")
			q, err := query.Parse([]byte(`{}`))
			require.NoError(t, err)
			selection := map[string]PhotoSetSelection{"asset": {AssetIDs: []string{asset.ID}}, "assets": {AssetIDs: []string{asset.ID, other.ID}}, "query": {Query: &q}}[tc.scope]
			prepared, err := s.PreparePhotoAuthored(ctx, selection)
			if tc.stale {
				require.ErrorIs(t, err, ErrStaleRevision)
				require.ErrorContains(t, err, "refresh your selection")
				require.ErrorContains(t, err, asset.ID)
				selection.SkipUnavailable = true
				prepared, err = s.PreparePhotoAuthored(ctx, selection)
				require.NoError(t, err)
				require.Len(t, prepared.Targets, tc.targets)
				for _, target := range prepared.Targets {
					require.Equal(t, other.ID, target.AssetID)
				}
				return
			}
			require.NoError(t, err)
			require.Len(t, prepared.Assets, tc.assets)
			require.Len(t, prepared.Targets, tc.targets)
			for _, target := range prepared.Targets {
				require.NotEqual(t, member.ID, target.FileID)
				require.Equal(t, tc.assets == 1, target.XMP != nil, "only a single photo carries XMP status")
			}
			for _, preparedAsset := range prepared.Assets {
				require.Len(t, preparedAsset.Files, 1)
				require.True(t, preparedAsset.Agreement["rating"])
				require.True(t, preparedAsset.Agreement["flag"])
				require.Zero(t, preparedAsset.Files[0].Rating)
				require.Empty(t, preparedAsset.Files[0].Flag)
			}
		})
	}
}

func TestPhotoPreparedReadDoesNotTakeWriter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	tx, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	require.Len(t, prepared.Targets, 2)
}

func TestPhotoPreparedThousandTargetsAndScopeLimits(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "Portfolio")
	require.NoError(t, err)
	ids := make([]string, 0, 1000)
	tagged := map[string]bool{}
	for i := range 1000 {
		asset := albumAsset(t, s, fmt.Sprintf("prepared-%04d.jpg", i))
		ids = append(ids, asset.ID)
		if i%photoSelectionChunkSize == 0 || i == 999 {
			file := asset.Files[0]
			node, err := s.NodeByID(ctx, file.NodeID)
			require.NoError(t, err)
			_, err = s.AssignTag(ctx, tag.ID, file.NodeID, node.Revision)
			require.NoError(t, err)
			tagged[file.ID] = true
		}
	}
	q, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	for _, selection := range []PhotoSetSelection{{AssetIDs: ids}, {Query: &q}} {
		prepared, err := s.PreparePhotoAuthored(ctx, selection)
		require.NoError(t, err)
		require.Empty(t, prepared.Assets)
		require.Len(t, prepared.Targets, 1000)
		for i, target := range prepared.Targets {
			asset, err := s.PhotoAssetByID(ctx, target.AssetID)
			require.NoError(t, err)
			if selection.Query == nil {
				require.Equal(t, ids[i], asset.ID)
			}
			require.Equal(t, asset.ID, target.AssetID)
			require.Equal(t, asset.Revision, target.AssetRevision)
			require.Equal(t, asset.Files[0].ID, target.FileID)
			require.Equal(t, asset.Files[0].Revision, target.Revision)
			require.Equal(t, asset.Files[0].Authored(), target.Values)
			node, err := s.NodeByID(ctx, target.NodeID)
			require.NoError(t, err)
			require.Equal(t, node.Name, target.FileName)
			require.Equal(t, node.Revision, target.NodeRevision)
			if tagged[target.FileID] {
				require.Equal(t, []string{tag.ID}, target.Tags)
			} else {
				require.Empty(t, target.Tags)
			}
		}
	}
	_, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{Query: &q, SkipUnavailable: true})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.ErrorContains(t, err, "provide either asset_ids or query")
	_, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: append(ids, ids[0]), SkipUnavailable: true})
	require.ErrorIs(t, err, ErrPhotoEditFileLimit)
	albumAsset(t, s, "over-limit.jpg")
	_, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{Query: &q})
	require.ErrorIs(t, err, ErrPhotoEditFileLimit)
}

func TestPhotoAuthoredBatchKeywordsUndoAndRestore(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	tag, err := s.CreateTag(ctx, "Portfolio")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	request := PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Rating: new(4)}, Keywords: []PhotoKeywordEdit{{tag.ID, true}}}
	receipt, err := s.EditPhotoAuthoredBatch(ctx, request)
	require.NoError(t, err)
	require.Len(t, receipt.After, 2)
	require.Len(t, receipt.Keywords, 2)
	require.Len(t, receipt.KeywordNodes, 2)
	require.NoError(t, s.ValidateMetadata(ctx))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
	undo, err := restored.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.Len(t, undo.Keywords, 2)
	for _, before := range receipt.Before {
		file, err := photoFileByIDQuery(ctx, restored.db, before.FileID)
		require.NoError(t, err)
		require.Equal(t, before.Values, file.Authored())
	}
	for _, change := range receipt.Keywords {
		var count int
		require.NoError(t, restored.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_tags WHERE node_id=? AND tag_id=?`, change.NodeID, change.TagID).Scan(&count))
		require.Equal(t, change.Before, count == 1)
	}
	fence := receipt.KeywordNodes[0]
	_, err = s.UnassignTag(ctx, tag.ID, fence.NodeID, fence.AfterRevision)
	require.NoError(t, err)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	for _, file := range receipt.After {
		current, err := photoFileByIDQuery(ctx, s.db, file.FileID)
		require.NoError(t, err)
		require.Equal(t, file.Revision, current.Revision)
	}
}

func TestPhotoAuthoredBatchRotationDeltaPreservesMemberDifferencesAndUndo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	seedInitialAuditAuthority(t, s, s.RootID())
	raw, image := fileByRole(asset.Files, PhotoRoleRAW), fileByRole(asset.Files, PhotoRoleImage)
	setRotation := func(fileID string, rotation int) {
		targets := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
		targets = slices.DeleteFunc(targets, func(target PhotoAuthoredEditTarget) bool { return target.FileID != fileID })
		_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Rotation: &rotation}})
		require.NoError(t, err)
	}
	setRotation(raw.ID, 90)
	setRotation(image.ID, 180)

	rotationDelta := 90
	receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), RotationDelta: &rotationDelta})
	require.NoError(t, err)
	rotations := map[string]int{}
	for _, file := range receipt.After {
		rotations[file.FileID] = file.Values.Rotation
	}
	require.Equal(t, 180, rotations[raw.ID])
	require.Equal(t, 270, rotations[image.ID])

	undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	rotations = map[string]int{}
	for _, file := range undo.After {
		rotations[file.FileID] = file.Values.Rotation
	}
	require.Equal(t, 90, rotations[raw.ID])
	require.Equal(t, 180, rotations[image.ID])
}

func preparedPhotoTargets(ctx context.Context, t *testing.T, s *Store, selection PhotoSetSelection) []PhotoAuthoredEditTarget {
	t.Helper()
	prepared, err := s.PreparePhotoAuthored(ctx, selection)
	require.NoError(t, err)
	targets := make([]PhotoAuthoredEditTarget, len(prepared.Targets))
	for i, target := range prepared.Targets {
		targets[i] = PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision}
	}
	return targets
}

func TestPhotoAuthoredBatchConflictsAndAtomicFailure(t *testing.T) {
	t.Parallel()
	t.Run("member and atomic rollback", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		asset := authoredPair(t, s)
		file := fileByRole(asset.Files, PhotoRoleRAW)
		request := PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Caption: new("River")}}
		request.Targets = slices.DeleteFunc(request.Targets, func(target PhotoAuthoredEditTarget) bool { return target.FileID != file.ID })
		r, err := s.EditPhotoAuthoredBatch(ctx, request)
		require.NoError(t, err)
		require.Len(t, r.After, 1)
		_, err = s.EditPhotoAuthoredBatch(ctx, request)
		require.ErrorIs(t, err, ErrStaleRevision)
		request.Targets = preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
		request.Keywords = []PhotoKeywordEdit{{"40000000-0000-4000-8000-000000000099", false}}
		_, err = s.EditPhotoAuthoredBatch(ctx, request)
		require.ErrorIs(t, err, ErrNotFound)
		current, err := s.PhotoAssetByID(ctx, asset.ID)
		require.NoError(t, err)
		require.Empty(t, fileByRole(current.Files, PhotoRoleImage).Caption)
		require.False(t, current.Agreement["caption"])
	})
	t.Run("node and asset revision conflicts", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		asset := authoredPair(t, s)
		targets := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
		tag, err := s.CreateTag(ctx, "Portfolio")
		require.NoError(t, err)
		_, err = s.AssignTag(ctx, tag.ID, fileByRole(asset.Files, PhotoRoleRAW).NodeID, targets[0].NodeRevision)
		require.NoError(t, err)
		_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Rating: new(5)}, Keywords: []PhotoKeywordEdit{{tag.ID, false}}})
		require.ErrorIs(t, err, ErrStaleRevision)
		for _, target := range targets {
			current, err := photoFileByIDQuery(ctx, s.db, target.FileID)
			require.NoError(t, err)
			require.Zero(t, current.Rating)
		}
		targets = preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
		display := fileByRole(asset.Files, PhotoRoleImage).ID
		_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &display)
		require.NoError(t, err)
		_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Rating: new(5)}})
		require.ErrorIs(t, err, ErrStaleRevision)
	})
}

func TestPhotoAuthoredBatchViewedContent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		replace   bool
		swap      bool
		bind      bool
		bindOther bool
		stale     bool
	}{
		{name: "matching", bind: true},
		{name: "replaced", replace: true, bind: true, stale: true},
		{name: "non-display matching", bindOther: true},
		{name: "non-display replaced", replace: true, bindOther: true, stale: true},
		{name: "swapped display", swap: true, bind: true, stale: true},
		{name: "omitted after replacement", replace: true},
		{name: "omitted after display swap", swap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			var display, other PhotoFile
			for _, file := range asset.Files {
				if file.ID == *asset.DisplayFileID {
					display = file
				} else {
					other = file
				}
			}
			node, err := s.NodeByID(ctx, display.NodeID)
			require.NoError(t, err)
			page := browsePhotoPage(t, s, `{}`)
			require.Len(t, page.Items, 1)
			require.Equal(t, node.CurrentVersionID, page.Items[0].ContentVersionID)
			if tc.replace {
				_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, browseHash("viewed-replacement"), 20, "image/jpeg")
				require.NoError(t, err)
			}
			if tc.swap {
				_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &other.ID)
				require.NoError(t, err)
			}
			// Fresh preparation must still refuse a decision about the old display.
			targets := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
			for i := range targets {
				if (tc.bind && targets[i].FileID == display.ID) || (tc.bindOther && targets[i].FileID == other.ID) {
					targets[i].ContentVersionID = node.CurrentVersionID
				}
			}
			receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Flag: new("pick")}})
			if tc.stale {
				require.ErrorIs(t, err, ErrStaleRevision)
				require.Empty(t, receipt.ReceiptID)
			} else {
				require.NoError(t, err)
				require.Len(t, receipt.After, 2)
			}
			current, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			for _, file := range current.Files {
				if tc.stale {
					require.Empty(t, file.Flag)
				} else {
					require.Equal(t, "pick", file.Flag)
				}
			}
		})
	}
}

func TestPhotoAuthoredBatchRequestValidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	targets := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	for name, patch := range map[string]PhotoAuthoredPatch{
		"rating": {Rating: new(6)}, "flag": {Flag: new("invalid")}, "label": {Label: new("invalid")}, "rotation": {Rotation: new(45)},
	} {
		t.Run(name, func(t *testing.T) {
			before, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: patch})
			require.ErrorIs(t, err, ErrInvalidPhotoAsset)
			after, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			require.Equal(t, before.Files, after.Files)
		})
	}
	_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Patch: PhotoAuthoredPatch{Rating: new(4)}})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, RotationDelta: new(0)})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Rotation: new(90)}, RotationDelta: new(90)})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: append(slices.Clone(targets), targets[0]), Patch: PhotoAuthoredPatch{Rating: new(4)}})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	tag, err := s.CreateTag(ctx, "Keyword")
	require.NoError(t, err)
	_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Keywords: []PhotoKeywordEdit{{tag.ID, true}, {tag.ID, true}}})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	var keywords []PhotoKeywordEdit
	for i := range 100 {
		tag, err := s.CreateTag(ctx, fmt.Sprintf("Keyword %03d", i))
		require.NoError(t, err)
		keywords = append(keywords, PhotoKeywordEdit{tag.ID, true})
	}
	request := PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Keywords: keywords}
	_, err = s.EditPhotoAuthoredBatch(ctx, request)
	require.NoError(t, err)
	extra, err := s.CreateTag(ctx, "Keyword 100")
	require.NoError(t, err)
	request.Targets = make([]PhotoAuthoredEditTarget, maxBatchTagTargets+1)
	_, err = s.EditPhotoAuthoredBatch(ctx, request)
	require.ErrorIs(t, err, ErrPhotoEditFileLimit)
	request.Targets = preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	request.Keywords = append(request.Keywords, PhotoKeywordEdit{extra.ID, true})
	_, err = s.EditPhotoAuthoredBatch(ctx, request)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
}

func TestPhotoAuthoredBatchRevisionGroups(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	makeAsset := func(name string) PhotoAsset {
		image := albumAsset(t, s, name)
		_, err := s.DetachPhotoFile(ctx, image.ID, image.Revision, image.Files[0].ID, PhotoDetachOptions{})
		require.NoError(t, err)
		raw, err := s.CreateFile(ctx, s.RootID(), name+".cr2", fakeHash("a1"), 4, "application/octet-stream")
		require.NoError(t, err)
		asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
		require.NoError(t, err)
		asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, image.Files[0].NodeID, PhotoRoleImage, nil)
		require.NoError(t, err)
		return asset
	}
	changed, unchanged := makeAsset("changed.jpg"), makeAsset("unchanged.jpg")
	for _, asset := range []PhotoAsset{changed, unchanged} {
		targets := []PhotoAuthoredTarget{}
		for _, file := range asset.Files {
			targets = append(targets, PhotoAuthoredTarget{FileID: file.ID, Revision: file.Revision, Patch: PhotoAuthoredPatch{Rating: new(4)}})
		}
		_, err := editPhotoAuthored(ctx, s, targets)
		require.NoError(t, err)
	}
	current, err := photoFileByIDQuery(ctx, s.db, changed.Files[0].ID)
	require.NoError(t, err)
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: current.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Rating: new(2)}}})
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	targets := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{changed.ID, unchanged.ID}})
	histories := map[string]AuditEventPage{}
	before := map[string]Node{}
	originals := map[string]PhotoFile{}
	changedIDs := []string{}
	for _, target := range targets {
		file, err := photoFileByIDQuery(ctx, s.db, target.FileID)
		require.NoError(t, err)
		node, err := s.NodeByID(ctx, file.NodeID)
		require.NoError(t, err)
		before[target.FileID] = node
		originals[target.FileID] = file
		if target.AssetID == changed.ID {
			changedIDs = append(changedIDs, target.FileID)
		}
		histories[target.FileID], err = s.AuditHistory(ctx, node.ID, 100, "")
		require.NoError(t, err)
	}
	receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Rating: new(4)}})
	require.NoError(t, err)
	afterIDs := make([]string, len(receipt.After))
	for i, file := range receipt.After {
		afterIDs[i] = file.FileID
	}
	require.ElementsMatch(t, changedIDs, afterIDs)
	check := func() {
		for _, target := range targets {
			if target.AssetID != unchanged.ID {
				continue
			}
			file, err := photoFileByIDQuery(ctx, s.db, target.FileID)
			require.NoError(t, err)
			require.Equal(t, target.Revision, file.Revision)
			node, err := s.NodeByID(ctx, file.NodeID)
			require.NoError(t, err)
			require.Equal(t, before[target.FileID], node)
			history, err := s.AuditHistory(ctx, node.ID, 100, "")
			require.NoError(t, err)
			require.Equal(t, histories[target.FileID], history)
		}
	}
	check()
	for _, after := range receipt.After {
		file, err := photoFileByIDQuery(ctx, s.db, after.FileID)
		require.NoError(t, err)
		expected := originals[after.FileID].Authored()
		expected.Rating = 4
		require.Equal(t, expected, file.Authored())
		require.Equal(t, after.Revision, file.Revision)
		require.Equal(t, originals[after.FileID].Revision+1, file.Revision)
	}
	undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.Equal(t, receipt.ReceiptID, undo.UndoOf)
	afterIDs = make([]string, len(undo.After))
	for i, file := range undo.After {
		afterIDs[i] = file.FileID
	}
	require.ElementsMatch(t, changedIDs, afterIDs)
	for _, after := range undo.After {
		file, err := photoFileByIDQuery(ctx, s.db, after.FileID)
		require.NoError(t, err)
		require.Equal(t, originals[after.FileID].Authored(), file.Authored())
		require.Equal(t, after.Revision, file.Revision)
		require.Equal(t, originals[after.FileID].Revision+2, file.Revision)
	}
	check()
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
}

func TestPhotoKeywordEditNoop(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "keyword-noop.jpg")
	tag, err := s.CreateTag(ctx, "Portfolio")
	require.NoError(t, err)
	request := PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Keywords: []PhotoKeywordEdit{{tag.ID, true}}}
	_, err = s.EditPhotoAuthoredBatch(ctx, request)
	require.NoError(t, err)
	before := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	beforeTag, err := s.TagByID(ctx, tag.ID)
	require.NoError(t, err)
	request.Targets = before
	noOp, err := s.EditPhotoAuthoredBatch(ctx, request)
	require.NoError(t, err)
	require.Empty(t, noOp.ReceiptID)
	require.Empty(t, noOp.After)
	require.Equal(t, before, preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}))
	currentTag, err := s.TagByID(ctx, tag.ID)
	require.NoError(t, err)
	require.Equal(t, beforeTag.Revision, currentTag.Revision)
}

func TestPhotoKeywordUndo(t *testing.T) {
	t.Parallel()
	t.Run("deleted keyword", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		asset := albumAsset(t, s, "keyword.jpg")
		tag, err := s.CreateTag(ctx, "Portfolio")
		require.NoError(t, err)
		file := asset.Files[0]
		node, err := s.NodeByID(ctx, file.NodeID)
		require.NoError(t, err)
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Keywords: []PhotoKeywordEdit{{tag.ID, false}}})
		require.NoError(t, err)
		currentTag, err := s.TagByID(ctx, tag.ID)
		require.NoError(t, err)
		_, err = s.DeleteTag(ctx, tag.ID, currentTag.Revision)
		require.NoError(t, err)
		_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
		require.ErrorIs(t, err, ErrStaleRevision)
		require.ErrorContains(t, err, "keyword no longer exists")
	})
	t.Run("unchanged targets", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		changed, unchanged := albumAsset(t, s, "changed-keyword.jpg"), albumAsset(t, s, "existing-keyword.jpg")
		tag, err := s.CreateTag(ctx, "Portfolio")
		require.NoError(t, err)
		node, err := s.NodeByID(ctx, unchanged.Files[0].NodeID)
		require.NoError(t, err)
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{changed.ID, unchanged.ID}}), Keywords: []PhotoKeywordEdit{{tag.ID, true}}})
		require.NoError(t, err)
		_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: unchanged.Files[0].ID, Revision: unchanged.Files[0].Revision, Patch: PhotoAuthoredPatch{Caption: new("Later caption")}}})
		require.NoError(t, err)
		undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
		require.NoError(t, err)
		require.Len(t, undo.Keywords, 1)
		require.False(t, undo.Keywords[0].After)
		current, err := s.PhotoAssetByID(ctx, unchanged.ID)
		require.NoError(t, err)
		require.Equal(t, "Later caption", current.Files[0].Caption)
		_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
		require.ErrorIs(t, err, ErrStaleRevision)
		_, err = s.UndoPhotoAuthored(ctx, undo.ReceiptID)
		require.NoError(t, err)
	})
	t.Run("value-only edit after keyword changes", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		asset := authoredPair(t, s)
		tag, err := s.CreateTag(ctx, "Portfolio")
		require.NoError(t, err)
		_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Rating: new(4)}})
		require.NoError(t, err)
		node, err := s.NodeByID(ctx, fileByRole(asset.Files, PhotoRoleRAW).NodeID)
		require.NoError(t, err)
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Rating: new(5)}})
		require.NoError(t, err)
		require.Empty(t, receipt.KeywordNodes)
		for _, after := range receipt.After {
			require.Equal(t, 5, after.Values.Rating)
		}
		node, err = s.NodeByID(ctx, fileByRole(asset.Files, PhotoRoleRAW).NodeID)
		require.NoError(t, err)
		_, err = s.UnassignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
		undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
		require.NoError(t, err)
		for _, after := range undo.After {
			require.Equal(t, 4, after.Values.Rating)
		}
	})
}

func TestPhotoAuthoredInitialDecisionUndoRestoresConfirmation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "initial-decision.jpg")
	receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Rating: new(0)}})
	require.NoError(t, err)
	require.NotEmpty(t, receipt.ReceiptID)
	undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.NotEmpty(t, undo.ReceiptID)
	require.Len(t, undo.After, 1)
	require.Equal(t, PhotoConfirmedRating, undo.Before[0].Values.Confirmed)
	require.Equal(t, PhotoAuthored{}, undo.After[0].Values)
}

func TestPhotoAuthoredBatchTargetChecksFollowRequestOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "first.jpg")
	other := albumAsset(t, s, "second.jpg")
	valid := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}})[0]
	otherValid := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: []string{other.ID}})[0]
	const missing = "40000000-0000-4000-8000-000000000001"
	edit := func(change func(*PhotoAuthoredEditTarget)) PhotoAuthoredEditTarget {
		target := valid
		change(&target)
		return target
	}
	missingAsset := edit(func(target *PhotoAuthoredEditTarget) { target.AssetID = missing })
	missingFile := edit(func(target *PhotoAuthoredEditTarget) { target.FileID = missing })
	otherFile := edit(func(target *PhotoAuthoredEditTarget) { target.FileID = otherValid.FileID })
	staleAsset := edit(func(target *PhotoAuthoredEditTarget) { target.AssetRevision++ })
	invalidAsset := edit(func(target *PhotoAuthoredEditTarget) { target.AssetID = "not-a-uuid" })
	for _, tc := range []struct {
		name    string
		targets []PhotoAuthoredEditTarget
		want    error
	}{
		{"invalid asset", []PhotoAuthoredEditTarget{invalidAsset}, ErrInvalidPhotoAsset},
		{"missing asset", []PhotoAuthoredEditTarget{missingAsset}, ErrNotFound},
		{"missing file", []PhotoAuthoredEditTarget{missingFile}, ErrNotFound},
		{"file of another asset", []PhotoAuthoredEditTarget{otherFile}, ErrStaleRevision},
		{"stale asset", []PhotoAuthoredEditTarget{staleAsset}, ErrStaleRevision},
		{"stale before missing", []PhotoAuthoredEditTarget{otherValid, staleAsset, missingFile}, ErrStaleRevision},
		{"missing before stale", []PhotoAuthoredEditTarget{otherValid, missingFile, staleAsset}, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: tc.targets, Patch: PhotoAuthoredPatch{Rating: new(3)}})
			require.ErrorIs(t, err, tc.want)
			for _, id := range []string{valid.FileID, otherValid.FileID} {
				current, err := photoFileByIDQuery(ctx, s.db, id)
				require.NoError(t, err)
				require.Zero(t, current.Rating)
			}
		})
	}
}

func TestPhotoAuthoredBatchEditsTargetsAcrossLoadChunks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ids := make([]string, 0, photoSelectionChunkSize+1)
	for i := range photoSelectionChunkSize + 1 {
		ids = append(ids, albumAsset(t, s, fmt.Sprintf("chunk-%04d.jpg", i)).ID)
	}
	targets := preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: ids})
	require.Len(t, targets, photoSelectionChunkSize+1)
	receipt, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: targets, Patch: PhotoAuthoredPatch{Rating: new(2)}})
	require.NoError(t, err)
	require.Len(t, receipt.After, len(targets))
	for _, target := range targets {
		current, err := photoFileByIDQuery(ctx, s.db, target.FileID)
		require.NoError(t, err)
		require.Equal(t, 2, current.Rating)
		require.Equal(t, target.Revision+1, current.Revision)
	}
}
