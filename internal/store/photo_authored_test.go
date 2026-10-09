package store

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

func authoredPair(t *testing.T, s *Store) PhotoAsset {
	t.Helper()
	ctx := t.Context()
	raw, err := s.CreateFile(ctx, s.RootID(), "capture.cr2", fakeHash("a1"), 4, "application/octet-stream")
	require.NoError(t, err)
	jpg, err := s.CreateFile(ctx, s.RootID(), "capture.jpg", fakeHash("b2"), 4, "image/jpeg")
	require.NoError(t, err)
	owned, err := s.PhotoAssetForNode(ctx, jpg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, owned.ID, owned.Revision, owned.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	return asset
}

func TestPhotoAuthoredPairAtomicUndoAndRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	jpg := fileByRole(asset.Files, PhotoRoleImage)
	assert.Equal(t, int64(1), raw.Revision)
	assert.Equal(t, PhotoAuthored{}, raw.Authored())
	targets := []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5), Flag: new("pick"), Caption: new("River"), Rotation: new(90)}}, {jpg.ID, 1, PhotoAuthoredPatch{Rating: new(3), Label: new("red")}}}
	receipt, err := s.EditPhotoPair(ctx, asset.ID, asset.Revision, targets)
	require.NoError(t, err)
	require.Len(t, receipt.After, 2)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.False(t, asset.Agreement["rating"])
	assert.True(t, asset.Agreement["creator"])
	_, err = s.EditPhotoAuthored(ctx, targets)
	require.ErrorIs(t, err, ErrStaleRevision)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	undo, err := restored.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, receipt.ReceiptID, undo.UndoOf)
	for _, after := range undo.After {
		assert.Equal(t, int64(3), after.Revision)
		assert.Equal(t, PhotoAuthored{}, after.Values)
	}
	_, err = restored.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	receipt, err = s.EditPhotoPair(ctx, asset.ID, asset.Revision, []PhotoAuthoredTarget{{raw.ID, 2, PhotoAuthoredPatch{Rating: new(5)}}, {jpg.ID, 2, PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	require.Len(t, receipt.After, 2)
	for _, after := range receipt.After {
		assert.Equal(t, int64(3), after.Revision)
		assert.Equal(t, 5, after.Values.Rating)
	}
}

func TestPhotoAuthoredRollbackAndMembershipFence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	jpg := fileByRole(asset.Files, PhotoRoleImage)
	_, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}, {jpg.ID, 1, PhotoAuthoredPatch{Rating: new(6)}}})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	rawNow, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Equal(t, raw, rawNow)
	_, err = s.EditPhotoPair(ctx, asset.ID, asset.Revision-1, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}})
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.EditPhotoPair(ctx, asset.ID, asset.Revision, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}})
	require.ErrorIs(t, err, ErrStaleRevision)
	receipt, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, PhotoDetachOptions{})
	require.NoError(t, err)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoAuthoredCompleteManyFileReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	targets := []PhotoAuthoredTarget{}
	for i := range 40 {
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("photo-%d.jpg", i), fakeHash("b2"), 4, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		f := asset.Files[0]
		targets = append(targets, PhotoAuthoredTarget{f.ID, 1, PhotoAuthoredPatch{Rating: new(4)}})
	}
	receipt, err := s.EditPhotoAuthored(ctx, targets)
	require.NoError(t, err)
	require.Len(t, receipt.After, 40)
	stored, err := loadPhotoAuthoredReceipt(ctx, s.db, receipt.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, receipt, stored)
	undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	assert.Len(t, undo.After, 40)
	text := strings.Repeat("x", MaxPhotoAuthoredTextBytes)
	for i := range targets {
		targets[i].Revision = 3
		targets[i].Patch = PhotoAuthoredPatch{Caption: &text}
	}
	large, err := s.EditPhotoAuthored(ctx, targets)
	require.NoError(t, err)
	for i := range targets {
		targets[i].Revision = 4
		targets[i].Patch = PhotoAuthoredPatch{Rating: new(3)}
	}
	_, err = s.EditPhotoAuthored(ctx, targets)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	current, err := photoFileByIDQuery(ctx, s.db, targets[0].FileID)
	require.NoError(t, err)
	assert.Equal(t, int64(4), current.Revision)
	_, err = s.EditPhotoAuthored(ctx, make([]PhotoAuthoredTarget, maxBatchTagTargets+1))
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	stored, err = loadPhotoAuthoredReceipt(ctx, s.db, large.ReceiptID)
	require.NoError(t, err)
	assert.Len(t, stored.After, 40)
	var afterJSON string
	require.NoError(t, s.db.QueryRow(`SELECT after_json FROM photo_change_receipts WHERE receipt_id=?`, large.ReceiptID).Scan(&afterJSON))
	assert.NotContains(t, afterJSON, `"before"`)
	largeUndo, err := s.UndoPhotoAuthored(ctx, large.ReceiptID)
	require.NoError(t, err)
	for _, a := range largeUndo.After {
		assert.Empty(t, a.Values.Caption)
	}
	redo, err := s.UndoPhotoAuthored(ctx, largeUndo.ReceiptID)
	require.NoError(t, err)
	for _, a := range redo.After {
		assert.Equal(t, text, a.Values.Caption)
	}
}

