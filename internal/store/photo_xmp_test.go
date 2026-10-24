package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func publishXMPOrientation(t *testing.T, s *Store, nodeID int64, fingerprint string) {
	t.Helper()
	n, err := s.NodeByID(t.Context(), nodeID)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), n.BlobHash, fingerprint, photoCanonical(t, photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(1))))
	require.NoError(t, err)
}

// xmpSidecarPhoto is a RAW+JPEG photo whose extracted RAW has a sidecar.
func xmpSidecarPhoto(t *testing.T) (*Store, PhotoAsset, PhotoFile) {
	t.Helper()
	s, asset, raw, _ := authoredSidecar(t)
	publishXMPOrientation(t, s, raw.NodeID, fakeHash("ee"))
	return s, asset, raw
}

// importXMP syncs sidecar values through publication and returns the undoable import.
func importXMP(t *testing.T, s *Store, fileID string, values PhotoAuthored, keywords ...string) PhotoAuthoredReceipt {
	t.Helper()
	f, err := photoFileByIDQuery(t.Context(), s.db, fileID)
	require.NoError(t, err)
	publishXMPOrientation(t, s, f.NodeID, fakeHash("ee"))
	work, run, err := s.ClassifyPhotoXMP(t.Context(), fileID)
	require.NoError(t, err)
	require.True(t, run)
	packet := PhotoXMPPacket{PhotoAuthored: values, Present: values.Confirmed | PhotoXMPKeywordsBit, Keywords: PhotoXMPKeywords(keywords)}
	packet.Confirmed = 0
	packet.FlagOwner = ""
	plan, mask := planXMP(work, packet, "")
	require.Zero(t, plan.Conflicts)
	require.Zero(t, mask)
	_, err = s.PublishPhotoXMP(t.Context(), plan, "", 0, BlobPhysical{})
	require.NoError(t, err)
	state := xmpState(t, s, fileID)
	require.Equal(t, PhotoXMPStateSynced, state.State)
	require.NotEmpty(t, state.ImportReceiptID)
	receipt, err := loadPhotoAuthoredReceipt(t.Context(), s.db, state.ImportReceiptID)
	require.NoError(t, err)
	return receipt
}

// planXMP compares packet as the sidecar's current version.
func planXMP(work PhotoXMPWork, packet PhotoXMPPacket, choice string) (PhotoXMPPlan, PhotoAuthoredFields) {
	steps := work.Steps()
	steps[len(steps)-1].Packet = packet
	return PlanPhotoXMP(work, steps, choice)
}

func xmpState(t *testing.T, s *Store, fileID string) PhotoXMPState {
	t.Helper()
	f, err := photoFileByIDQuery(t.Context(), s.db, fileID)
	require.NoError(t, err)
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: []string{f.AssetID}})
	require.NoError(t, err)
	for _, target := range prepared.Targets {
		if target.FileID == fileID {
			return *target.XMP
		}
	}
	t.Fatal("original missing from authored preparation")
	return PhotoXMPState{}
}

func xmpTargets(t *testing.T, s *Store) []PhotoSidecarTarget {
	t.Helper()
	targets, err := s.PhotoXMPTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	return targets
}
func TestPhotoXMPTargetCursorVisitsEveryOriginalOnce(t *testing.T) {
	t.Parallel()
	t.Run("originals in one asset", func(t *testing.T) {
		s, asset, raw := xmpSidecarPhoto(t)
		jpeg := fileByRole(asset.Files, PhotoRoleImage)
		publishXMPOrientation(t, s, jpeg.NodeID, fakeHash("ee"))
		_, err := editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{
			{FileID: raw.ID, Revision: raw.Revision, Patch: PhotoAuthoredPatch{Rating: new(3)}},
			{FileID: jpeg.ID, Revision: jpeg.Revision, Patch: PhotoAuthoredPatch{Rating: new(3)}},
		})
		require.NoError(t, err)
		var seen []string
		after := ""
		for {
			page, err := s.PhotoXMPTargetsAfter(t.Context(), after, 1)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].FileID)
			after = page[0].Cursor()
		}
		require.ElementsMatch(t, []string{raw.ID, jpeg.ID}, seen, "a cursor inside an asset keeps its other originals")
	})
	t.Run("originals sharing bytes", func(t *testing.T) {
		s, asset, raw := xmpSidecarPhoto(t)
		n := requireNodeByID(t, s, raw.NodeID)
		sidecar := requireNodeByID(t, s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
		other, err := s.CreateFile(t.Context(), s.RootID(), "second.jpg", n.BlobHash, n.Size, "image/jpeg")
		require.NoError(t, err)
		shared, err := s.CreateFile(t.Context(), s.RootID(), "second.xmp", sidecar.BlobHash, sidecar.Size, "application/rdf+xml")
		require.NoError(t, err)
		group, err := s.PhotoAssetForNode(t.Context(), other.ID)
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(t.Context(), group.ID, group.Revision, shared.ID, PhotoRoleSidecar, &group.Files[0].ID)
		require.NoError(t, err)
		first, err := s.PhotoXMPTargetsAfter(t.Context(), "", 1)
		require.NoError(t, err)
		require.Len(t, first, 1)
		second, err := s.PhotoXMPTargetsAfter(t.Context(), first[0].Cursor(), 1)
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.ElementsMatch(t, []int64{raw.NodeID, other.ID}, []int64{first[0].NodeID, second[0].NodeID})
		assert.Greater(t, second[0].Cursor(), first[0].Cursor())
		last, err := s.PhotoXMPTargetsAfter(t.Context(), second[0].Cursor(), 1)
		require.NoError(t, err)
		require.Empty(t, last)
	})
}

// settleXMP classifies an original; packet merges remain Pending.
func settleXMP(t *testing.T, s *Store, fileID string) PhotoXMPState {
	t.Helper()
	_, _, err := s.ClassifyPhotoXMP(t.Context(), fileID)
	require.NoError(t, err)
	return xmpState(t, s, fileID)
}

