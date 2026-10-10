package store

import (
	"context"
	"fmt"
	"strconv"
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

func TestPhotoRejectsPreviewDuringWrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	_, err := s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
	require.NoError(t, err)
	tx, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	_, err = tx.Exec(`UPDATE nodes SET revision=revision+1 WHERE id=?`, asset.Files[0].NodeID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	preview, err := s.PreflightPhotoRejects(ctx, PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Unchanged)
	assert.Empty(t, preview.Mixed)
}

func TestPhotoRejectsRetainedCompanions(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var targets []PhotoAuthoredTarget
	for i := 0; i < 334; i++ {
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
		if i == 0 {
			rejectOriginals(t, s, asset)
		} else {
			for _, file := range asset.Files {
				if file.Role != PhotoRoleSidecar {
					targets = append(targets, PhotoAuthoredTarget{FileID: file.ID, Revision: file.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}})
				}
			}
		}
	}
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	preview, err := s.PreflightPhotoRejects(t.Context(), PhotoRejectsRequest{Query: value})
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Photos)
	assert.Equal(t, 3, preview.Files)
	assert.Equal(t, 333, preview.Unchanged)
	_, err = s.EditPhotoAuthored(t.Context(), targets)
	require.NoError(t, err)
	_, err = s.MovePhotoRejects(t.Context(), PhotoRejectsRequest{Query: value}, preview.Digest)
	require.ErrorIs(t, err, ErrInvalidPhotoQuery)
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
}

func TestPhotoRejectsAtomicTrashAndRestore(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		t.Run(strconv.FormatBool(audited), func(t *testing.T) {
			s := newTestStore(t)
			asset, nodes := photoTrashFixture(t, s)
			if audited {
				seedInitialAuditAuthority(t, s, s.RootID())
			}
			rejectOriginals(t, s, asset)
			request := PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
			preview, err := s.PreflightPhotoRejects(t.Context(), request)
			require.NoError(t, err)
			assert.Equal(t, 1, preview.Photos)
			assert.Equal(t, 3, preview.Files)
			assert.Empty(t, preview.Mixed)
			_, err = s.MovePhotoRejects(t.Context(), request, preview.Digest)
			require.NoError(t, err)
			assert.Contains(t, receiptOperations(photoReceiptRows(t, s, asset.ID)), "trash")
			for _, node := range nodes {
				current, err := s.NodeByID(t.Context(), node.ID)
				require.NoError(t, err)
				assert.NotNil(t, current.TrashedAt)
			}
			current, err := s.NodeByID(t.Context(), nodes[2].ID)
			require.NoError(t, err)
			_, _, err = s.Restore(t.Context(), current.ID, current.Revision)
			require.NoError(t, err)
			for _, node := range nodes {
				current, err := s.NodeByID(t.Context(), node.ID)
				require.NoError(t, err)
				assert.Nil(t, current.TrashedAt)
			}
		})
	}
}

func TestPhotoRejectsMixedAndStale(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"flag", "membership", "scope", "node"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			asset := authoredPair(t, s)
			request := PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
			_, err := s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Flag: new("reject")}}})
			require.NoError(t, err)
			preview, err := s.PreflightPhotoRejects(t.Context(), request)
			require.NoError(t, err)
			assert.Zero(t, preview.Photos)
			assert.Equal(t, 1, preview.Unchanged)
			require.Len(t, preview.Mixed, 1)
			assert.Len(t, preview.Mixed[0].Members, 2)
			asset, err = s.PhotoAssetByID(t.Context(), asset.ID)
			require.NoError(t, err)
			rejectOriginals(t, s, asset)
			preview, err = s.PreflightPhotoRejects(t.Context(), request)
			require.NoError(t, err)
			switch change {
			case "flag":
				asset, err = s.PhotoAssetByID(t.Context(), asset.ID)
				require.NoError(t, err)
				_, err = s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
			case "membership":
				asset, err = s.PhotoAssetByID(t.Context(), asset.ID)
				require.NoError(t, err)
				_, err = s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
			case "scope":
				_, err = s.CreateFile(t.Context(), s.RootID(), "new.jpg", fakeHash("c3"), 2, "image/jpeg")
			case "node":
				_, err = s.db.Exec(`UPDATE nodes SET revision=revision+1 WHERE id=?`, asset.Files[0].NodeID)
			}
			require.NoError(t, err)
			_, err = s.MovePhotoRejects(t.Context(), request, preview.Digest)
			require.ErrorIs(t, err, ErrStaleRevision)
			for _, file := range asset.Files {
				node, err := s.NodeByID(t.Context(), file.NodeID)
				require.NoError(t, err)
				assert.Nil(t, node.TrashedAt)
			}
		})
	}
}