func TestPhotoAuthoredReleasedJSONLDefaults(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	lines := bytes.Split(backup.Bytes(), []byte{'\n'})
	for i, line := range lines {
		if bytes.Contains(line, []byte(`"type":"photo_file"`)) {
			f := asset.Files[0]
			old := metadataPhotoFileV28{Type: metadataPhotoFileType, FileID: f.ID, AssetID: f.AssetID, NodeID: f.NodeID, Role: f.Role, SidecarOfID: f.SidecarOfID, CreatedAt: f.CreatedAt}
			lines[i], err = json.Marshal(old, json.Deterministic(true))
			require.NoError(t, err)
		}
	}
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'}))))
	got, err := restored.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.Equal(t, asset.Files, got.Files)
}

func TestPhotoAuthoredLegacyAuditDefaults(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	auditFolder, err := s.Mkdir(ctx, s.RootID(), "Audited")
	require.NoError(t, err)
	photoFolder, err := s.Mkdir(ctx, s.RootID(), "Photos")
	require.NoError(t, err)
	node, err := s.CreateFile(ctx, photoFolder.ID, "photo.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	file := asset.Files[0]
	_, err = s.db.Exec(`DELETE FROM photo_change_receipts WHERE asset_id=?`, asset.ID)
	require.NoError(t, err)
	_, err = s.db.Exec(`DELETE FROM photo_assets WHERE asset_id=?`, asset.ID)
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, auditFolder.ID)
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO photo_assets(asset_id,kind,revision,display_file_id,created_at,updated_at) VALUES(?,?,?,?,?,?)`, asset.ID, asset.Kind, asset.Revision, file.ID, asset.CreatedAt, asset.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO photo_files(file_id,asset_id,node_id,role,created_at) VALUES(?,?,?,?,?)`, file.ID, asset.ID, file.NodeID, file.Role, file.CreatedAt)
		return err
	}))
	require.NoError(t, s.ValidateMetadata(ctx))
	plan, err := s.PreviewInitialAudit(ctx, photoFolder.ID, "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	human, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{file.ID, 1, PhotoAuthoredPatch{Rating: new(0)}}})
	require.NoError(t, err)
	require.NotEmpty(t, human.ReceiptID)
	require.NoError(t, s.ValidateMetadata(ctx))
	_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{file.ID, 2, PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoAuthoredMemberQuery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	jpg := fileByRole(asset.Files, PhotoRoleImage)
	_, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}, {jpg.ID, 1, PhotoAuthoredPatch{Rating: new(3), Label: new("red")}}})
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{"v":1,"filters":{"rating_min":5,"labels":["red"]}}`)
	page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: value}, nil)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	value.Filters = query.Filters{RatingMin: new(int64(3)), Labels: []string{"red"}}
	page, err = s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: value}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, asset.ID, page.Items[0].AssetID)
}

func TestPhotoAuthoredAuditRoundTripAndRollback(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	seedInitialAuditAuthority(t, s, s.RootID())
	targets := []PhotoAuthoredTarget{}
	for _, f := range asset.Files {
		targets = append(targets, PhotoAuthoredTarget{f.ID, 1, PhotoAuthoredPatch{Rating: new(5)}})
	}
	receipt, err := s.EditPhotoAuthored(ctx, targets)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	history, err := s.AuditHistory(ctx, asset.Files[0].NodeID, 10, "")
	require.NoError(t, err)
	require.NotNil(t, history.Items[0].Attachment)
	require.NotNil(t, history.Items[0].Attachment.After.Photo)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	_, err = restored.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	require.NoError(t, restored.ValidateMetadata(ctx))
	_, err = s.db.Exec(`CREATE TRIGGER reject_photo_audit BEFORE UPDATE ON audit_scopes BEGIN SELECT RAISE(ABORT,'forced photo audit failure'); END`)
	require.NoError(t, err)
	targets[0].Revision = 2
	targets[0].Patch.Rating = new(4)
	_, err = s.EditPhotoAuthored(ctx, targets[:1])
	require.ErrorContains(t, err, "forced photo audit failure")
	current, err := photoFileByIDQuery(ctx, s.db, targets[0].FileID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), current.Revision)
	assert.Equal(t, 5, current.Rating)
}

func TestPhotoSidecarInitializationFences(t *testing.T) {
	t.Parallel()
	for _, humanRating := range []int{-1, 0, 4} {
		t.Run(fmt.Sprintf("human rating=%d", humanRating), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			raw := fileByRole(asset.Files, PhotoRoleRAW)
			sidecar, err := s.CreateFile(ctx, s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml")
			require.NoError(t, err)
			_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
			publishPhotoPacket(t, s, sidecar.BlobHash, 5)
			targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			assert.Equal(t, sidecar.ID, targets[0].NodeID)
			if humanRating >= 0 {
				human, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(humanRating)}}})
				require.NoError(t, err)
				require.NotEmpty(t, human.ReceiptID)
				require.Len(t, human.After, 1)
				assert.Equal(t, int64(2), human.After[0].Revision)
				receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
				require.NoError(t, err)
				assert.Empty(t, receipt.ReceiptID)
				got, err := photoFileByIDQuery(ctx, s.db, raw.ID)
				require.NoError(t, err)
				assert.Equal(t, humanRating, got.Rating)
				assert.Equal(t, int64(2), got.Revision)
				require.NoError(t, s.ValidateMetadata(ctx))
				return
			}
			second, err := s.CreateFile(ctx, s.RootID(), "another.xmp", fakeHash("d4"), 4, "application/rdf+xml")
			require.NoError(t, err)
			currentAsset, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			_, err = s.AttachPhotoFile(ctx, asset.ID, currentAsset.Revision, second.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
			publishPhotoPacket(t, s, second.BlobHash, 4)
			firstPage, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
			require.NoError(t, err)
			require.Len(t, firstPage, 1)
			assert.Equal(t, sidecar.ID, firstPage[0].NodeID)
			secondPage, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), strconv.FormatInt(sidecar.ID, 10), 1)
			require.NoError(t, err)
			require.Len(t, secondPage, 1)
			assert.Equal(t, second.ID, secondPage[0].NodeID)
			receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
			require.NoError(t, err)
			require.NotEmpty(t, receipt.ReceiptID)
			assert.Equal(t, int64(2), receipt.After[0].Revision)
			repeated, err := s.InitializePhotoSidecar(ctx, targets[0])
			require.NoError(t, err)
			assert.Empty(t, repeated.ReceiptID)
			competing, err := s.InitializePhotoSidecar(ctx, secondPage[0])
			require.NoError(t, err)
			assert.Empty(t, competing.ReceiptID)
			_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			targets, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			assert.Empty(t, targets)
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &backup))
			restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		})
	}
}

func TestPhotoAuthoredRestoreRejectsDifferentReceiptNode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"different", "missing detached"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			f := asset.Files[0]
			receipt, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{f.ID, 1, PhotoAuthoredPatch{Rating: new(4)}}})
			require.NoError(t, err)
			if mode == "missing detached" {
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, f.ID, PhotoDetachOptions{})
				require.NoError(t, err)
			}
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &backup))
			lines := bytes.Split(backup.Bytes(), []byte{'\n'})
			for i, line := range lines {
				if mode == "different" && bytes.Contains(line, []byte(receipt.ReceiptID)) {
					lines[i] = bytes.ReplaceAll(line, []byte(`\"node_id\":`+strconv.FormatInt(f.NodeID, 10)), []byte(`\"node_id\":1`))
				}
				if mode == "missing detached" && bytes.Contains(line, []byte(`"type":"photo_file"`)) && bytes.Contains(line, []byte(f.ID)) {
					lines[i] = nil
				}
			}
			restored := newTestStore(t)
			err = restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
			require.ErrorContains(t, err, "missing or different file nodes")
		})
	}
}