func TestPhotoXMPInvalidation(t *testing.T) {
	t.Parallel()
	type fixture struct {
		t   *testing.T
		s   *Store
		raw PhotoFile
		tag Tag
	}
	restoreOriginal := func(audited bool) func(fixture) {
		return func(f fixture) {
			if audited {
				seedInitialAuditAuthority(f.t, f.s, f.s.RootID())
			}
			n := requireNodeByID(f.t, f.s, f.raw.NodeID)
			_, _, err := f.s.Trash(f.t.Context(), n.ID, n.Revision)
			require.NoError(f.t, err)
			_, run, err := f.s.ClassifyPhotoXMP(f.t.Context(), f.raw.ID)
			require.NoError(f.t, err)
			require.False(f.t, run)
			require.Empty(f.t, xmpTargets(f.t, f.s), "a retained JPEG does not make a trashed RAW pending")
			_, _, err = f.s.Restore(f.t.Context(), n.ID, UnconditionalRev)
			require.NoError(f.t, err)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(fixture)
		before string
	}{
		{"original restore", restoreOriginal(false), PhotoXMPStatePending},
		{"audited original restore", restoreOriginal(true), PhotoXMPStatePending},
		{"audit enrollment", func(f fixture) {
			other, err := f.s.Mkdir(f.t.Context(), f.s.RootID(), "Other")
			require.NoError(f.t, err)
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			for _, file := range asset.Files {
				n := requireNodeByID(f.t, f.s, file.NodeID)
				_, _, err = f.s.Move(f.t.Context(), n.ID, other.ID, n.Name, n.Revision)
				require.NoError(f.t, err)
			}
			scope, err := f.s.Mkdir(f.t.Context(), f.s.RootID(), "Audited")
			require.NoError(f.t, err)
			seedInitialAuditAuthority(f.t, f.s, scope.ID)
			require.Equal(f.t, PhotoXMPStateUnavailable, settleXMP(f.t, f.s, f.raw.ID).State)
			plan, err := f.s.PreviewInitialAudit(f.t.Context(), other.ID, "api", nil)
			require.NoError(f.t, err)
			_, err = f.s.EnableInitialAudit(f.t.Context(), plan)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"batch pairing rename", func(f fixture) {
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			n := requireNodeByID(f.t, f.s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
			_, _, err = f.s.Move(f.t.Context(), n.ID, f.s.RootID(), "missing.cr2.xmp", n.Revision)
			require.NoError(f.t, err)
			_, err = f.s.BatchMove(f.t.Context(), []BatchMoveRequest{{NodeID: n.ID, IfRevision: requireNodeByID(f.t, f.s, n.ID).Revision, DestinationPath: "/capture.cr2.xmp"}})
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"asset display revision", func(f fixture) {
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			_, err = f.s.SetPhotoDisplay(f.t.Context(), asset.ID, asset.Revision, &f.raw.ID)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"original rename", func(f fixture) {
			n := requireNodeByID(f.t, f.s, f.raw.NodeID)
			_, _, err := f.s.Move(f.t.Context(), n.ID, f.s.RootID(), "renamed.cr2", n.Revision)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"original current version", func(f fixture) {
			n := requireNodeByID(f.t, f.s, f.raw.NodeID)
			_, _, err := f.s.ReplaceContent(f.t.Context(), n.ID, n.Revision, fakeHash("a2"), 5, "application/octet-stream")
			require.NoError(f.t, err)
			publishXMPOrientation(f.t, f.s, n.ID, fakeHash("ee"))
		}, PhotoXMPStatePending},
		{"regeneration with same orientation", func(f fixture) {
			publishXMPOrientation(f.t, f.s, f.raw.NodeID, fakeHash("ff"))
		}, PhotoXMPStateSynced},
		{"source orientation", func(f fixture) {
			n := requireNodeByID(f.t, f.s, f.raw.NodeID)
			_, err := f.s.PublishSourceMetadata(f.t.Context(), n.BlobHash, fakeHash("ff"), photoCanonical(f.t, photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(6))))
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"paired member authored revision", func(f fixture) {
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			jpeg := fileByRole(asset.Files, PhotoRoleImage)
			_, err = editPhotoAuthored(f.t.Context(), f.s, []PhotoAuthoredTarget{{FileID: jpeg.ID, Revision: jpeg.Revision, Patch: PhotoAuthoredPatch{Rating: new(3)}}})
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"sidecar rename", func(f fixture) {
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			n := requireNodeByID(f.t, f.s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
			_, _, err = f.s.Move(f.t.Context(), n.ID, f.s.RootID(), "renamed.xmp", n.Revision)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"sidecar current version", func(f fixture) {
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			n := requireNodeByID(f.t, f.s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
			_, _, err = f.s.ReplaceContent(f.t.Context(), n.ID, n.Revision, fakeHash("c4"), 5, "application/rdf+xml")
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"sibling xmp created", func(f fixture) {
			_, err := f.s.CreateFile(f.t.Context(), f.s.RootID(), "capture.cr2.xmp", fakeHash("e5"), 4, "application/rdf+xml")
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"sibling xmp trashed", func(f fixture) {
			n, err := f.s.CreateFile(f.t.Context(), f.s.RootID(), "capture.cr2.xmp", fakeHash("e5"), 4, "application/rdf+xml")
			require.NoError(f.t, err)
			asset, err := f.s.PhotoAssetByID(f.t.Context(), f.raw.AssetID)
			require.NoError(f.t, err)
			sidecar := requireNodeByID(f.t, f.s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
			_, _, err = f.s.ReplaceContent(f.t.Context(), sidecar.ID, sidecar.Revision, fakeHash("d4"), 50, "application/rdf+xml")
			require.NoError(f.t, err)
			importXMP(f.t, f.s, f.raw.ID, PhotoAuthored{Rating: 2, Confirmed: PhotoConfirmedRating}, "Travel")
			_, _, err = f.s.Trash(f.t.Context(), n.ID, n.Revision)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"own keyword", func(f fixture) {
			tag, err := f.s.CreateTag(f.t.Context(), "Harbor")
			require.NoError(f.t, err)
			_, err = f.s.AssignTag(f.t.Context(), tag.ID, f.raw.NodeID, requireNodeByID(f.t, f.s, f.raw.NodeID).Revision)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"keyword renamed", func(f fixture) {
			_, err := f.s.RenameTag(f.t.Context(), f.tag.ID, f.tag.Revision, "Journey")
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"keyword removed", func(f fixture) {
			_, err := f.s.UnassignTag(f.t.Context(), f.tag.ID, f.raw.NodeID, requireNodeByID(f.t, f.s, f.raw.NodeID).Revision)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"keyword deleted", func(f fixture) {
			_, err := f.s.DeleteTag(f.t.Context(), f.tag.ID, f.tag.Revision)
			require.NoError(f.t, err)
		}, PhotoXMPStatePending},
		{"shared keyword assigned elsewhere", func(f fixture) {
			other, err := f.s.CreateFile(f.t.Context(), f.s.RootID(), "other.jpg", fakeHash("e5"), 4, "image/jpeg")
			require.NoError(f.t, err)
			_, err = f.s.AssignTag(f.t.Context(), f.tag.ID, other.ID, other.Revision)
			require.NoError(f.t, err)
			tag, err := f.s.TagByID(f.t.Context(), f.tag.ID)
			require.NoError(f.t, err)
			require.Greater(f.t, tag.Revision, f.tag.Revision, "the shared tag revision advanced")
		}, PhotoXMPStateSynced},
		{"unrelated audited content replacement", func(f fixture) {
			other, err := f.s.CreateFile(f.t.Context(), f.s.RootID(), "unrelated.txt", fakeHash("e5"), 4, "text/plain")
			require.NoError(f.t, err)
			seedInitialAuditAuthority(f.t, f.s, f.s.RootID())
			importXMP(f.t, f.s, f.raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating}, "Travel")
			_, _, err = f.s.ReplaceContent(f.t.Context(), other.ID, other.Revision, fakeHash("e6"), 5, "text/plain")
			require.NoError(f.t, err)
		}, PhotoXMPStateSynced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, raw := xmpSidecarPhoto(t)
			importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating}, "Travel")
			tag, err := s.TagByName(t.Context(), "Travel")
			require.NoError(t, err)
			raw, err = photoFileByIDQuery(t.Context(), s.db, raw.ID)
			require.NoError(t, err)
			work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
			require.NoError(t, err)
			tc.mutate(fixture{t, s, raw, tag})
			if tc.before == PhotoXMPStatePending {
				_, err := s.PublishPhotoXMP(t.Context(), PhotoXMPPlan{Work: work, Refusal: "stale worker result"}, "", 0, BlobPhysical{})
				require.ErrorIs(t, err, ErrStaleRevision)
				require.Contains(t, xmpTargets(t, s), PhotoSidecarTarget{FileID: raw.ID, NodeID: raw.NodeID, AssetID: raw.AssetID})
			} else {
				require.NotContains(t, xmpTargets(t, s), PhotoSidecarTarget{FileID: raw.ID, NodeID: raw.NodeID, AssetID: raw.AssetID})
			}
			assert.Equal(t, tc.before, xmpState(t, s, raw.ID).State)
		})
	}
}

// A newer build may leave a receipt this reader cannot interpret.
func TestPhotoXMPNewerReceiptStatusAndPruneRefusal(t *testing.T) {
	t.Parallel()
	s, _, raw := xmpSidecarPhoto(t)
	importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	sc := work.Sidecar.Node
	asset, err := s.PhotoAssetByID(t.Context(), raw.AssetID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, work.Sidecar.FileID, PhotoDetachOptions{})
	require.NoError(t, err)
	changed, _, err := s.ReplaceContent(t.Context(), sc.ID, sc.Revision, fakeHash("e5"), 50, "application/rdf+xml")
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE photo_change_receipts SET after_json=json_set(after_json,'$.version',2) WHERE operation='xmp_sync'`)
	require.NoError(t, err)
	assert.Equal(t, PhotoXMPStateUnavailable, xmpState(t, s, raw.ID).State)
	publishXMPOrientation(t, s, raw.NodeID, fakeHash("ff"))
	assert.Equal(t, PhotoXMPStateUnavailable, xmpState(t, s, raw.ID).State)
	for range 2 {
		state := settleXMP(t, s, raw.ID)
		assert.Equal(t, PhotoXMPStateUnavailable, state.State)
		assert.Contains(t, state.Reason, "could not be read")
	}
	_, err = s.PruneContentVersions(t.Context(), sc.ID, changed.Revision, VersionPruneSelector{AllPrior: true}, true)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.ErrorContains(t, err, "unsupported XMP receipt version 2")
	require.Equal(t, changed, requireNodeByID(t, s, sc.ID), "refused pruning must leave the node unchanged")
}

func TestPhotoXMPInputEligibilityMatchesTargetRoles(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, asset, _, _ := authoredSidecar(t)
	video, err := s.CreateFile(ctx, s.RootID(), "capture.mov", fakeHash("d4"), 4, "video/quicktime")
	require.NoError(t, err)
	videoAsset, err := s.PhotoAssetForNode(ctx, video.ID)
	require.NoError(t, err)
	asset.Files = append(asset.Files, videoAsset.Files...)
	tag, err := s.CreateTag(ctx, "Portfolio")
	require.NoError(t, err)
	for _, role := range []string{PhotoRoleRAW, PhotoRoleImage, PhotoRoleVideo, PhotoRoleSidecar} {
		t.Run(role, func(t *testing.T) {
			file := fileByRole(asset.Files, role)
			require.NotEmpty(t, file.ID)
			node := requireNodeByID(t, s, file.NodeID)
			_, err := s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
			var eligible bool
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT eligible FROM photo_xmp_inputs WHERE file_id=?`, file.ID).Scan(&eligible))
			assert.Equal(t, photoSidecarTargetRole(role), eligible)
		})
	}
}

func TestPhotoXMPQueryFilters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, asset, raw := xmpSidecarPhoto(t)
	match := func(text string) []string {
		t.Helper()
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: text, Sort: query.Sort{Field: "name", Direction: "asc"}}}, nil)
		require.NoError(t, err)
		ids := []string{}
		for _, item := range page.Items {
			ids = append(ids, item.AssetID)
		}
		return ids
	}
	assert.Equal(t, []string{asset.ID}, match("xmp:PENDING"))
	assert.Empty(t, match("xmp:CONFLICT"))
	n, err := s.CreateFile(ctx, s.RootID(), "settled.jpg", fakeHash("a2"), 4, "image/jpeg")
	require.NoError(t, err)
	settled, err := s.PhotoAssetForNode(ctx, n.ID)
	require.NoError(t, err)
	// A photo with no sidecar or decisions settles as None and is never queued.
	publishXMPOrientation(t, s, n.ID, fakeHash("ee"))
	require.Equal(t, PhotoXMPStateNone, settleXMP(t, s, settled.Files[0].ID).State)
	require.NotContains(t, xmpTargets(t, s), PhotoSidecarTarget{FileID: settled.Files[0].ID, NodeID: n.ID, AssetID: settled.ID})
	for text, want := range map[string][]string{
		"xmp:pending AND NOT name:settled.jpg": {asset.ID},
		"xmp:pending OR xmp:conflict":          {asset.ID},
		"xmp:pending OR name:settled.jpg":      {asset.ID, settled.ID},
		"NOT xmp:pending":                      {settled.ID},
	} {
		assert.ElementsMatch(t, want, match(text), text)
	}
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: raw.Revision, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	work, run, err := s.ClassifyPhotoXMP(ctx, raw.ID)
	require.NoError(t, err)
	require.True(t, run)
	plan, _ := planXMP(work, PhotoXMPPacket{Rating: 4, Present: PhotoConfirmedRating, Keywords: []string{}}, "")
	require.Equal(t, PhotoConfirmedRating, plan.Conflicts)
	_, err = s.PublishPhotoXMP(ctx, plan, "", 0, BlobPhysical{})
	require.NoError(t, err)
	assert.Equal(t, PhotoXMPStateConflict, xmpState(t, s, raw.ID).State)
	assert.Equal(t, []string{asset.ID}, match("xmp:CONFLICT"))
	assert.Empty(t, match("xmp:PENDING"))

	// A catalog edit makes the original Pending while retaining its conflict.
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: work.Original.Revision, Patch: PhotoAuthoredPatch{Rating: new(2)}}})
	require.NoError(t, err)
	assert.Equal(t, []string{asset.ID}, match("xmp:CONFLICT"))
	assert.Equal(t, []string{asset.ID}, match("xmp:PENDING"))
}

// restoredXMPStore backs up a store's metadata and restores it into a new one.
func restoredXMPStore(t *testing.T, s *Store) *Store {
	t.Helper()
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(backup.Bytes())))
	return restored
}

