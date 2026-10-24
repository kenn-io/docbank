package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func rejectOriginals(t *testing.T, s *Store, asset PhotoAsset) {
	t.Helper()
	var targets []PhotoAuthoredTarget
	for _, file := range asset.Files {
		if file.Role != PhotoRoleSidecar {
			targets = append(targets, rejectTarget(file))
		}
	}
	_, err := editPhotoAuthored(t.Context(), s, targets)
	require.NoError(t, err)
}

func rejectSettledOriginals(t *testing.T, s *Store, asset PhotoAsset) PhotoAsset {
	t.Helper()
	rejectOriginals(t, s, asset)
	return syncRejectedAsset(t, s, asset.ID)
}

// rejectMoveTarget captures original revisions independently of sidecar writes.
func rejectMoveTarget(t *testing.T, s *Store, assetID string) PhotoRejectTarget {
	t.Helper()
	asset, err := photoAssetByIDQuery(t.Context(), s.db, assetID)
	require.NoError(t, err)
	target := PhotoRejectTarget{AssetID: asset.ID, Revision: asset.Revision}
	for _, file := range asset.Files {
		target.MemberRevision += photoRejectRevision(file.Role, requireNodeByID(t, s, file.NodeID).Revision)
	}
	return target
}

// syncRejectedXMP settles an original as the sync worker does, with a sidecar that agrees with its reject.
func syncRejectedXMP(t *testing.T, s *Store, fileID string) {
	t.Helper()
	f, err := photoFileByIDQuery(t.Context(), s.db, fileID)
	require.NoError(t, err)
	publishXMPOrientation(t, s, f.NodeID, fakeHash("ee"))
	work, run, err := s.ClassifyPhotoXMP(t.Context(), fileID)
	require.NoError(t, err)
	if run {
		publishRejectedXMP(t, s, work)
	}
}

func publishRejectedXMP(t *testing.T, s *Store, work PhotoXMPWork) {
	t.Helper()
	plan, _ := PlanPhotoXMP(work, work.Steps(), "")
	if work.Sidecar != nil {
		packet := PhotoXMPPacket{PhotoAuthored: work.Original.Authored(), Present: work.Original.Confirmed | PhotoXMPKeywordsBit, Keywords: work.Keywords}
		packet.Confirmed = 0
		packet.FlagOwner = ""
		if work.History != nil && work.History.WrittenVersion == work.Sidecar.Node.CurrentVersionID {
			packet = work.History.Settled.Packet
			if packet.LegacyReject {
				packet.Rating = 0
				packet.Ignored |= PhotoConfirmedRating
			}
		}
		plan, _ = planXMP(work, packet, "")
	} else {
		plan.Packet = PhotoXMPPacket{Flag: "reject", Present: PhotoConfirmedRating | PhotoConfirmedFlag, Ignored: PhotoConfirmedRating, LegacyReject: true, Keywords: []string{}}
		plan.Write = true
	}
	_, err := s.PublishPhotoXMP(t.Context(), plan, browseHash(work.Original.ID), 8, BlobPhysical{Encoding: "raw", StoredBytes: 8, Created: true})
	require.NoError(t, err)
}

func syncRejectedAsset(t *testing.T, s *Store, assetID string) PhotoAsset {
	t.Helper()
	// The second pass settles tokens changed by another original's first attachment.
	for range 2 {
		asset, err := s.PhotoAssetByID(t.Context(), assetID)
		require.NoError(t, err)
		for _, file := range asset.Files {
			if file.Role != PhotoRoleSidecar {
				syncRejectedXMP(t, s, file.ID)
			}
		}
	}
	asset, err := s.PhotoAssetByID(t.Context(), assetID)
	require.NoError(t, err)
	return asset
}

func rejectTarget(file PhotoFile) PhotoAuthoredTarget {
	return PhotoAuthoredTarget{
		FileID: file.ID, Revision: file.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")},
	}
}

func rejectsRequest() PhotoRejectsRequest {
	return PhotoRejectsRequest{Query: query.Query{
		V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"},
	}}
}