func TestPhotoRejectsBeyondPageAndOverflow(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var targets []PhotoAuthoredTarget
	for i := 0; i < maxPhotoRejectsMoveTargets+2; i++ {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("photo-%04d.jpg", i), fakeHash("a1"), 1, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		targets = append(targets, PhotoAuthoredTarget{FileID: asset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Flag: new("reject")}})
	}
	_, err := s.EditPhotoAuthored(t.Context(), targets[:1])
	require.NoError(t, err)
	request := PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Photos)
	assert.Equal(t, 1001, preview.Unchanged)
	_, err = s.EditPhotoAuthored(t.Context(), targets[1:1001])
	require.NoError(t, err)
	preview, err = s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, 1001, preview.Photos)
	assert.Equal(t, 1001, preview.Files)
	_, err = s.MovePhotoRejects(t.Context(), request, preview.Digest)
	require.ErrorIs(t, err, ErrInvalidPhotoQuery)
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
	_, err = s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{{FileID: targets[1000].FileID, Revision: 2, Patch: PhotoAuthoredPatch{Flag: new("pick")}}})
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	preview, err = s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, 1000, preview.Photos)
	assert.Equal(t, 1000, preview.Files)
	assert.Equal(t, 2, preview.Unchanged)
	started := time.Now()
	moved, err := s.MovePhotoRejects(t.Context(), request, preview.Digest)
	t.Logf("audited 1,000-photo confirmation: %s", time.Since(started))
	require.NoError(t, err)
	assert.Equal(t, preview, moved)
	roots, err = s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Len(t, roots, 1000)
	var receipts int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM photo_change_receipts WHERE operation='trash'`).Scan(&receipts))
	assert.Equal(t, 1000, receipts)
}

func TestPhotoRejectsHiddenScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	rejectOriginals(t, s, asset)
	require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	_, err := s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	request := PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}, Hidden: true}
	_, err = s.PreflightPhotoRejects(t.Context(), request)
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(t.Context(), "synthetic-passcode")
	require.NoError(t, err)
	ctx := WithPhotoHiddenToken(t.Context(), token)
	visible, err := s.PreflightPhotoRejects(ctx, PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}})
	require.NoError(t, err)
	assert.Zero(t, visible.Photos)
	preview, err := s.PreflightPhotoRejects(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, 1, preview.Photos)
	require.NoError(t, s.LockPhotoHidden(ctx))
	_, err = s.MovePhotoRejects(ctx, request, preview.Digest)
	require.ErrorIs(t, err, ErrHiddenLocked)
	node, err := s.NodeByID(t.Context(), asset.Files[0].NodeID)
	require.NoError(t, err)
	assert.Nil(t, node.TrashedAt)
}

func TestPhotoRejectsRollbackAllAssets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := 0; i < 2; i++ {
		node, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("reject-%d.jpg", i), fakeHash("a1"), 1, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(t.Context(), node.ID)
		require.NoError(t, err)
		rejectOriginals(t, s, asset)
	}
	request := PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	_, err = s.db.Exec(`CREATE TRIGGER fail_second_trash BEFORE UPDATE OF trashed_at ON nodes WHEN NEW.trashed_at IS NOT NULL AND (SELECT COUNT(*) FROM nodes WHERE trashed_at IS NOT NULL)>0 BEGIN SELECT RAISE(ABORT,'synthetic trash failure'); END`)
	require.NoError(t, err)
	_, err = s.MovePhotoRejects(t.Context(), request, preview.Digest)
	require.ErrorContains(t, err, "synthetic trash failure")
	roots, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Empty(t, roots)
	current, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, preview.Digest, current.Digest)
}

func TestPhotoRejectsRetainedEditAndMixedBound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	eligible := authoredPair(t, s)
	rejectOriginals(t, s, eligible)
	for i := range 21 {
		first, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("mixed-%d-a.jpg", i), fakeHash("a1"), 4, "image/jpeg")
		require.NoError(t, err)
		second, err := s.CreateFile(t.Context(), s.RootID(), fmt.Sprintf("mixed-%d-b.raw", i), fakeHash("a1"), 4, "application/octet-stream")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(t.Context(), first.ID)
		require.NoError(t, err)
		asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, second.ID, PhotoRoleRAW, nil)
		require.NoError(t, err)
		_, err = s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}}})
		require.NoError(t, err)
	}
	retained, err := s.CreateFile(t.Context(), s.RootID(), "retained.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	request := PhotoRejectsRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Sort: query.Sort{Field: "name", Direction: "asc"}}}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, 21, preview.MixedCount)
	require.Len(t, preview.Mixed, 20)
	_, err = s.db.Exec(`UPDATE nodes SET revision=revision+1,name='renamed.jpg' WHERE id=?`, retained.ID)
	require.NoError(t, err)
	_, err = s.MovePhotoRejects(t.Context(), request, preview.Digest)
	require.NoError(t, err)
}

func TestPhotoRejectsTrashedOriginalStillCounts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	raw, image := fileByRole(asset.Files, PhotoRoleRAW), fileByRole(asset.Files, PhotoRoleImage)
	_, err := s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{
		{FileID: raw.ID, Revision: raw.Revision, Patch: PhotoAuthoredPatch{Flag: new("reject")}},
		{FileID: image.ID, Revision: image.Revision, Patch: PhotoAuthoredPatch{Flag: new("pick")}},
	})
	require.NoError(t, err)
	node, err := s.NodeByID(t.Context(), image.NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	value, err := query.Parse([]byte(`{}`))
	require.NoError(t, err)
	request := PhotoRejectsRequest{Query: value}
	preview, err := s.PreflightPhotoRejects(t.Context(), request)
	require.NoError(t, err)
	assert.Zero(t, preview.Photos)
	assert.Equal(t, 1, preview.Unchanged)
	require.Len(t, preview.Mixed, 1)
	assert.Contains(t, preview.Mixed[0].Members, PhotoRejectMember{image.ID, node.Name, "pick", true})
	_, err = s.MovePhotoRejects(t.Context(), request, preview.Digest)
	require.NoError(t, err)
	live, err := s.NodeByID(t.Context(), raw.NodeID)
	require.NoError(t, err)
	assert.Nil(t, live.TrashedAt)
}