func syncedPhoto(t *testing.T) (*Store, PhotoFile) {
	t.Helper()
	s, _, raw := xmpSidecarPhoto(t)
	importXMP(t, s, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
	return s, raw
}

// A backup restores the receipts, and the work they imply, exactly.
func TestPhotoXMPBackupRoundTrip(t *testing.T) {
	t.Parallel()
	t.Run("a conflict stays actionable", func(t *testing.T) {
		ctx := t.Context()
		s, raw := syncedPhoto(t)
		current, err := photoFileByIDQuery(ctx, s.db, raw.ID)
		require.NoError(t, err)
		_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
		require.NoError(t, err)
		prior, _, err := s.ClassifyPhotoXMP(ctx, raw.ID)
		require.NoError(t, err)
		_, _, err = s.ReplaceContent(ctx, prior.Sidecar.Node.ID, prior.Sidecar.Node.Revision, fakeHash("d4"), 50, "application/rdf+xml")
		require.NoError(t, err)
		work, run, err := s.ClassifyPhotoXMP(ctx, raw.ID)
		require.NoError(t, err)
		require.True(t, run)
		packet := PhotoXMPPacket{Rating: 4, Present: PhotoConfirmedRating, Keywords: []string{}}
		plan, _ := planXMP(work, packet, "")
		require.Equal(t, PhotoConfirmedRating, plan.Conflicts)
		_, err = s.PublishPhotoXMP(ctx, plan, "", 0, BlobPhysical{})
		require.NoError(t, err)
		before := xmpState(t, s, raw.ID)
		require.Equal(t, PhotoXMPStateConflict, before.State)
		require.Equal(t, []PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, before.Rows)
		require.NotEmpty(t, before.ConflictID)
		restored := restoredXMPStore(t, s)
		after := xmpState(t, restored, raw.ID)
		require.Equal(t, before, after)
		work, err = restored.PhotoXMPConflict(ctx, raw.ID, after.ConflictID)
		require.NoError(t, err)
		choice, _ := planXMP(work, packet, "sidecar")
		imported, err := restored.PublishPhotoXMP(ctx, choice, "", 0, BlobPhysical{})
		require.NoError(t, err)
		require.NotNil(t, imported)
		require.Equal(t, PhotoXMPStateSynced, xmpState(t, restored, raw.ID).State)
		settled, err := photoFileByIDQuery(ctx, restored.db, raw.ID)
		require.NoError(t, err)
		require.Equal(t, 4, settled.Rating)
	})
	t.Run("pending orientation work is kept", func(t *testing.T) {
		s, raw := syncedPhoto(t)
		n := requireNodeByID(t, s, raw.NodeID)
		_, err := s.PublishSourceMetadata(t.Context(), n.BlobHash, fakeHash("ef"), photoCanonical(t, photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(6))))
		require.NoError(t, err)
		want := []PhotoSidecarTarget{{FileID: raw.ID, NodeID: raw.NodeID, AssetID: raw.AssetID}}
		restored := restoredXMPStore(t, s)
		require.Equal(t, PhotoXMPStatePending, xmpState(t, restored, raw.ID).State)
		require.Equal(t, want, xmpTargets(t, restored), "restoring technical projections must retain orientation work")
	})
	t.Run("a removed subtree keeps its snapshot", func(t *testing.T) {
		for _, test := range []struct {
			name, fingerprint string
			first, oversized  bool
		}{
			{"first", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true, false},
			{"synced", "366e3194627e3cafad1135c75f75e250ec76aca78f559644c61958aa049483ac", false, false},
			{"oversized", "9643f163881e7f6f015da707df395a3d67f6819a52534f8f0ae9586884336146", false, true},
		} {
			t.Run(test.name, func(t *testing.T) {
				s, asset, raw := xmpSidecarPhoto(t)
				if !test.first {
					importXMP(t, s, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating}, "Travel")
				}
				if test.oversized {
					tag, err := s.CreateTag(t.Context(), strings.Repeat("x", maxPhotoExportKeywordBytes+1))
					require.NoError(t, err)
					_, err = s.AssignTag(t.Context(), tag.ID, raw.NodeID, requireNodeByID(t, s, raw.NodeID).Revision)
					require.NoError(t, err)
				}
				before, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				current, err := photoFileByIDQuery(t.Context(), s.db, raw.ID)
				require.NoError(t, err)
				_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
				require.NoError(t, err)
				folder, err := s.Mkdir(t.Context(), s.RootID(), "sidecars")
				require.NoError(t, err)
				sidecar := requireNodeByID(t, s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
				_, _, err = s.Move(t.Context(), sidecar.ID, folder.ID, sidecar.Name, sidecar.Revision)
				require.NoError(t, err)
				_, _, err = s.Trash(t.Context(), folder.ID, UnconditionalRev)
				require.NoError(t, err)
				work, run, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				require.False(t, run)
				require.NotNil(t, work.Removal)
				require.Equal(t, 5, work.Removal.Catalog.Rating)
				require.Equal(t, test.fingerprint, work.Removal.KeywordFingerprint)
				require.Equal(t, before.History, work.History)
				restored := restoredXMPStore(t, s)
				roundTrip, run, err := restored.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				require.False(t, run)
				require.Equal(t, work.Removal, roundTrip.Removal)
				require.Equal(t, work.History, roundTrip.History)
				_, _, err = s.Restore(t.Context(), folder.ID, UnconditionalRev)
				require.NoError(t, err)
				resumed, run, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				require.Equal(t, !test.oversized, run)
				require.NotNil(t, resumed.Sidecar)
				require.Equal(t, before.History, resumed.History)
			})
		}
	})
	t.Run("source paths survive a portable restore", func(t *testing.T) {
		for _, tc := range []struct{ source, key string }{
			{"/camera/cafe\u0301.xmp", "/camera/cafe\u0301.xmp"},
			{`C:\camera\capture.xmp`, `C:/camera/capture.xmp`},
			{`c:\camera\capture.xmp`, `C:/camera/capture.xmp`},
			{`\\server\share\capture.xmp`, `//server/share/capture.xmp`},
			{`/camera/literal\capture.xmp`, `/camera/literal\capture.xmp`},
		} {
			t.Run(tc.source, func(t *testing.T) {
				s, _, raw := xmpSidecarPhoto(t)
				work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				sc := work.Sidecar.Node
				run, err := s.BeginIngest(t.Context(), "photo-import", "portable source")
				require.NoError(t, err)
				_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", tc.source, "")
				require.NoError(t, err)
				rawNode, err := s.NodeByID(t.Context(), raw.NodeID)
				require.NoError(t, err)
				_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), rawNode.Name, rawNode.BlobHash, rawNode.Size, "application/octet-stream", strings.TrimSuffix(tc.source, ".xmp")+".cr2", "")
				require.NoError(t, err)

				importXMP(t, s, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
				work, _, err = s.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				sources, err := photoXMPSourceVersionsTx(t.Context(), s.db, sc.ID)
				require.NoError(t, err)
				require.Len(t, sources, 1)
				require.Equal(t, tc.key, sources[0].Path)
				require.Equal(t, sc.BlobHash, sources[0].Digest)
				restored := restoredXMPStore(t, s)
				if strings.HasPrefix(tc.key, "C:/") {
					alias := "c" + tc.source[1:]
					if tc.source[0] == 'c' {
						alias = "C" + tc.source[1:]
					}
					require.NoError(t, restored.withStorageTx(t.Context(), func(tx *sql.Tx) error {
						unchanged, _, err := photoXMPSourceUnchangedTx(t.Context(), tx, sc, PhotoImportMember{OriginalPath: alias, BlobHash: sc.BlobHash})
						require.True(t, unchanged, "drive-letter casing must reach the same persisted cursor")
						return err
					}))
				}

				_, _, err = restored.ReplaceContent(t.Context(), sc.ID, UnconditionalRev, fakeHash("d5"), 5, "application/rdf+xml")
				require.NoError(t, err)
				after, runSync, err := restored.ClassifyPhotoXMP(t.Context(), raw.ID)
				require.NoError(t, err)
				require.NoError(t, restored.CheckPhotoXMPPairing(t.Context(), after, ".CR2"))
				require.True(t, runSync)
				plan, mask := planXMP(after, work.History.Settled.Packet, "")
				require.Zero(t, mask)
				_, err = restored.PublishPhotoXMP(t.Context(), plan, "", 0, BlobPhysical{})
				require.NoError(t, err)
				restoredSources, err := photoXMPSourceVersionsTx(t.Context(), restored.db, sc.ID)
				require.NoError(t, err)
				require.Equal(t, sources, restoredSources)
				require.Equal(t, PhotoXMPStateSynced, xmpState(t, restored, raw.ID).State)
			})
		}
	})
}