func TestPhotoRejectsCoverage(t *testing.T) {
	t.Parallel()
	s, _, _, profile := collectionCoverageFixture(t, 0)
	asset, _ := photoTrashFixture(t, s)
	rejectSettledOriginals(t, s, asset)
	var display PhotoFile
	for _, file := range asset.Files {
		if file.ID == *asset.DisplayFileID {
			display = file
		}
	}
	node, err := s.NodeByID(t.Context(), display.NodeID)
	require.NoError(t, err)
	collectionCoveragePublish(t, s, node, profile, "complete")
	uncoveredNode, err := s.CreateFile(t.Context(), s.RootID(), "uncovered.jpg", fakeHash("a3"), 1, "image/jpeg")
	require.NoError(t, err)
	uncovered, err := s.PhotoAssetForNode(t.Context(), uncoveredNode.ID)
	require.NoError(t, err)
	rejectSettledOriginals(t, s, uncovered)
	request := rejectsRequest()
	request.Query.Text = "text_coverage:complete"
	request.Coverage = CoverageSelection{
		Configuration: "configured", ProfileFingerprint: profile.Fingerprint,
	}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, preview.Targets, 1)
	assert.Equal(t, asset.ID, preview.Targets[0].AssetID)
}

func TestPhotoRejectsPreflightDoesNotBlockOnOpenWriter(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, _ := photoTrashFixture(t, s)
	rejectSettledOriginals(t, s, asset)
	tx, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	preview, err := s.PreflightPhotoRejects(ctx, rejectsRequest())
	require.NoError(t, err)
	assert.Len(t, preview.Targets, 1)
}

func TestPhotoRejectsMoveGuards(t *testing.T) {
	t.Parallel()
	changes := []string{"flag reread", "revision", "hidden state", "duplicate target", "no live files", "hidden locked at preflight and at move"}
	for _, change := range changes {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			asset, _ := photoTrashFixture(t, s)
			asset = rejectSettledOriginals(t, s, asset)
			ctx := t.Context()
			request := rejectsRequest()
			if change == "hidden locked at preflight and at move" {
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
				request.Hidden = true
				_, err = s.PreflightPhotoRejects(ctx, request)
				require.ErrorIs(t, err, ErrHiddenLocked)
				token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
				require.NoError(t, err)
				ctx = WithPhotoHiddenToken(ctx, token)
			}
			preview, err := s.PreflightPhotoRejects(ctx, request)
			require.NoError(t, err)
			require.Len(t, preview.Targets, 1)
			original := fileByRole(asset.Files, PhotoRoleImage)
			wantErr := ErrStaleRevision
			switch change {
			case "duplicate target":
				preview.Targets = append(preview.Targets, preview.Targets[0])
				wantErr = ErrInvalidPhotoAsset
			case "hidden locked at preflight and at move":
				err = s.LockPhotoHidden(ctx)
				wantErr = ErrHiddenLocked
			case "no live files":
				trashed, trashErr := s.TrashPhotoAsset(ctx, asset.ID, preview.Targets[0].Revision)
				require.NoError(t, trashErr)
				var memberRevision int64
				for _, file := range trashed.Files {
					current, err := s.NodeByID(ctx, file.NodeID)
					require.NoError(t, err)
					if file.Role != PhotoRoleSidecar {
						memberRevision += current.Revision
					}
				}
				preview.Targets = []PhotoRejectTarget{{AssetID: trashed.ID, Revision: trashed.Revision, MemberRevision: memberRevision}}
			case "flag reread":
				_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: original.ID, Revision: original.Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
			case "hidden state":
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			case "revision":
				_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
			}
			require.NoError(t, err)
			roots, err := s.TrashedRoots(ctx)
			require.NoError(t, err)
			_, err = s.MovePhotoRejects(ctx, request.Hidden, preview.Targets)
			require.ErrorIs(t, err, wantErr)
			current, err := s.TrashedRoots(ctx)
			require.NoError(t, err)
			assert.Equal(t, roots, current)
		})
	}
}

