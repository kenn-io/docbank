package store

import (
	"bytes"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func photoTrashFixture(t *testing.T, s *Store) (PhotoAsset, []Node) {
	t.Helper()
	var nodes []Node
	for _, name := range []string{"capture.raw", "capture.jpg", "capture.xmp"} {
		dir, err := s.Mkdir(t.Context(), s.RootID(), name+"-folder")
		require.NoError(t, err)
		n, err := s.CreateFile(t.Context(), dir.ID, name, fakeHash("a1"), 1, "application/octet-stream")
		require.NoError(t, err)
		nodes = append(nodes, n)
	}
	jpegAsset, err := s.PhotoAssetForNode(t.Context(), nodes[1].ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(t.Context(), jpegAsset.ID, jpegAsset.Revision, jpegAsset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.CreatePhotoAsset(t.Context(), nodes[0].ID, PhotoRoleRAW, PhotoKindPhoto)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, nodes[1].ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, nodes[2].ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	return asset, nodes
}

func TestPhotoTrashAndRestoreMember(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	album, err := s.CreatePhotoSet(t.Context(), "Trip")
	require.NoError(t, err)
	album, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision-1)
	require.ErrorIs(t, err, ErrStaleRevision)
	trashed, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	assert.Equal(t, asset.Revision+1, trashed.Revision)
	assert.Equal(t, asset.Files, trashed.Files)
	page, total, err := s.TrashedRootsPage(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, 1, total)
	assert.Equal(t, asset.ID, page[0].PhotoAssetID)
	assert.Equal(t, 3, page[0].PhotoFileCount)
	unpaged, err := s.TrashedRoots(t.Context())
	require.NoError(t, err)
	assert.Len(t, unpaged, 3)
	_, err = s.CreateFile(t.Context(), *nodes[1].ParentID, nodes[1].Name, fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Restore(t.Context(), nodes[2].ID, UnconditionalRev)
	require.NoError(t, err)
	for _, before := range nodes {
		after, err := s.NodeByID(t.Context(), before.ID)
		require.NoError(t, err)
		assert.Nil(t, after.TrashedAt)
		assert.Equal(t, before.CurrentVersionID, after.CurrentVersionID)
		assert.Equal(t, before.BlobHash, after.BlobHash)
	}
	jpeg, err := s.NodeByID(t.Context(), nodes[1].ID)
	require.NoError(t, err)
	assert.Equal(t, "capture (2).jpg", jpeg.Name)
	members, err := photoSetMemberIDs(t.Context(), s.db, album.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{asset.ID}, members)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestPhotoRestoreFolderContainedMember(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, nodes := photoTrashFixture(t, s)
	other, err := s.CreateFile(t.Context(), *nodes[0].ParentID, "notes.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), other.ID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), *nodes[0].ParentID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), nodes[1].ID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), nodes[2].ID, UnconditionalRev)
	require.NoError(t, err)
	_, _, err = s.Restore(t.Context(), nodes[0].ID, UnconditionalRev)
	require.NoError(t, err)
	for _, before := range nodes {
		n, err := s.NodeByID(t.Context(), before.ID)
		require.NoError(t, err)
		assert.Nil(t, n.TrashedAt)
	}
	n, err := s.NodeByID(t.Context(), other.ID)
	require.NoError(t, err)
	assert.NotNil(t, n.TrashedAt)
}

func TestPhotoTrashEmptyCompleteGroups(t *testing.T) {
	for _, mode := range []string{"bounded", "live peer", "new peer", "retained peer", "folder"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			asset, nodes := photoTrashFixture(t, s)
			if mode == "live peer" {
				_, _, err := s.Trash(t.Context(), nodes[0].ID, UnconditionalRev)
				require.NoError(t, err)
			} else {
				_, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
				require.NoError(t, err)
			}
			age := time.Duration(0)
			if mode == "retained peer" {
				stamp := nowRFC3339()
				_, err := s.db.Exec(`INSERT INTO media_sources VALUES('source','supplied_media','','',?,?)`, nodes[1].BlobHash, stamp)
				require.NoError(t, err)
				_, err = s.db.Exec(`INSERT INTO media_source_versions VALUES('version','source',1,?,?,?)`, nodes[1].CurrentVersionID, `{}`, stamp)
				require.NoError(t, err)
			}
			if mode == "new peer" {
				_, err := s.db.Exec(`UPDATE nodes SET trashed_at=? WHERE id IN (?, ?)`, time.Now().UTC().Add(-48*time.Hour).Format(timestampLayout), nodes[0].ID, nodes[1].ID)
				require.NoError(t, err)
				age = 24 * time.Hour
			}
			if mode == "folder" {
				_, _, err := s.Restore(t.Context(), nodes[0].ID, UnconditionalRev)
				require.NoError(t, err)
				for _, node := range nodes {
					_, _, err = s.Trash(t.Context(), *node.ParentID, UnconditionalRev)
					require.NoError(t, err)
				}
			}
			dry, err := s.TrashEmptyBounded(t.Context(), age, 1, false)
			require.NoError(t, err)
			rep, err := s.TrashEmptyBounded(t.Context(), age, 1, true)
			require.NoError(t, err)
			assert.Equal(t, dry.Candidates, rep.Deleted)
			assert.False(t, rep.More)
			if mode == "live peer" || mode == "new peer" || mode == "retained peer" {
				assert.Zero(t, rep.Deleted)
				for _, node := range nodes {
					_, err := s.NodeByID(t.Context(), node.ID)
					require.NoError(t, err)
				}
			} else {
				assert.Equal(t, int64(3), rep.Deleted)
				for _, node := range nodes {
					_, err := s.NodeByID(t.Context(), node.ID)
					require.ErrorIs(t, err, ErrNotFound)
				}
			}
		})
	}
}

func TestPhotoTrashLateFailureRollsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	_, err := s.db.Exec(`CREATE TRIGGER fail_photo_trash BEFORE UPDATE OF trashed_at ON nodes WHEN NEW.id=` + strconv.FormatInt(nodes[2].ID, 10) + ` AND NEW.trashed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'late failure'); END`)
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.ErrorContains(t, err, "late failure")
	for _, before := range nodes {
		after, err := s.NodeByID(t.Context(), before.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
}

func TestPhotoTrashJSONLRecovery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	_, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	target, err := Open(filepath.Join(t.TempDir(), "recovered.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	_, _, err = target.Restore(t.Context(), nodes[1].ID, UnconditionalRev)
	require.NoError(t, err)
	for _, node := range nodes {
		restored, err := target.NodeByID(t.Context(), node.ID)
		require.NoError(t, err)
		assert.Nil(t, restored.TrashedAt)
	}
}

func TestPhotoTrashAuditedGroup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
	_, _, err = s.Restore(t.Context(), nodes[1].ID, UnconditionalRev)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(t.Context()))
}

func TestPhotoTrashAuditedUnsupportedMemberRollsBack(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	seedInitialAuditAuthority(t, s, *nodes[0].ParentID)
	_, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	for _, before := range nodes {
		after, err := s.NodeByID(t.Context(), before.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	}
}
