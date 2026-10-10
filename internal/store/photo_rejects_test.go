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
			targets = append(targets, PhotoAuthoredTarget{FileID: file.ID, Revision: file.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}})
		}
	}
	_, err := s.EditPhotoAuthored(t.Context(), targets)
	require.NoError(t, err)
}

func rejectsRequest() PhotoRejectsRequest {
	return PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
}

func TestPhotoRejectsPreviewDuringWrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	rejectOriginals(t, s, asset)
	tx, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	_, err = tx.Exec(`UPDATE nodes SET revision=revision+1 WHERE id=?`, asset.Files[0].NodeID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	preview, err := s.PreflightPhotoRejects(ctx, rejectsRequest())
	require.NoError(t, err)
	assert.Len(t, preview.Targets, 1)
}

func TestPhotoRejectsStaleTargets(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"flag", "flag reread", "added", "removed", "trash", "hidden", "unhidden", "revision"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			asset := authoredPair(t, s)
			rejectOriginals(t, s, asset)
			ctx := t.Context()
			request := rejectsRequest()
			if change == "hidden" || change == "unhidden" {
				require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
				token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
				require.NoError(t, err)
				ctx = WithPhotoHiddenToken(ctx, token)
				if change == "unhidden" {
					var err error
					asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
					require.NoError(t, err)
					request.Hidden = true
				}
			}
			preview, err := s.PreflightPhotoRejects(ctx, request)
			require.NoError(t, err)
			require.Len(t, preview.Targets, 1)
			asset, err = s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			switch change {
			case "flag":
				_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
			case "flag reread":
				_, err = s.db.Exec(`UPDATE photo_files SET flag='pick' WHERE file_id=?`, asset.Files[0].ID)
			case "added":
				node, createErr := s.CreateFile(ctx, s.RootID(), "added.raw", fakeHash("a1"), 4, "application/octet-stream")
				require.NoError(t, createErr)
				_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, node.ID, PhotoRoleRAW, nil)
			case "removed":
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
			case "trash":
				for _, file := range asset.Files {
					node, readErr := s.NodeByID(ctx, file.NodeID)
					require.NoError(t, readErr)
					_, _, err = s.Trash(ctx, node.ID, node.Revision)
					require.NoError(t, err)
				}
			case "hidden", "unhidden":
				_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, change == "hidden")
			case "revision":
				_, err = s.db.Exec(`UPDATE photo_assets SET revision=revision+1 WHERE asset_id=?`, asset.ID)
			}
			require.NoError(t, err)
			roots, err := s.TrashedRoots(ctx)
			require.NoError(t, err)
			_, err = s.MovePhotoRejects(ctx, request.Hidden, preview.Targets)
			require.ErrorIs(t, err, ErrStaleRevision)
			current, err := s.TrashedRoots(ctx)
			require.NoError(t, err)
			assert.Equal(t, roots, current)
		})
	}
}