// An unread outside sidecar stales the preview until the worker settles it.
func TestPhotoRejectsMoveWaitsForSidecarSync(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset, nodes := photoTrashFixture(t, s)
	sidecar := fileByRole(asset.Files, PhotoRoleSidecar)
	asset, err := s.DetachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoDetachOptions{})
	require.NoError(t, err)
	rejectOriginals(t, s, asset)
	preview, err := s.PreflightPhotoRejects(ctx, rejectsRequest())
	require.NoError(t, err)
	preview.Targets = []PhotoRejectTarget{rejectMoveTarget(t, s, asset.ID)}
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, nodes[2].ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	node, err := s.NodeByID(ctx, nodes[2].ID)
	require.NoError(t, err)
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("bc"), 8, node.MimeType)
	require.NoError(t, err)
	_, err = s.MovePhotoRejects(ctx, false, preview.Targets)
	require.ErrorIs(t, err, ErrStaleRevision)
	syncRejectedAsset(t, s, asset.ID)
	moved, err := s.MovePhotoRejects(ctx, false, preview.Targets)
	require.NoError(t, err)
	require.Equal(t, []string{asset.ID}, moved.Moved)
}

func TestPhotoRejectsAuditRefusal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node, err := s.CreateFile(
		t.Context(), s.RootID(), "live-reject.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	live, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	rejectSettledOriginals(t, s, live)
	dir, err := s.Mkdir(t.Context(), s.RootID(), "audited")
	require.NoError(t, err)
	for i := range 2 {
		node, err := s.CreateFile(t.Context(), dir.ID, fmt.Sprintf("supported-%d.jpg", i), fakeHash("b2"), 1, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		rejectSettledOriginals(t, s, asset)
	}
	settleRejectsAuditFixture(t, s, dir.ID)
	preview, err := s.PreflightPhotoRejects(t.Context(), rejectsRequest())
	require.NoError(t, err)
	require.Len(t, preview.Targets, 3)
	var supported []PhotoRejectTarget
	var blocked PhotoRejectTarget
	for _, target := range preview.Targets {
		syncRejectedAsset(t, s, target.AssetID)
		if target.AssetID == live.ID {
			blocked = target
		} else {
			supported = append(supported, target)
		}
	}
	require.Len(t, supported, 2)
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	var sequence, after int64
	require.NoError(t, s.db.QueryRow(`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&sequence))
	_, err = s.MovePhotoRejects(t.Context(), false, append(supported, blocked))
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	require.NoError(t, s.db.QueryRow(`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	assert.Equal(t, sequence, after)
	current, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Equal(t, roots, current)
}

func settleRejectsAuditFixture(t *testing.T, s *Store, dirID int64) {
	t.Helper()
	seedInitialAuditAuthority(t, s, dirID)
	var ids []string
	require.NoError(t, queryRows(t.Context(), s.db, `SELECT asset_id FROM photo_assets`, nil, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	}))
	for _, id := range ids {
		syncRejectedAsset(t, s, id)
	}
}

func trimRejectsBoundary(t *testing.T, s *Store) string {
	t.Helper()
	var ids []string
	require.NoError(t, queryRows(t.Context(), s.db, `SELECT asset_id FROM photo_files WHERE role='raw' ORDER BY asset_id DESC LIMIT 3`, nil, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	}))
	require.Len(t, ids, 3)
	for _, i := range []int{0, 2} {
		asset, err := s.PhotoAssetByID(t.Context(), ids[i])
		require.NoError(t, err)
		for _, file := range asset.Files {
			if i == 0 && file.Role != PhotoRoleRAW || i == 2 && file.Role == PhotoRoleImage {
				node := requireNodeByID(t, s, file.NodeID)
				_, _, err := s.Trash(t.Context(), node.ID, node.Revision)
				require.NoError(t, err)
			}
		}
		syncRejectedAsset(t, s, asset.ID)
	}
	return ids[0]
}

func TestPhotoRejectsFileBound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var targets []PhotoAuthoredTarget
	for i := range 252 {
		dir, err := s.Mkdir(t.Context(), s.RootID(), fmt.Sprintf("group-%03d", i))
		require.NoError(t, err)
		asset, nodes := photoTrashFixture(t, s)
		for _, node := range nodes {
			_, _, err = s.Move(t.Context(), *node.ParentID, dir.ID, node.Name+"-folder", UnconditionalRev)
			require.NoError(t, err)
		}
		for _, file := range asset.Files {
			if file.Role != PhotoRoleSidecar {
				targets = append(targets, rejectTarget(file))
			}
		}
	}
	_, err := editPhotoAuthored(t.Context(), s, targets)
	require.NoError(t, err)
	for _, target := range targets {
		if file, err := photoFileByIDQuery(t.Context(), s.db, target.FileID); err == nil && file.Role == PhotoRoleRAW {
			syncRejectedAsset(t, s, file.AssetID)
		}
	}
	lastID := trimRejectsBoundary(t, s)
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	preview, err := s.PreflightPhotoRejects(t.Context(), PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, 252, preview.Photos)
	assert.Equal(t, 1004, preview.Files)
	// After 999 files, a four-file photo waits while the last photo's one file still fits.
	var extraID string
	require.NoError(t, s.db.QueryRow(
		`SELECT MAX(asset_id) FROM photo_files WHERE asset_id<>?`, lastID).Scan(&extraID))
	require.Len(t, preview.Targets, 251)
	assert.Equal(t, lastID, preview.Targets[250].AssetID)
	extra, err := s.PhotoAssetByID(t.Context(), extraID)
	require.NoError(t, err)
	var memberRevision int64
	for _, file := range extra.Files {
		node, err := s.NodeByID(t.Context(), file.NodeID)
		require.NoError(t, err)
		if file.Role != PhotoRoleSidecar {
			memberRevision += node.Revision
		}
	}
	oversized := append([]PhotoRejectTarget{}, preview.Targets...)
	oversized = append(oversized, PhotoRejectTarget{
		AssetID: extra.ID, Revision: extra.Revision, MemberRevision: memberRevision,
	})
	_, err = s.MovePhotoRejects(t.Context(), false, oversized)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.ErrorContains(t, err, "at most 1000 live files")
	unchanged, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Len(t, unchanged, 4)
	_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
	require.NoError(t, err)
	preview, err = s.PreflightPhotoRejects(t.Context(), PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Photos)
	require.Len(t, preview.Targets, 1)
	assert.Equal(t, extraID, preview.Targets[0].AssetID)
	assert.Equal(t, 4, preview.Files)
	_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
	require.NoError(t, err)
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Len(t, roots, 1008)
}