// Pruning keeps each live source path's latest sidecar version, and a purge drops them.
func TestPhotoXMPSourceVersionRetention(t *testing.T) {
	t.Parallel()
	t.Run("each path keeps its latest version", func(t *testing.T) {
		s, _, raw := xmpSidecarPhoto(t)
		work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
		require.NoError(t, err)
		sc := work.Sidecar.Node
		run, err := s.BeginIngest(t.Context(), "photo-import", "source retention")
		require.NoError(t, err)
		_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", "/camera/capture.xmp", "")
		require.NoError(t, err)
		first := sc.CurrentVersionID
		sc, _, err = s.ReplaceContent(t.Context(), sc.ID, UnconditionalRev, fakeHash("d5"), 5, "application/rdf+xml")
		require.NoError(t, err)
		_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", "/backup/capture.xmp", "")
		require.NoError(t, err)
		second := sc.CurrentVersionID
		sc, _, err = s.ReplaceContent(t.Context(), sc.ID, UnconditionalRev, fakeHash("d6"), 6, "application/rdf+xml")
		require.NoError(t, err)
		pruned, err := s.PruneContentVersions(t.Context(), sc.ID, sc.Revision, VersionPruneSelector{AllPrior: true}, true)
		require.NoError(t, err)
		require.Len(t, pruned.DependencyRetained, 2)
		require.ElementsMatch(t, []string{first, second}, []string{pruned.DependencyRetained[0].ID, pruned.DependencyRetained[1].ID})
		restored := restoredXMPStore(t, s)
		sources, err := photoXMPSourceVersionsTx(t.Context(), restored.db, sc.ID)
		require.NoError(t, err)
		require.Len(t, sources, 2)
		_, err = restored.ContentVersionByID(t.Context(), first)
		require.NoError(t, err)
		_, err = restored.ContentVersionByID(t.Context(), second)
		require.NoError(t, err)
		// A fresh observation replaces only its own path's retained packet.
		run, err = s.BeginIngest(t.Context(), "photo-import", "new observation")
		require.NoError(t, err)
		_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", "/camera/capture.xmp", "")
		require.NoError(t, err)
		sc = requireNodeByID(t, s, sc.ID)
		_, err = s.PruneContentVersions(t.Context(), sc.ID, sc.Revision, VersionPruneSelector{AllPrior: true}, true)
		require.NoError(t, err)
		_, err = s.ContentVersionByID(t.Context(), first)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.ContentVersionByID(t.Context(), second)
		require.NoError(t, err)
		asset, err := s.PhotoAssetByID(t.Context(), raw.AssetID)
		require.NoError(t, err)
		_, err = s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, fileByRole(asset.Files, PhotoRoleSidecar).ID, PhotoDetachOptions{})
		require.NoError(t, err)
		_, _, err = s.Trash(t.Context(), sc.ID, UnconditionalRev)
		require.NoError(t, err)
		purged, err := s.TrashEmpty(t.Context(), 0, true)
		require.NoError(t, err)
		require.Equal(t, int64(1), purged.Deleted)
		_, err = s.ContentVersionByID(t.Context(), second)
		require.ErrorIs(t, err, ErrNotFound)
		sources, err = photoXMPSourceVersionsTx(t.Context(), s.db, sc.ID)
		require.NoError(t, err)
		require.Empty(t, sources)
	})

	t.Run("pruning keeps snapshot references", func(t *testing.T) {
		s, raw := syncedPhoto(t)
		work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
		require.NoError(t, err)
		sc := work.Sidecar.Node
		changed, _, err := s.ReplaceContent(t.Context(), sc.ID, sc.Revision, fakeHash("e5"), 50, "application/rdf+xml")
		require.NoError(t, err)
		reverted, version, _, err := s.RevertContent(t.Context(), sc.ID, changed.Revision, sc.CurrentVersionID)
		require.NoError(t, err)
		importXMP(t, s, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
		work, _, err = s.ClassifyPhotoXMP(t.Context(), raw.ID)
		require.NoError(t, err)
		result, err := s.PruneContentVersions(t.Context(), sc.ID, reverted.Revision, VersionPruneSelector{AllPrior: true}, true)
		require.NoError(t, err)
		_, err = s.ContentVersionByID(t.Context(), version.ID)
		require.NoError(t, err, "a saved settled version must remain resolvable")
		require.Contains(t, result.DependencyRetained, version)
		restored := restoredXMPStore(t, s)
		after, _, err := restored.ClassifyPhotoXMP(t.Context(), raw.ID)
		require.NoError(t, err)
		require.Equal(t, work.History.Settled, after.History.Settled)
	})
	t.Run("more sources than a photo has files", func(t *testing.T) {
		s, _, raw := xmpSidecarPhoto(t)
		work, _, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
		require.NoError(t, err)
		sc := work.Sidecar.Node
		run, err := s.BeginIngest(t.Context(), "photo-import", "many folders")
		require.NoError(t, err)
		var held []string
		for i := range PhotoMaxFiles + 44 {
			sc, _, err = s.ReplaceContent(t.Context(), sc.ID, UnconditionalRev, fmt.Sprintf("%064x", i+1000), 5, "application/rdf+xml")
			require.NoError(t, err)
			_, _, err = s.IngestFileWithMembership(t.Context(), run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", fmt.Sprintf("/camera/%03d/capture.xmp", i), "")
			require.NoError(t, err)
			held = append(held, sc.CurrentVersionID)
		}
		sources, err := photoXMPSourceVersionsTx(t.Context(), s.db, sc.ID)
		require.NoError(t, err)
		require.Len(t, sources, len(held), "every live source path is enumerated, however many")
		sc, _, err = s.ReplaceContent(t.Context(), sc.ID, UnconditionalRev, fakeHash("d5"), 5, "application/rdf+xml")
		require.NoError(t, err)
		pruned, err := s.PruneContentVersions(t.Context(), sc.ID, sc.Revision, VersionPruneSelector{AllPrior: true}, true)
		require.NoError(t, err)
		retained := []string{}
		for _, version := range pruned.DependencyRetained {
			retained = append(retained, version.ID)
		}
		require.ElementsMatch(t, held, retained, "each path's latest version stays for its live source")
	})
}

func TestPhotoXMPSidecarSelectionSurvivesRestore(t *testing.T) {
	t.Parallel()
	s, asset, raw := xmpSidecarPhoto(t)
	lower := requireNodeByID(t, s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
	importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	higher, err := s.CreateFile(t.Context(), s.RootID(), "capture.cr2.xmp", fakeHash("e5"), 4, "application/rdf+xml")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, higher.ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	work, run, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	require.False(t, run)
	require.NotNil(t, work.Sidecar)
	assert.Equal(t, lower.ID, work.Sidecar.Node.ID)
	_, _, err = s.Trash(t.Context(), lower.ID, lower.Revision)
	require.NoError(t, err)
	work, run, err = s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	require.True(t, run)
	require.NotNil(t, work.Sidecar)
	assert.Equal(t, higher.ID, work.Sidecar.Node.ID)
	plan, mask := planXMP(work, PhotoXMPPacket{Rating: 4, Present: PhotoConfirmedRating, Keywords: []string{}}, "")
	require.Zero(t, mask)
	_, err = s.PublishPhotoXMP(t.Context(), plan, "", 0, BlobPhysical{})
	require.NoError(t, err)
	_, _, err = s.Restore(t.Context(), lower.ID, UnconditionalRev)
	require.NoError(t, err)
	work, _, err = s.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	require.NotNil(t, work.Sidecar)
	assert.Equal(t, higher.ID, work.Sidecar.Node.ID, "the selected live sidecar stays selected after restoration")
}

func TestPhotoXMPMetadataRejectsDanglingReceipt(t *testing.T) {
	t.Parallel()
	s, _, raw := xmpSidecarPhoto(t)
	importXMP(t, s, raw.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &backup))
	version := xmpReceipt(t, s, raw.ID).Sync.Settled.VersionID
	data := bytes.ReplaceAll(backup.Bytes(), []byte(`\"version_id\":\"`+version+`\"`), []byte(`\"version_id\":\"11111111-1111-4111-8111-111111111111\"`))
	require.ErrorContains(t, newTestStore(t).ImportMetadata(t.Context(), bytes.NewReader(data)), "XMP receipts")
}

func TestPhotoXMPReceiptLimit(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"refusals and overflow", "trash", "detach"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, raw := xmpSidecarPhoto(t)
			ctx := t.Context()
			text := strings.Repeat("x", MaxPhotoAuthoredTextBytes)
			values := PhotoAuthored{Rating: 3, Caption: text, Creator: text, Copyright: text, Confirmed: PhotoConfirmedRating | PhotoConfirmedCaption | PhotoConfirmedCreator | PhotoConfirmedCopyright}
			importXMP(t, s, raw.ID, values)
			packet := PhotoXMPPacket{PhotoAuthored: values, Present: values.Confirmed, Keywords: []string{}}
			packet.Confirmed = 0
			work, _, err := s.ClassifyPhotoXMP(ctx, raw.ID)
			require.NoError(t, err)
			sc := work.Sidecar.Node
			run, err := s.BeginIngest(ctx, "photo-import", "many folders")
			require.NoError(t, err)
			var paths []string
			for i := range 6 {
				path := fmt.Sprintf("/folder/%02d/capture.xmp", i)
				paths = append(paths, path)
				_, _, err = s.IngestFileWithMembership(ctx, run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", path, "")
				require.NoError(t, err)
			}
			work, _, err = s.ClassifyPhotoXMP(ctx, raw.ID)
			require.NoError(t, err)
			steps := work.Steps()
			for i := range steps {
				steps[i].Packet = packet
			}
			plan, mask := PlanPhotoXMP(work, steps, "")
			require.Zero(t, mask)
			require.Zero(t, plan.Conflicts)
			_, err = s.PublishPhotoXMP(ctx, plan, "", 0, BlobPhysical{})
			require.NoError(t, err)
			require.Len(t, xmpReceipt(t, s, raw.ID).Sync.Outside, len(paths))
			if scenario != "refusals and overflow" {
				asset, err := s.PhotoAssetByID(ctx, raw.AssetID)
				require.NoError(t, err)
				sidecar := fileByRole(asset.Files, PhotoRoleSidecar)
				node := requireNodeByID(t, s, sidecar.NodeID)
				if scenario == "trash" {
					_, _, err = s.Trash(ctx, node.ID, node.Revision)
				} else {
					asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoDetachOptions{})
				}
				require.NoError(t, err)
				receipt := xmpReceipt(t, s, raw.ID)
				require.NotNil(t, receipt.Removed)
				require.Len(t, receipt.Sync.Outside, len(paths), "no source history is given up for the removal record")
				require.NotEmpty(t, receipt.Sync.Settled.Digest)
				if scenario == "trash" {
					return
				}
				// Pruning a detached sidecar's prior versions keeps the settled one its history compares with.
				changed, _, err := s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("e5"), 50, "application/rdf+xml")
				require.NoError(t, err)
				pruned, err := s.PruneContentVersions(ctx, node.ID, changed.Revision, VersionPruneSelector{AllPrior: true}, true)
				require.NoError(t, err)
				settled, err := s.ContentVersionByID(ctx, node.CurrentVersionID)
				require.NoError(t, err)
				require.Contains(t, pruned.DependencyRetained, settled)
				_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.NodeID, PhotoRoleSidecar, &raw.ID)
				require.NoError(t, err)
				work, _, err = s.ClassifyPhotoXMP(ctx, raw.ID)
				require.NoError(t, err)
				require.NotNil(t, work.History)
				require.Len(t, work.History.Outside, len(paths), "reattaching the same sidecar finds every source it compared")
				return
			}
			editRating := func(rating int) PhotoXMPWork {
				current, err := photoFileByIDQuery(ctx, s.db, raw.ID)
				require.NoError(t, err)
				_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: current.Revision, Patch: PhotoAuthoredPatch{Rating: new(rating)}}})
				require.NoError(t, err)
				work, pending, err := s.ClassifyPhotoXMP(ctx, raw.ID)
				require.NoError(t, err)
				require.True(t, pending)
				return work
			}
			// A refusal still fits beside that history.
			_, _, err = s.ReplaceContent(ctx, sc.ID, UnconditionalRev, fakeHash("e6"), 8, "application/rdf+xml")
			require.NoError(t, err)
			work = editRating(5)
			_, err = s.PublishPhotoXMP(ctx, PhotoXMPPlan{Work: work, Refusal: "The sidecar is malformed. " + strings.Repeat("detail ", 500)}, "", 0, BlobPhysical{})
			require.NoError(t, err)
			state := xmpState(t, s, raw.ID)
			require.Equal(t, PhotoXMPStateInvalid, state.State)
			require.Contains(t, state.Reason, "The sidecar is malformed.")
			// Sources that overflow the receipt make the packet Invalid, and a different sidecar file starts over.
			_, _, err = s.ReplaceContent(ctx, sc.ID, UnconditionalRev, sc.BlobHash, sc.Size, "application/rdf+xml")
			require.NoError(t, err)
			editRating(3)
			_, _, err = s.IngestFileWithMembership(ctx, run, s.RootID(), sc.Name, sc.BlobHash, sc.Size, "application/rdf+xml", "/folder/overflow/capture.xmp", "")
			require.NoError(t, err)
			work, _, err = s.ClassifyPhotoXMP(ctx, raw.ID)
			require.NoError(t, err)
			steps = work.Steps()
			for i := range steps {
				steps[i].Packet = packet
			}
			plan, mask = PlanPhotoXMP(work, steps, "")
			require.Zero(t, mask)
			require.Zero(t, plan.Conflicts)
			_, err = s.PublishPhotoXMP(ctx, plan, "", 0, BlobPhysical{})
			require.NoError(t, err)
			state = xmpState(t, s, raw.ID)
			require.Equal(t, PhotoXMPStateInvalid, state.State)
			require.Contains(t, state.Reason, "Detach this sidecar and attach a different XMP file")
			asset, err := s.PhotoAssetByID(ctx, raw.AssetID)
			require.NoError(t, err)
			asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, fileByRole(asset.Files, PhotoRoleSidecar).ID, PhotoDetachOptions{})
			require.NoError(t, err)
			replacement, err := s.CreateFile(ctx, s.RootID(), "capture.cr2.xmp", fakeHash("e5"), 4, "application/rdf+xml")
			require.NoError(t, err)
			_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, replacement.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
			work, pending, err := s.ClassifyPhotoXMP(ctx, raw.ID)
			require.NoError(t, err)
			require.True(t, pending)
			require.Nil(t, work.History)
			plan, _ = planXMP(work, packet, "")
			_, err = s.PublishPhotoXMP(ctx, plan, "", 0, BlobPhysical{})
			require.NoError(t, err)
			require.Equal(t, PhotoXMPStateSynced, xmpState(t, s, raw.ID).State)
		})
	}
	t.Run("the removal reserve holds legacy control characters", func(t *testing.T) {
		control := strings.Repeat("\x01", MaxPhotoAuthoredTextBytes)
		data, err := json.Marshal(PhotoXMPRemoval{Catalog: PhotoAuthored{Caption: control, Creator: control, Copyright: control, Rating: 5, Rotation: 270, Confirmed: PhotoConfirmedAll}, KeywordFingerprint: strings.Repeat("f", 64)}, json.Deterministic(true))
		require.NoError(t, err)
		require.LessOrEqual(t, len(data), photoXMPRemovalReserve)
	})
}

