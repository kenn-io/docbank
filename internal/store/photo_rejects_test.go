package store

import (
	"context"
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
	_, err := s.EditPhotoAuthored(t.Context(), targets)
	require.NoError(t, err)
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
	rejectOriginals(t, s, asset)
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
	rejectOriginals(t, s, uncovered)
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
	rejectOriginals(t, s, asset)
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
	changes := []string{
		"flag reread", "revision", "replace sidecar", "hidden state", "duplicate target", "no live files", "hidden locked at preflight and at move",
	}
	for _, change := range changes {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			asset, nodes := photoTrashFixture(t, s)
			rejectOriginals(t, s, asset)
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
			asset, err = s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
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
				for _, node := range nodes {
					current, err := s.NodeByID(ctx, node.ID)
					require.NoError(t, err)
					memberRevision += current.Revision
				}
				preview.Targets = []PhotoRejectTarget{{AssetID: trashed.ID, Revision: trashed.Revision, MemberRevision: memberRevision}}
			case "flag reread":
				_, err = s.db.Exec(`UPDATE photo_files SET flag='pick' WHERE file_id=?`, original.ID)
			case "replace sidecar":
				node, readErr := s.NodeByID(ctx, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
				require.NoError(t, readErr)
				replaced, _, replaceErr := s.ReplaceContent(
					ctx, node.ID, node.Revision, fakeHash("bc"), 8, node.MimeType)
				require.NoError(t, replaceErr)
				require.Greater(t, replaced.Revision, node.Revision)
				current, readErr := s.PhotoAssetByID(ctx, asset.ID)
				require.NoError(t, readErr)
				require.Equal(t, preview.Targets[0].Revision, current.Revision)
			case "hidden state":
				_, err = s.db.Exec(`UPDATE photo_assets SET hidden_at=? WHERE asset_id=?`, nowRFC3339(), asset.ID)
			case "revision":
				_, err = s.db.Exec(`UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
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

func TestPhotoRejectsAuditRefusal(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	node, err := s.CreateFile(
		t.Context(), s.RootID(), "live-reject.jpg", fakeHash("a1"), 1, "image/jpeg")
	require.NoError(t, err)
	live, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	rejectOriginals(t, s, live)
	dir, err := s.Mkdir(t.Context(), s.RootID(), "audited")
	require.NoError(t, err)
	for i := range 2 {
		node, err := s.CreateFile(t.Context(), dir.ID, fmt.Sprintf("supported-%d.jpg", i), fakeHash("b2"), 1, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		rejectOriginals(t, s, asset)
	}
	seedInitialAuditAuthority(t, s, dir.ID)
	preview, err := s.PreflightPhotoRejects(t.Context(), rejectsRequest())
	require.NoError(t, err)
	require.Len(t, preview.Targets, 3)
	var supported []PhotoRejectTarget
	var blocked PhotoRejectTarget
	for _, target := range preview.Targets {
		if target.AssetID == live.ID {
			blocked = target
		} else {
			supported = append(supported, target)
		}
	}
	require.Len(t, supported, 2)
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	var sequence int64
	require.NoError(t, s.db.QueryRow(`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&sequence))
	_, err = s.MovePhotoRejects(t.Context(), false, append(supported, blocked))
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	var after int64
	require.NoError(t, s.db.QueryRow(`SELECT operation_sequence_high_water FROM audit_authority`).Scan(&after))
	assert.Equal(t, sequence, after)
	current, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Equal(t, roots, current)
}

func TestPhotoRejectsFileBound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var targets []PhotoAuthoredTarget
	for i := range 335 {
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
	_, err := s.EditPhotoAuthored(t.Context(), targets)
	require.NoError(t, err)
	var lastID string
	require.NoError(t, s.db.QueryRow(`SELECT MAX(asset_id) FROM photo_files`).Scan(&lastID))
	last, err := s.PhotoAssetByID(t.Context(), lastID)
	require.NoError(t, err)
	for _, file := range last.Files {
		if file.Role != PhotoRoleRAW {
			node, readErr := s.NodeByID(t.Context(), file.NodeID)
			require.NoError(t, readErr)
			_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
			require.NoError(t, err)
		}
	}
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	preview, err := s.PreflightPhotoRejects(t.Context(), PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, 335, preview.Photos)
	assert.Equal(t, 1003, preview.Files)
	// The 334th photo's three files overflow the batch; the last photo's one live file still fits.
	var extraID string
	require.NoError(t, s.db.QueryRow(
		`SELECT MAX(asset_id) FROM photo_files WHERE asset_id<>?`, lastID).Scan(&extraID))
	require.Len(t, preview.Targets, 334)
	assert.Equal(t, lastID, preview.Targets[333].AssetID)
	extra, err := s.PhotoAssetByID(t.Context(), extraID)
	require.NoError(t, err)
	var memberRevision int64
	for _, file := range extra.Files {
		node, err := s.NodeByID(t.Context(), file.NodeID)
		require.NoError(t, err)
		memberRevision += node.Revision
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
	assert.Len(t, unchanged, 2)
	_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
	require.NoError(t, err)
	preview, err = s.PreflightPhotoRejects(t.Context(), PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Photos)
	require.Len(t, preview.Targets, 1)
	assert.Equal(t, extraID, preview.Targets[0].AssetID)
	assert.Equal(t, 3, preview.Files)
	_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
	require.NoError(t, err)
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Len(t, roots, 1005)
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
		_, err = s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{
			{FileID: raw.ID, Revision: raw.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}},
			{FileID: image.ID, Revision: image.Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}},
		})
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