func TestPhotoAuthoredRegroupingPreservesAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	f := fileByRole(asset.Files, PhotoRoleRAW)
	receipt, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{f.ID, 1, PhotoAuthoredPatch{Rating: new(5), Caption: new("River"), Creator: new("Example photographer")}}})
	require.NoError(t, err)
	for range 2 {
		asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, f.ID, PhotoDetachOptions{})
		require.NoError(t, err)
		_, err = s.PhotoAssetForNode(ctx, f.NodeID)
		require.ErrorIs(t, err, ErrNotFound)
		retained, err := photoFileByIDQuery(ctx, s.db, f.ID)
		require.NoError(t, err)
		assert.Empty(t, retained.AssetID)
		assert.Equal(t, int64(2), retained.Revision)
		assert.Equal(t, 5, retained.Rating)
		assert.Equal(t, f.CreatedAt, retained.CreatedAt)
		var backup bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &backup))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		require.NoError(t, restored.ValidateMetadata(ctx))
		retainedRestore, err := photoFileByIDQuery(ctx, restored.db, f.ID)
		require.NoError(t, err)
		assert.Equal(t, retained, retainedRestore)
		seedInitialAuditAuthority(t, restored, restored.RootID())
		require.NoError(t, restored.ValidateMetadata(ctx))
		asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, f.NodeID, PhotoRoleRAW, nil)
		require.NoError(t, err)
		assert.Equal(t, f.ID, fileByRole(asset.Files, PhotoRoleRAW).ID)
	}
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, f.ID, PhotoDetachOptions{})
	require.NoError(t, err)
	promoted, err := s.PromotePhotoNode(ctx, f.NodeID, nil, PhotoRoleRAW, PhotoKindPhoto)
	require.NoError(t, err)
	assert.Equal(t, f.ID, promoted.Files[0].ID)
	assert.Equal(t, 5, promoted.Files[0].Rating)
	assert.Equal(t, int64(2), promoted.Files[0].Revision)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, promoted.ID, promoted.Revision, f.ID, PhotoDetachOptions{})
	require.NoError(t, err)
	node, err := s.NodeByID(ctx, f.NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	_, err = photoFileByIDQuery(ctx, s.db, f.ID)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func publishPhotoPacket(t *testing.T, s *Store, hash string, rating int64) {
	t.Helper()
	metadata := document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{
		{Key: "image.xmp.packet_valid", Namespace: "image.xmp", SourceField: "packet", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(true)}},
		{Key: "image.xmp.rating", Namespace: "image.xmp", SourceField: "Rating", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(rating)}},
	}, Warnings: []document.SourceMetadataWarningV1{}}
	canonical, _, err := document.MarshalSourceMetadataV1(metadata)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), hash, fakeHash("ee"), canonical)
	require.NoError(t, err)
}