// xmpReceipt reads an original's current sync receipt.
func xmpReceipt(t *testing.T, s *Store, fileID string) photoXMPReceipt {
	t.Helper()
	var data []byte
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT after_json FROM photo_change_receipts WHERE operation='xmp_sync' AND CAST(json_extract(after_json,'$.file_id') AS TEXT)=?`, fileID).Scan(&data))
	receipt, err := decodePhotoXMPReceipt(data)
	require.NoError(t, err, "the receipt must stay readable")
	return receipt
}

func TestPhotoXMPAuditQueueEligibility(t *testing.T) {
	t.Parallel()
	s, asset, raw := xmpSidecarPhoto(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synced.jpg", fakeHash("f6"), 4, "image/jpeg")
	require.NoError(t, err)
	other, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	jpeg := other.Files[0]
	sidecar, err := s.CreateFile(t.Context(), s.RootID(), "synced.jpg.xmp", fakeHash("a7"), 4, "application/rdf+xml")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(t.Context(), other.ID, other.Revision, sidecar.ID, PhotoRoleSidecar, &jpeg.ID)
	require.NoError(t, err)
	importXMP(t, s, jpeg.ID, PhotoAuthored{Rating: 3, Confirmed: PhotoConfirmedRating})
	importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	trashed, err := s.CreateFile(t.Context(), s.RootID(), "trashed.jpg", fakeHash("d4"), 4, "image/jpeg")
	require.NoError(t, err)
	trashedAsset, err := s.PhotoAssetForNode(t.Context(), trashed.ID)
	require.NoError(t, err)
	_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: trashedAsset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(3)}}})
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), trashed.ID, UnconditionalRev)
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "untouched.jpg", fakeHash("e5"), 4, "image/jpeg")
	require.NoError(t, err)
	require.Empty(t, xmpTargets(t, s))
	seedInitialAuditAuthority(t, s, s.RootID())
	var queued []string
	require.NoError(t, queryRows(t.Context(), s.db, "SELECT asset_id FROM photo_xmp_unsettled", nil, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		queued = append(queued, id)
		return nil
	}))
	require.ElementsMatch(t, []string{asset.ID, other.ID}, queued)
	targets := xmpTargets(t, s)
	require.Len(t, targets, 2)
	require.ElementsMatch(t, []string{raw.ID, jpeg.ID}, []string{targets[0].FileID, targets[1].FileID})
}

func TestPhotoXMPAuditClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, state, reason string
	}{
		{"photo outside scope", PhotoXMPStateUnavailable, "The photo is outside the active audit scope."},
		{"creation blocked", PhotoXMPStateUnavailable, "Audit authority blocks creating a sidecar attachment."},
		{"sidecar outside scope", PhotoXMPStateUnavailable, "The sidecar is outside the active audit scope."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, asset, raw := xmpSidecarPhoto(t)
			importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
			scope, err := s.Mkdir(t.Context(), s.RootID(), "Audited")
			require.NoError(t, err)
			if tc.name != "photo outside scope" {
				for _, file := range asset.Files {
					if file.Role == PhotoRoleSidecar {
						continue
					}
					n := requireNodeByID(t, s, file.NodeID)
					_, _, err = s.Move(t.Context(), n.ID, scope.ID, n.Name, n.Revision)
					require.NoError(t, err)
				}
			}
			if tc.name == "creation blocked" {
				n := requireNodeByID(t, s, fileByRole(asset.Files, PhotoRoleSidecar).NodeID)
				_, _, err = s.Trash(t.Context(), n.ID, n.Revision)
				require.NoError(t, err)
				// A new decision reopens sync after explicit sidecar removal.
				_, err = s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}}), Patch: PhotoAuthoredPatch{Rating: new(5)}})
				require.NoError(t, err)
			}
			seedInitialAuditAuthority(t, s, scope.ID)
			_, run, err := s.ClassifyPhotoXMP(t.Context(), raw.ID)
			require.NoError(t, err)
			require.Equal(t, tc.state == PhotoXMPStatePending, run)
			state := xmpState(t, s, raw.ID)
			assert.Equal(t, tc.state, state.State)
			assert.Equal(t, tc.reason, state.Reason)
		})
	}
}

func TestPhotoXMPQueryFiltersWithStacks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	_, err := s.EditPhotoAuthoredBatch(t.Context(), PhotoAuthoredBatch{
		Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids}),
		Patch:   PhotoAuthoredPatch{Rating: new(5)},
	})
	require.NoError(t, err)
	request := captureRequest(t, "asc")
	request.Query.Syntax = "advanced"
	request.Query.Text = "xmp:pending"
	page, err := s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, ids, rowIDs(page.Items))
	require.Len(t, page.Stacks, 1)
	require.Equal(t, ids, page.Stacks[0].AssetIDs)
	for _, id := range ids[:2] {
		asset, err := s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		file := asset.Files[0]
		node := requireNodeByID(t, s, file.NodeID)
		sidecar, err := s.CreateFile(t.Context(), s.RootID(), node.Name+".xmp", browseHash(node.Name+".xmp"), 4, "application/rdf+xml")
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &file.ID)
		require.NoError(t, err)
		work, run, err := s.ClassifyPhotoXMP(t.Context(), file.ID)
		require.NoError(t, err)
		require.True(t, run)
		plan, _ := planXMP(work, PhotoXMPPacket{Rating: 4, Present: PhotoConfirmedRating, Keywords: []string{}}, "")
		require.Equal(t, PhotoConfirmedRating, plan.Conflicts)
		_, err = s.PublishPhotoXMP(t.Context(), plan, "", 0, BlobPhysical{})
		require.NoError(t, err)
	}
	request.Query.Text = "xmp:conflict"
	page, err = s.ListPhotoAssets(t.Context(), request, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, ids[:2], rowIDs(page.Items))
	require.Len(t, page.Stacks, 1)
	require.Equal(t, ids[:2], page.Stacks[0].AssetIDs)
}
