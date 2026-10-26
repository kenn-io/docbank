package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhotoTrashFolderXMPQueue(t *testing.T) {
	t.Parallel()
	s, folder := photoXMPPairs(t, 2)
	queued := func() int {
		var count int
		require.NoError(t, s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM photo_xmp_unsettled").Scan(&count))
		return count
	}
	require.Equal(t, 2, queued())
	trashed, _, err := s.Trash(t.Context(), folder.ID, UnconditionalRev)
	require.NoError(t, err)
	require.Zero(t, queued(), "trashed originals leave the queue even when sidecars update afterward")
	_, _, err = s.Restore(t.Context(), trashed.ID, trashed.Revision)
	require.NoError(t, err)
	require.Equal(t, 2, queued(), "restoring the folder queues both originals again")
}

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

// photoCheckoutCommitFixture commits an edit of the photo's JPEG through a real
// checkout, after renaming it so the receipt's node revisions match the asset's.
func photoCheckoutCommitFixture(t *testing.T, s *Store, asset PhotoAsset) {
	t.Helper()
	image := fileByRole(asset.Files, PhotoRoleImage)
	node, err := s.NodeByID(t.Context(), image.NodeID)
	require.NoError(t, err)
	_, _, err = s.Move(t.Context(), node.ID, *node.ParentID, "renamed.jpg", node.Revision)
	require.NoError(t, err)
	c, _, _, err := s.CreatePhotoCheckout(t.Context(), PhotoCheckoutSelection{AssetIDs: []string{asset.ID}}, "machine-local-checkout", "", 100)
	require.NoError(t, err)
	entries, err := s.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	index := slices.IndexFunc(entries, func(e PhotoCheckoutEntry) bool { return e.FileID == image.ID })
	require.GreaterOrEqual(t, index, 0)
	require.NoError(t, s.PublishPhotoCheckoutEntry(t.Context(), entries[index], func() (bool, error) { return true, nil }))
	require.NoError(t, s.FinishPhotoCheckout(t.Context(), c.ID, c.OperationID, StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	c, commit, err := s.PreparePhotoCheckoutOperation(t.Context(), PhotoCheckoutInspection{CheckoutID: c.ID}, StorageOperationKindPhotoCheckoutCommit)
	require.NoError(t, err)
	_, err = s.ClaimStorageOperation(t.Context(), commit.ID)
	require.NoError(t, err)
	require.NoError(t, s.CommitPhotoCheckoutEntry(t.Context(), c, entries[index], browseHash("edit"), 23, "image/png", BlobPhysical{Encoding: "raw", StoredBytes: 23}))
}

// A revision that only sidecar-only steps have passed is the one a person viewed before
// the sync worker attached a sidecar. Operations that change membership still need the exact one.
func TestPhotoMutationRevisionDrift(t *testing.T) {
	t.Parallel()
	for _, op := range []struct {
		name       string
		membership bool
		run        func(*Store, PhotoAsset, []Node, int64) error
	}{
		{"trash", false, func(s *Store, a PhotoAsset, _ []Node, rev int64) error {
			_, err := s.TrashPhotoAsset(t.Context(), a.ID, rev)
			return err
		}},
		{"hide", false, func(s *Store, a PhotoAsset, _ []Node, rev int64) error {
			_, err := s.SetPhotoAssetHidden(t.Context(), a.ID, rev, true)
			return err
		}},
		{"display", false, func(s *Store, a PhotoAsset, _ []Node, rev int64) error {
			_, err := s.SetPhotoDisplay(t.Context(), a.ID, rev, new(fileByRole(a.Files, PhotoRoleRAW).ID))
			return err
		}},
		{"exclude", false, func(s *Store, a PhotoAsset, _ []Node, rev int64) error {
			_, err := s.SetPhotoAssetExcluded(t.Context(), a.ID, rev, true)
			return err
		}},
		{"authored edit", false, func(s *Store, a PhotoAsset, _ []Node, rev int64) error {
			raw := fileByRole(a.Files, PhotoRoleRAW)
			node, err := s.NodeByID(t.Context(), raw.NodeID)
			require.NoError(t, err)
			_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{
				Targets: []PhotoAuthoredEditTarget{{AssetID: a.ID, AssetRevision: rev, FileID: raw.ID, Revision: raw.Revision, NodeRevision: node.Revision}},
				Patch:   PhotoAuthoredPatch{Rating: new(4)},
			})
			return err
		}},
		{"attach", true, func(s *Store, a PhotoAsset, nodes []Node, rev int64) error {
			_, err := s.AttachPhotoFile(t.Context(), a.ID, rev, nodes[2].ID, PhotoRoleSidecar, new(fileByRole(a.Files, PhotoRoleRAW).ID))
			return err
		}},
		{"detach", true, func(s *Store, a PhotoAsset, _ []Node, rev int64) error {
			_, err := s.DetachPhotoFile(t.Context(), a.ID, rev, fileByRole(a.Files, PhotoRoleSidecar).ID, PhotoDetachOptions{})
			return err
		}},
	} {
		// The drift rule is one helper: trash alone runs every gap, and the rest run the sidecar-only one.
		gaps := []struct {
			name     string
			steps    int64
			mixed    bool
			checkout bool
		}{{"after sidecar-only steps", 1, false, false}, {"after sidecar-only steps and a checkout commit", 1, false, true}, {"after a mixed change", 2, true, false}}
		if op.name != "trash" {
			gaps = gaps[:1]
		}
		for _, gap := range gaps {
			t.Run(op.name+" "+gap.name, func(t *testing.T) {
				s := newTestStore(t)
				require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
				asset, nodes := photoTrashFixture(t, s)
				if gap.checkout {
					photoCheckoutCommitFixture(t, s, asset)
				}
				err := op.run(s, asset, nodes, asset.Revision-gap.steps)
				if op.membership || gap.mixed {
					require.ErrorIs(t, err, ErrStaleRevision)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

// Trashing a folder takes each photo and its sidecar together, so no removal record is written and
// restoring the folder finds every photo's sync history as it was.
func TestPhotoTrashFolderKeepsSyncReceipts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	shoot, err := s.Mkdir(ctx, s.RootID(), "shoot")
	require.NoError(t, err)
	var raws []PhotoFile
	for i := range 3 {
		dir, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("group-%d", i))
		require.NoError(t, err)
		asset, nodes := photoTrashFixture(t, s)
		for _, node := range nodes {
			_, _, err = s.Move(ctx, *node.ParentID, dir.ID, node.Name+"-folder", UnconditionalRev)
			require.NoError(t, err)
		}
		raw := fileByRole(asset.Files, PhotoRoleRAW)
		importXMP(t, s, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
		raws = append(raws, raw)
		_, _, err = s.Move(ctx, dir.ID, shoot.ID, dir.Name, UnconditionalRev)
		require.NoError(t, err)
	}
	receiptIDs := func() []string {
		var ids []string
		require.NoError(t, queryRows(ctx, s.db, `SELECT receipt_id FROM photo_change_receipts WHERE operation='xmp_sync' ORDER BY receipt_id`, nil, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		}))
		return ids
	}
	before := receiptIDs()
	require.Len(t, before, 3)
	shoot = requireNodeByID(t, s, shoot.ID)
	_, _, err = s.Trash(ctx, shoot.ID, shoot.Revision)
	require.NoError(t, err)
	require.Equal(t, before, receiptIDs(), "trashing a photo with its sidecar writes no removal record")
	_, _, err = s.Restore(ctx, shoot.ID, UnconditionalRev)
	require.NoError(t, err)
	for _, raw := range raws {
		work, resume, err := s.ClassifyPhotoXMP(ctx, raw.ID)
		require.NoError(t, err)
		require.True(t, resume, "sync resumes after the restore")
		require.NotNil(t, work.History, "the settled history survives the trash")
	}
}

func TestPhotoTrashAndRestoreMember(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		t.Run("audited="+strconv.FormatBool(audited), func(t *testing.T) {
			s := newTestStore(t)
			asset, nodes := photoTrashFixture(t, s)
			album, err := s.CreatePhotoSet(t.Context(), "Trip")
			require.NoError(t, err)
			album, err = s.ChangePhotoSetMembers(t.Context(), album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
			require.NoError(t, err)
			if audited {
				seedInitialAuditAuthority(t, s, s.RootID())
			}
			trashed, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
			require.NoError(t, err)
			assert.Equal(t, asset.Revision+1, trashed.Revision)
			assert.Equal(t, asset.Files, trashed.Files)
			receipts := photoReceiptRows(t, s, asset.ID)
			_, err = s.TrashPhotoAsset(t.Context(), asset.ID, trashed.Revision)
			require.ErrorIs(t, err, ErrInvalidPhotoAsset)
			assert.Contains(t, err.Error(), "photo has no live files to trash")
			unchanged, err := s.PhotoAssetByID(t.Context(), asset.ID)
			require.NoError(t, err)
			assert.Equal(t, trashed.Revision, unchanged.Revision)
			assert.Equal(t, receipts, photoReceiptRows(t, s, asset.ID))
			page, total, err := s.TrashedRootsPage(t.Context(), 1, 0)
			require.NoError(t, err)
			require.Len(t, page, 1)
			assert.Equal(t, 1, total)
			assert.Equal(t, asset.ID, page[0].PhotoAssetID)
			assert.Equal(t, nodes[0].ID, page[0].ID)
			assert.Equal(t, 3, page[0].PhotoFileCount)
			unpaged, err := s.TrashedRoots(t.Context())
			require.NoError(t, err)
			assert.Len(t, unpaged, 3)
			if !audited {
				_, err = s.CreateFile(t.Context(), *nodes[1].ParentID, nodes[1].Name, fakeHash("b2"), 1, "text/plain")
				require.NoError(t, err)
			}
			selected, err := s.NodeByID(t.Context(), nodes[2].ID)
			require.NoError(t, err)
			_, _, err = s.Restore(t.Context(), selected.ID, selected.Revision-1)
			require.ErrorIs(t, err, ErrStaleRevision)
			require.ErrorContains(t, err, fmt.Sprintf("node %d at revision %d, expected %d",
				selected.ID, selected.Revision, selected.Revision-1))
			_, _, err = s.Restore(t.Context(), selected.ID, selected.Revision)
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
			if !audited {
				assert.Equal(t, "capture (2).jpg", jpeg.Name)
			}
			current, err := s.PhotoAssetByID(t.Context(), asset.ID)
			require.NoError(t, err)
			assert.Equal(t, asset.Revision+2, current.Revision)
			assert.Contains(t, receiptOperations(photoReceiptRows(t, s, asset.ID)), "trash")
			assert.Contains(t, receiptOperations(photoReceiptRows(t, s, asset.ID)), "restore")
			members, err := photoSetMemberIDs(t.Context(), s.db, album.ID)
			require.NoError(t, err)
			assert.Equal(t, []string{asset.ID}, members)
			require.NoError(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func TestPhotoRestoreOriginalParentOrder(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"parent", "nested parent", "folder member"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			_, nodes := photoTrashFixture(t, s)
			var selected Node
			var original string
			var unrelatedID int64
			if mode == "folder member" {
				other, err := s.CreateFile(t.Context(), *nodes[0].ParentID, "notes.txt", fakeHash("b2"), 1, "text/plain")
				require.NoError(t, err)
				unrelatedID = other.ID
				_, _, err = s.Trash(t.Context(), other.ID, UnconditionalRev)
				require.NoError(t, err)
				_, _, err = s.Trash(t.Context(), *nodes[0].ParentID, UnconditionalRev)
				require.NoError(t, err)
				for _, node := range nodes[1:] {
					_, _, err = s.Trash(t.Context(), node.ID, UnconditionalRev)
					require.NoError(t, err)
				}
				selected, err = s.NodeByID(t.Context(), nodes[0].ID)
				require.NoError(t, err)
			} else {
				parent, err := s.NodeByID(t.Context(), *nodes[0].ParentID)
				require.NoError(t, err)
				container := parent
				if mode == "nested parent" {
					container, err = s.Mkdir(t.Context(), s.RootID(), "Trips")
					require.NoError(t, err)
					parent, _, err = s.Move(t.Context(), parent.ID, container.ID, parent.Name, UnconditionalRev)
					require.NoError(t, err)
				}
				_, _, err = s.Move(t.Context(), nodes[1].ID, container.ID, nodes[1].Name, UnconditionalRev)
				require.NoError(t, err)
				ordinary, err := s.CreateFile(t.Context(), parent.ID, "notes.txt", fakeHash("b2"), 1, "text/plain")
				require.NoError(t, err)
				selected, original, err = s.Trash(t.Context(), nodes[0].ID, UnconditionalRev)
				require.NoError(t, err)
				folder, _, err := s.Trash(t.Context(), container.ID, UnconditionalRev)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE nodes SET trashed_at=? WHERE trashed_at=?`, time.Now().UTC().Add(-time.Hour).Format(timestampLayout), *folder.TrashedAt)
				require.NoError(t, err)
				_, _, err = s.Restore(t.Context(), ordinary.ID, UnconditionalRev)
				require.ErrorIs(t, err, ErrNotTrashed)
			}
			_, restoredPath, err := s.Restore(t.Context(), selected.ID, selected.Revision)
			require.NoError(t, err)
			if original != "" {
				assert.Equal(t, original, restoredPath)
			}
			for _, node := range nodes {
				restored, err := s.NodeByID(t.Context(), node.ID)
				require.NoError(t, err)
				assert.Nil(t, restored.TrashedAt)
			}
			if unrelatedID != 0 {
				unrelated, err := s.NodeByID(t.Context(), unrelatedID)
				require.NoError(t, err)
				assert.NotNil(t, unrelated.TrashedAt)
			}
		})
	}
}

func TestPhotoTrashCompletesPartialGroup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	image := fileByRole(asset.Files, PhotoRoleImage)
	asset, err := s.SetPhotoDisplay(t.Context(), asset.ID, asset.Revision, &image.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), nodes[0].ID, UnconditionalRev)
	require.NoError(t, err)
	rows, total, err := s.TrashedRootsPage(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	assert.Equal(t, 1, rows[0].PhotoFileCount)
	assert.Equal(t, nodes[0].ID, rows[0].ID)
	_, err = s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	rows, _, err = s.TrashedRootsPage(t.Context(), 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, rows[0].PhotoFileCount)
	assert.Equal(t, nodes[1].ID, rows[0].ID)
}

func TestPhotoTrashPageUsesLatestMemberTime(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	_, _, err := s.Trash(t.Context(), nodes[0].ID, UnconditionalRev)
	require.NoError(t, err)
	old := time.Now().UTC().Add(-48 * time.Hour).Format(timestampLayout)
	_, err = s.db.Exec(`UPDATE nodes SET trashed_at=? WHERE id=?`, old, nodes[0].ID)
	require.NoError(t, err)
	doc, err := s.CreateFile(t.Context(), s.RootID(), "notes.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), doc.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE nodes SET trashed_at=? WHERE id=?`, time.Now().UTC().Add(-24*time.Hour).Format(timestampLayout), doc.ID)
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	newest, err := s.NodeByID(t.Context(), nodes[1].ID)
	require.NoError(t, err)
	page, total, err := s.TrashedRootsPage(t.Context(), 1, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, page, 1)
	assert.Equal(t, nodes[0].ID, page[0].ID)
	assert.Equal(t, newest.TrashedAt, page[0].TrashedAt)
	page, _, err = s.TrashedRootsPage(t.Context(), 1, 1)
	require.NoError(t, err)
	assert.Equal(t, doc.ID, page[0].ID)
}

func TestTrashPageCountsRootsAcrossPhotoAndDirectoryGroups(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset, nodes := photoTrashFixture(t, s)
	sidecar := fileByRole(asset.Files, PhotoRoleSidecar)
	asset, err := s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, PhotoDetachOptions{})
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	dir, err := s.Mkdir(t.Context(), s.RootID(), "folder")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), dir.ID, "child.txt", fakeHash("b2"), 2, "text/plain")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), dir.ID, UnconditionalRev)
	require.NoError(t, err)
	// An unattached sidecar has a photo_files row but no asset. It remains
	// its own trash root, just like the ordinary directory above.
	_, _, err = s.Trash(t.Context(), nodes[2].ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "live.jpg", fakeHash("d4"), 4, "image/jpeg")
	require.NoError(t, err)

	page, total, err := s.TrashedRootsPage(t.Context(), 10, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	require.Len(t, page, 3)
	assert.ElementsMatch(t, []int64{nodes[0].ID, dir.ID, nodes[2].ID},
		[]int64{page[0].ID, page[1].ID, page[2].ID})

	page, total, err = s.TrashedRootsPage(t.Context(), 10, 3)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.Equal(t, 3, total)
}

func TestPhotoTrashEmptyCompleteGroups(t *testing.T) {
	t.Parallel()
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
				version, err := s.ContentVersionByID(t.Context(), nodes[1].CurrentVersionID)
				require.NoError(t, err)
				_, err = s.RetainSuppliedMedia(t.Context(), MediaPublicationRequest{
					Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000061", Principal: "operator", Verb: "submit_supplied_media", RequestSHA256: browseHash("retained peer"), SourceID: nodes[1].BlobHash},
					SourceID:  nodes[1].BlobHash, ContentVersion: version, CaptureJSON: "{}",
					Occurrence: MediaOccurrenceInput{SourceID: nodes[1].BlobHash, Principal: "operator", Ref: "photo", Revision: "1", Filename: "capture.jpg", MessageJSON: "{}"},
				})
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
			held := map[string]int64{"live peer": 1, "new peer": 2, "retained peer": 2}[mode]
			assert.Equal(t, held, dry.Held)
			assert.Equal(t, held, rep.Held)
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

func TestTrashEmptyDryRunUsesReadSnapshot(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	folder, err := s.Mkdir(t.Context(), s.RootID(), "ordinary")
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), folder.ID, UnconditionalRev)
	require.NoError(t, err)
	writer, err := s.writeDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := s.TrashEmpty(ctx, 0, false)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Candidates)
}

func TestTrashEmptyOrdinaryFoldersAcrossBatches(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range 205 {
		folder, err := s.Mkdir(t.Context(), s.RootID(), fmt.Sprintf("folder-%d", i))
		require.NoError(t, err)
		_, err = s.CreateFile(t.Context(), folder.ID, "notes.txt", fakeHash("b2"), 1, "text/plain")
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), folder.ID, UnconditionalRev)
		require.NoError(t, err)
	}
	for _, count := range []int64{101, 101, 3} {
		dry, err := s.TrashEmptyBounded(t.Context(), 0, 101, false)
		require.NoError(t, err)
		assert.Equal(t, count, dry.Candidates)
		assert.Equal(t, count == 101, dry.More)
		run, err := s.TrashEmptyBounded(t.Context(), 0, 101, true)
		require.NoError(t, err)
		assert.Equal(t, count, run.Deleted)
		assert.Equal(t, dry.More, run.More)
	}
}

func BenchmarkTrashEmptyLarge(b *testing.B) {
	for _, kind := range []string{"files", "folders", "photos"} {
		for _, count := range []int{1000, 10000} {
			b.Run(fmt.Sprintf("%s/%d", kind, count), func(b *testing.B) {
				s, err := Open(filepath.Join(b.TempDir(), "trash.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, s.Close()) })
				require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
					for i := range count {
						var node Node
						var err error
						if kind == "folders" {
							node, err = s.mkdirTx(b.Context(), tx, s.RootID(), fmt.Sprintf("folder-%d", i), nowRFC3339())
							if err == nil {
								_, _, err = s.createFileTx(b.Context(), tx, node.ID, "notes.txt", fakeHash("b2"), 1, "text/plain")
							}
						} else {
							name, mime := fmt.Sprintf("notes-%d.txt", i), "text/plain"
							if kind == "photos" {
								name, mime = fmt.Sprintf("capture-%d.jpg", i), "image/jpeg"
							}
							node, _, err = s.createFileTx(b.Context(), tx, s.RootID(), name, fakeHash("b2"), 1, mime)
							if err == nil && kind == "photos" {
								err = s.enrollNewPhotoFileTx(b.Context(), tx, node)
							}
						}
						if err != nil {
							return err
						}
						if err := s.trashNodeTx(b.Context(), tx, node, nowRFC3339()); err != nil {
							return err
						}
					}
					return nil
				}))
				b.ReportAllocs()
				for b.Loop() {
					result, err := s.TrashEmpty(b.Context(), 0, false)
					require.NoError(b, err)
					require.Equal(b, int64(count), result.Candidates)
				}
			})
		}
	}
}

func TestPhotoTrashLateFailureRollsBack(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"late failure trigger", "audit unsupported"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestStore(t)
			asset, nodes := photoTrashFixture(t, s)
			if mode == "audit unsupported" {
				seedInitialAuditAuthority(t, s, *nodes[0].ParentID)
			} else {
				_, err := s.db.Exec(`CREATE TRIGGER fail_photo_trash BEFORE UPDATE OF trashed_at ON nodes WHEN NEW.id=` + strconv.FormatInt(nodes[2].ID, 10) + ` AND NEW.trashed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'late failure'); END`)
				require.NoError(t, err)
			}
			_, err := s.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
			if mode == "audit unsupported" {
				require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			} else {
				require.ErrorContains(t, err, "late failure")
			}
			for _, before := range nodes {
				after, err := s.NodeByID(t.Context(), before.ID)
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
		})
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

func TestPhotoRestoreFollowsCompanionsAcrossFolders(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	pair := func(rawDir, jpegDir Node, name string) {
		raw, err := s.CreateFile(t.Context(), rawDir.ID, name+".raw", fakeHash(name+"1"), 1, "application/octet-stream")
		require.NoError(t, err)
		jpeg, err := s.CreateFile(t.Context(), jpegDir.ID, name+".jpg", fakeHash(name+"2"), 1, "image/jpeg")
		require.NoError(t, err)
		auto, err := s.PhotoAssetForNode(t.Context(), jpeg.ID)
		require.NoError(t, err)
		_, err = s.DetachPhotoFile(t.Context(), auto.ID, auto.Revision, auto.Files[0].ID, PhotoDetachOptions{})
		require.NoError(t, err)
		asset, err := s.CreatePhotoAsset(t.Context(), raw.ID, PhotoRoleRAW, PhotoKindPhoto)
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
		require.NoError(t, err)
	}
	var folders []Node
	for _, name := range []string{"a", "b", "c", "unrelated"} {
		folder, err := s.Mkdir(t.Context(), s.RootID(), name)
		require.NoError(t, err)
		folders = append(folders, folder)
	}
	pair(folders[1], folders[0], "x")
	pair(folders[2], folders[1], "y")
	for _, folder := range folders {
		_, _, err := s.Trash(t.Context(), folder.ID, UnconditionalRev)
		require.NoError(t, err)
	}
	selected, err := s.NodeByID(t.Context(), folders[0].ID)
	require.NoError(t, err)
	_, _, err = s.Restore(t.Context(), selected.ID, selected.Revision)
	require.NoError(t, err)
	for index, folder := range folders {
		current, err := s.NodeByID(t.Context(), folder.ID)
		require.NoError(t, err)
		assert.Equal(t, index == 3, current.TrashedAt != nil, folder.Name)
	}
}