func TestPhotoRejectsPreviewedAlbumTargets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	rejectOriginals(t, s, asset)
	set, err := s.CreatePhotoSet(t.Context(), "Synthetic album")
	require.NoError(t, err)
	set, err = s.ChangePhotoSetMembers(t.Context(), set.ID, set.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{"filters":{"set_ids":["` + set.ID + `"]}}`))
	require.NoError(t, err)
	preview, err := s.PreflightPhotoRejects(t.Context(), PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	require.Len(t, preview.Targets, 1)
	_, err = s.ChangePhotoSetMembers(t.Context(), set.ID, set.Revision, false, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "unrelated.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	moved, err := s.MovePhotoRejects(t.Context(), false, preview.Targets)
	require.NoError(t, err)
	assert.Equal(t, []string{asset.ID}, moved.Moved)
}

func TestPhotoRejectsHiddenLock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	rejectOriginals(t, s, asset)
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	_, err := s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	request := rejectsRequest()
	request.Hidden = true
	_, err = s.PreflightPhotoRejects(t.Context(), request)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(t.Context(), "synthetic-passcode")
	require.NoError(t, err)
	ctx := WithPhotoHiddenToken(t.Context(), token)
	preview, err := s.PreflightPhotoRejects(ctx, request)
	require.NoError(t, err)
	require.Len(t, preview.Targets, 1)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.MovePhotoRejects(ctx, true, preview.Targets)
	require.ErrorIs(t, err, ErrHiddenLocked)
}

func TestPhotoRejectsFileBound(t *testing.T) {
	t.Parallel()
	t.Run("sidecar file cap", func(t *testing.T) {
		s := newTestStore(t)
		var targets []PhotoAuthoredTarget
		for i := range 335 {
			dir, err := s.Mkdir(t.Context(), s.RootID(), fmt.Sprintf("group-%03d", i))
			require.NoError(t, err)
			raw, err := s.CreateFile(t.Context(), dir.ID, "capture.raw", fakeHash("a1"), 1, "application/octet-stream")
			require.NoError(t, err)
			jpg, err := s.CreateFile(t.Context(), dir.ID, "capture.jpg", fakeHash("a1"), 1, "image/jpeg")
			require.NoError(t, err)
			owned, err := s.PhotoAssetForNode(t.Context(), jpg.ID)
			require.NoError(t, err)
			_, err = s.DetachPhotoFile(t.Context(), owned.ID, owned.Revision, owned.Files[0].ID, PhotoDetachOptions{})
			require.NoError(t, err)
			asset, err := s.CreatePhotoAsset(t.Context(), raw.ID, PhotoRoleRAW, PhotoKindPhoto)
			require.NoError(t, err)
			asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, jpg.ID, PhotoRoleImage, nil)
			require.NoError(t, err)
			xmp, err := s.CreateFile(t.Context(), dir.ID, "capture.xmp", fakeHash("a1"), 1, "application/rdf+xml")
			require.NoError(t, err)
			member := fileByRole(asset.Files, PhotoRoleRAW)
			asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, xmp.ID, PhotoRoleSidecar, &member.ID)
			require.NoError(t, err)
			for _, file := range asset.Files {
				if file.Role != PhotoRoleSidecar {
					targets = append(targets, PhotoAuthoredTarget{FileID: file.ID, Revision: file.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}})
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
		assert.Len(t, preview.Targets, 333)
		for _, target := range preview.Targets {
			assert.Less(t, target.AssetID, lastID)
		}
		var extraID string
		require.NoError(t, s.db.QueryRow(`SELECT MAX(asset_id) FROM photo_files WHERE asset_id<>?`, lastID).Scan(&extraID))
		extra, err := s.PhotoAssetByID(t.Context(), extraID)
		require.NoError(t, err)
		oversized := append([]PhotoRejectTarget{}, preview.Targets...)
		oversized = append(oversized, PhotoRejectTarget{extra.ID, extra.Revision})
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
		assert.Equal(t, 2, preview.Photos)
		assert.Len(t, preview.Targets, 2)
		assert.Equal(t, 4, preview.Files)
		_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
		require.NoError(t, err)
		roots, err := s.TrashedRoots(t.Context())
		require.NoError(t, err)
		assert.Len(t, roots, 1005)
	})
}

func TestPhotoRejectsRollbackAllAssets(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"failure", "asset"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			for i := range 2 {
				node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("reject-%d.jpg", i), fakeHash("a1"), 1, "image/jpeg")
				require.NoError(t, err)
				asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, err)
				rejectOriginals(t, s, asset)
			}
			seedInitialAuditAuthority(t, s, s.RootID())
			preview, err := s.PreflightPhotoRejects(t.Context(), rejectsRequest())
			require.NoError(t, err)
			trigger := `CREATE TRIGGER fail_second_trash BEFORE UPDATE OF trashed_at ON nodes WHEN NEW.trashed_at IS NOT NULL AND (SELECT COUNT(*) FROM nodes WHERE trashed_at IS NOT NULL)>0 BEGIN SELECT RAISE(ABORT,'synthetic trash failure'); END`
			if change != "failure" {
				update := `UPDATE photo_assets SET revision=revision+1 WHERE asset_id=(SELECT MAX(asset_id) FROM photo_assets);`
				trigger = `CREATE TRIGGER stale_second_photo AFTER UPDATE OF trashed_at ON nodes WHEN NEW.trashed_at IS NOT NULL BEGIN ` + update + ` END`
			}
			_, err = s.db.Exec(trigger)
			require.NoError(t, err)
			_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
			if change == "failure" {
				require.ErrorContains(t, err, "synthetic trash failure")
			} else {
				require.ErrorIs(t, err, ErrStaleRevision)
			}
			roots, err := s.TrashedRoots(t.Context())
			require.NoError(t, err)
			assert.Empty(t, roots)
			var receipts int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_change_receipts WHERE operation='trash'`).Scan(&receipts))
			assert.Zero(t, receipts)
		})
	}
}

func TestPhotoRejectsTrashedOriginalStillCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var firstRawNodeID int64
	trashedMembers := make(map[string]PhotoRejectMember)
	for i := range 21 {
		first, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("mixed-%d-a.jpg", i), fakeHash("a1"), 4, "image/jpeg")
		require.NoError(t, err)
		second, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("mixed-%d-b.raw", i), fakeHash("a1"), 4, "application/octet-stream")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(t.Context(), first.ID)
		require.NoError(t, err)
		asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, second.ID, PhotoRoleRAW, nil)
		require.NoError(t, err)
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
		trashedMembers[asset.ID] = PhotoRejectMember{image.ID, node.Name, "pick", true}
		if i == 0 {
			firstRawNodeID = raw.NodeID
		}
	}
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	request := PhotoRejectsRequest{Query: value}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Zero(t, preview.Photos)
	assert.Equal(t, 21, preview.Unchanged)
	assert.Equal(t, 21, preview.MixedCount)
	require.Len(t, preview.Mixed, 20)
	for _, pair := range preview.Mixed {
		assert.Contains(t, pair.Members, trashedMembers[pair.AssetID])
	}
	_, err = s.MovePhotoRejects(t.Context(), false, preview.Targets)
	require.NoError(t, err)
	live, err := s.NodeByID(t.Context(), firstRawNodeID)
	require.NoError(t, err)
	assert.Nil(t, live.TrashedAt)
}