func TestPhotoSidecarEvidenceFencesAndProvenance(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"version", "trash", "membership", "checksum", "decode", "fingerprint", "role"} {
		t.Run(fence, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			f := fileByRole(asset.Files, PhotoRoleRAW)
			sidecar, err := s.CreateFile(ctx, s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml")
			require.NoError(t, err)
			asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &f.ID)
			require.NoError(t, err)
			pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
			publishPhotoPacket(t, s, sidecar.BlobHash, 4)
			targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.Equal(t, f.ID, targets[0].FileID)
			expected := ErrStaleRevision
			switch fence {
			case "version":
				_, _, err = s.ReplaceContent(ctx, sidecar.ID, sidecar.Revision, fakeHash("d4"), 4, "application/rdf+xml")
			case "trash":
				node, nodeErr := s.NodeByID(ctx, f.NodeID)
				require.NoError(t, nodeErr)
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
			case "membership":
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, f.ID, PhotoDetachOptions{ClearDependentSidecars: true})
			case "checksum":
				_, err = s.db.Exec(`DROP TRIGGER source_metadata_generations_immutable_update`)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE source_metadata_generations SET checksum=?`, fakeHash("ab"))
				expected = ErrSourceMetadataCorrupt
			case "decode":
				_, err = s.db.Exec(`DROP TRIGGER source_metadata_generations_immutable_update`)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE source_metadata_generations SET canonical_json='{}'`)
				expected = ErrSourceMetadataCorrupt
			case "fingerprint":
				targets[0].ExtractorFingerprint = fakeHash("ff")
			case "role":
				_, err = s.db.Exec(`UPDATE photo_files SET role='image',sidecar_of_file_id=NULL WHERE file_id=?`, targets[0].SidecarFileID)
			}
			require.NoError(t, err)
			_, err = s.InitializePhotoSidecar(ctx, targets[0])
			require.ErrorIs(t, err, expected)
		})
	}
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	f := fileByRole(asset.Files, PhotoRoleRAW)
	sidecar, err := s.CreateFile(ctx, s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &f.ID)
	require.NoError(t, err)
	publishPhotoPacket(t, s, sidecar.BlobHash, 4)
	targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
	require.NoError(t, err)
	assert.NotEmpty(t, receipt.ReceiptID)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, targets[0].SidecarFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	var incomplete bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &incomplete))
	lines := bytes.Split(incomplete.Bytes(), []byte{'\n'})
	for i, line := range lines {
		if bytes.Contains(line, []byte(`"type":"photo_file"`)) && bytes.Contains(line, []byte(targets[0].SidecarFileID)) {
			lines[i] = nil
		}
	}
	corruptRestore := newTestStore(t)
	require.ErrorContains(t, corruptRestore.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'}))), "sidecar provenance references missing or different nodes")

	_, _, err = s.ReplaceContent(ctx, sidecar.ID, sidecar.Revision, fakeHash("d4"), 4, "image/jpeg")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	require.NoError(t, restored.ValidateMetadata(ctx))
	node, err := s.NodeByID(ctx, sidecar.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	var purged bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &purged))
	purgedRestore := newTestStore(t)
	require.NoError(t, purgedRestore.ImportMetadata(ctx, bytes.NewReader(purged.Bytes())))
}