func TestPhotoRejectsMixedAndUnchanged(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	expectedMembers := make(map[string][]PhotoRejectMember)
	for i := range 21 {
		dir, err := s.Mkdir(t.Context(), s.RootID(), fmt.Sprintf("mixed-%d", i))
		require.NoError(t, err)
		asset, nodes := photoTrashFixture(t, s)
		for _, node := range nodes {
			_, _, err = s.Move(t.Context(), *node.ParentID, dir.ID, node.Name+"-folder", UnconditionalRev)
			require.NoError(t, err)
		}
		raw, image := fileByRole(asset.Files, PhotoRoleRAW), fileByRole(asset.Files, PhotoRoleImage)
		_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: raw.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}}})
		require.NoError(t, err)
		_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: image.ID, Revision: image.Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
		require.NoError(t, err)
		node, err := s.NodeByID(t.Context(), image.NodeID)
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
		require.NoError(t, err)
		rawNode, err := s.NodeByID(t.Context(), raw.NodeID)
		require.NoError(t, err)
		expectedMembers[asset.ID] = []PhotoRejectMember{
			{raw.ID, rawNode.Name, "reject", false},
			{image.ID, node.Name, "pick", true},
		}
	}
	node, err := s.CreateFile(t.Context(), s.RootID(), "rejected.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	rejectOriginals(t, s, asset)
	_, err = s.CreateFile(t.Context(), s.RootID(), "ordinary.jpg", fakeHash("c3"), 1, "image/jpeg")
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	request := PhotoRejectsRequest{Query: value}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, query.MaxIDValues, preview.SelectionLimit)
	assert.Equal(t, 1, preview.Photos)
	assert.Equal(t, 22, preview.Unchanged)
	assert.Equal(t, 21, preview.MixedCount)
	require.Len(t, preview.Mixed, 20)
	for _, pair := range preview.Mixed {
		assert.ElementsMatch(t, expectedMembers[pair.AssetID], pair.Members)
	}
}