func TestPhotoSidecarListingUsesGenerationKey(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+photoSidecarCandidatesSQL, document.SourceMetadataContractV1, fakeHash("ee"), 0, 10)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	assert.Contains(t, plan, "SEARCH g USING INDEX")
	assert.Contains(t, plan, "source_sha256=? AND contract_version=? AND extractor_fingerprint=?")
	assert.NotContains(t, plan, "photo_change_receipts")
}

func TestPhotoSidecarSharedEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	for _, name := range []string{"first", "second"} {
		source, err := s.CreateFile(ctx, s.RootID(), name+".jpg", fakeHash("a1"), 4, "image/jpeg")
		require.NoError(t, err)
		asset, err := s.PhotoAssetForNode(ctx, source.ID)
		require.NoError(t, err)
		sidecar, err := s.CreateFile(ctx, s.RootID(), name+".xmp", fakeHash("c3"), 4, "application/rdf+xml")
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &asset.Files[0].ID)
		require.NoError(t, err)
	}
	publishPhotoPacket(t, s, fakeHash("c3"), 4)
	targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 2)
	assert.NotEqual(t, targets[0].FileID, targets[1].FileID)
	for _, target := range targets {
		receipt, err := s.InitializePhotoSidecar(ctx, target)
		require.NoError(t, err)
		require.NotEmpty(t, receipt.ReceiptID)
		assert.Equal(t, 4, receipt.After[0].Values.Rating)
	}
	targets, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	assert.Empty(t, targets)
	require.NoError(t, s.ValidateMetadata(ctx))
}
