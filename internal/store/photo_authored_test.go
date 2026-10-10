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
	"go.kenn.io/docbank/internal/audit"
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

func TestPhotoAuthoredReceiptRequiresCompleteValues(t *testing.T) {
	t.Parallel()
	const id = "40000000-0000-4000-8000-000000000001"
	before := PhotoAuthoredSnapshot{FileID: id, NodeID: 1, Revision: 1}
	after := before
	after.Revision++
	r := PhotoAuthoredReceipt{ReceiptID: id, Before: []PhotoAuthoredSnapshot{before}, After: []PhotoAuthoredSnapshot{after}}
	beforeJSON, err := json.Marshal(r.Before, json.Deterministic(true))
	require.NoError(t, err)
	afterJSON, err := json.Marshal(r.afterState(), json.Deterministic(true))
	require.NoError(t, err)
	_, err = decodePhotoAuthoredReceipt(beforeJSON, afterJSON, id)
	require.NoError(t, err)
	for _, side := range []string{"before", "after"} {
		for _, field := range []string{"values", "rating", "flag", "label", "caption", "creator", "copyright", "rotation"} {
			for _, null := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/null=%t", side, field, null), func(t *testing.T) {
					t.Parallel()
					var snapshots []map[string]any
					var state map[string]any
					b, a := beforeJSON, afterJSON
					if side == "before" {
						require.NoError(t, json.Unmarshal(b, &snapshots))
					} else {
						require.NoError(t, json.Unmarshal(a, &state))
						entries, ok := state["after"].([]any)
						require.True(t, ok)
						snapshot, ok := entries[0].(map[string]any)
						require.True(t, ok)
						snapshots = []map[string]any{snapshot}
					}
					values := snapshots[0]
					if field != "values" {
						var ok bool
						values, ok = values["values"].(map[string]any)
						require.True(t, ok)
					}
					if null {
						values[field] = nil
					} else {
						delete(values, field)
					}
					var encodeErr error
					if side == "before" {
						b, encodeErr = json.Marshal(snapshots, json.Deterministic(true))
					} else {
						a, encodeErr = json.Marshal(state, json.Deterministic(true))
					}
					require.NoError(t, encodeErr)
					_, decodeErr := decodePhotoAuthoredReceipt(b, a, id)
					require.ErrorIs(t, decodeErr, ErrInvalidPhotoAsset)
				})
			}
		}
	}
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
	_, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}, {jpg.ID, 1, PhotoAuthoredPatch{Rating: new(6)}}})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	rawNow, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Equal(t, raw, rawNow)
	_, err = s.EditPhotoPair(ctx, asset.ID, asset.Revision-1, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}})
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.EditPhotoPair(ctx, asset.ID, asset.Revision, []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5)}}})
	require.ErrorIs(t, err, ErrStaleRevision)
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
	undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, receipt.ReceiptID, undo.UndoOf)
	for _, after := range undo.After {
		assert.Equal(t, int64(3), after.Revision)
		assert.Equal(t, PhotoAuthored{}, after.Values)
	}
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	receipt, err = s.EditPhotoPair(ctx, asset.ID, asset.Revision, []PhotoAuthoredTarget{{raw.ID, 3, PhotoAuthoredPatch{Rating: new(5)}}, {jpg.ID, 3, PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	require.Len(t, receipt.After, 2)
	for _, after := range receipt.After {
		assert.Equal(t, int64(4), after.Revision)
		assert.Equal(t, 5, after.Values.Rating)
	}
	receipt, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{raw.ID, 4, PhotoAuthoredPatch{Rating: new(4)}}})
	require.NoError(t, err)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
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
	for _, test := range []string{"released defaults", "unknown confirmation", "unconfirmed value"} {
		t.Run(test, func(t *testing.T) {
			lines := bytes.Split(backup.Bytes(), []byte{'\n'})
			for i, line := range lines {
				if bytes.Contains(line, []byte(`"type":"photo_file"`)) {
					switch test {
					case "unknown confirmation":
						lines[i] = bytes.Replace(line, []byte(`"confirmed_fields":0`), []byte(`"confirmed_fields":128`), 1)
					case "unconfirmed value":
						lines[i] = bytes.Replace(line, []byte(`"rating":0`), []byte(`"rating":3`), 1)
					default:
						f := asset.Files[0]
						old := metadataPhotoFileBeforeAuthored{Type: metadataPhotoFileType, FileID: f.ID, AssetID: f.AssetID, NodeID: f.NodeID, Role: f.Role, SidecarOfID: f.SidecarOfID, CreatedAt: f.CreatedAt}
						lines[i], err = json.Marshal(old, json.Deterministic(true))
						require.NoError(t, err)
					}
				}
			}
			restored := newTestStore(t)
			err := restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
			if test != "released defaults" {
				require.ErrorContains(t, err, "invalid authored photo decision")
				_, err = restored.NodeByID(ctx, node.ID)
				require.ErrorIs(t, err, ErrNotFound)
				return
			}
			require.NoError(t, err)
			got, err := restored.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			assert.Equal(t, asset.Files, got.Files)
		})
	}
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
	require.Equal(t, PhotoConfirmedRating, human.After[0].Values.Confirmed)
	require.NoError(t, s.ValidateMetadata(ctx))
	_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{file.ID, 2, PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoAuthoredEnrollmentRetainsOutsideScopeCaption(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "Audited")
	require.NoError(t, err)
	node := browsePhotoNode(t, s, "outside.jpg", fakeHash("a1"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	_, err = s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{asset.Files[0].ID, 1, PhotoAuthoredPatch{Caption: new("Outside-scope caption")}}})
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	records, err := loadInitialAuditRecords(ctx, s.db)
	require.NoError(t, err)
	genesis, err := auditRecordListField(records["attached_metadata_genesis"][0].record, "records")
	require.NoError(t, err)
	var photos []audit.Record
	for _, record := range genesis {
		if record.Kind == "photo_authored" {
			photos = append(photos, record)
		}
	}
	require.Len(t, photos, 1)
	retained, err := photoAuthoredFromAudit(photos[0])
	require.NoError(t, err)
	assert.Equal(t, node.ID, retained.NodeID)
	assert.Equal(t, "Outside-scope caption", retained.Values.Caption)
	baseline, err := auditRecordListField(records["enrollment_baseline"][0].record, "attachments")
	require.NoError(t, err)
	for _, record := range baseline {
		assert.NotEqual(t, "photo_authored", record.Kind)
	}
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
	_, err := s.EditPhotoAuthored(ctx, targets)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	history, err := s.AuditHistory(ctx, asset.Files[0].NodeID, 10, "")
	require.NoError(t, err)
	require.NotNil(t, history.Items[0].Attachment)
	require.NotNil(t, history.Items[0].Attachment.After.Photo)
	cleared, err := s.EditPhotoAuthored(ctx, []PhotoAuthoredTarget{{targets[0].FileID, 2, PhotoAuthoredPatch{Caption: new("")}}})
	require.NoError(t, err)
	require.Len(t, cleared.After, 1)
	assert.Equal(t, PhotoConfirmedRating, cleared.Before[0].Values.Confirmed)
	assert.Equal(t, PhotoConfirmedRating|PhotoConfirmedCaption, cleared.After[0].Values.Confirmed)
	currentAsset, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.False(t, currentAsset.Agreement["caption"])
	for _, file := range currentAsset.Files {
		assert.Empty(t, file.Caption)
	}
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	current, err := photoFileByIDQuery(ctx, restored.db, targets[0].FileID)
	require.NoError(t, err)
	assert.Equal(t, cleared.After[0].Values, current.Authored())
	assert.Equal(t, PhotoConfirmedRating|PhotoConfirmedCaption, current.Confirmed)
	undo, err := restored.UndoPhotoAuthored(ctx, cleared.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, PhotoConfirmedRating, undo.After[0].Values.Confirmed)
	currentAsset, err = restored.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.True(t, currentAsset.Agreement["caption"])
	redo, err := restored.UndoPhotoAuthored(ctx, undo.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, cleared.After[0].Values, redo.After[0].Values)
	require.NoError(t, restored.ValidateMetadata(ctx))
	_, err = s.db.Exec(`CREATE TRIGGER reject_photo_audit BEFORE UPDATE ON audit_scopes BEGIN SELECT RAISE(ABORT,'forced photo audit failure'); END`)
	require.NoError(t, err)
	targets[0].Revision = 3
	targets[0].Patch.Rating = new(4)
	_, err = s.EditPhotoAuthored(ctx, targets[:1])
	require.ErrorContains(t, err, "forced photo audit failure")
	current, err = photoFileByIDQuery(ctx, s.db, targets[0].FileID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), current.Revision)
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
	canonical := photoCanonical(t,
		photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean(true)),
		photoMetadataField("image.xmp.rating", "image.xmp", "Rating", photoInteger(rating)),
	)
	_, err := s.PublishSourceMetadata(t.Context(), hash, fakeHash("ee"), canonical)
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
	assert.Contains(t, plan, "SEARCH g USING COVERING INDEX")
	assert.Contains(t, plan, "source_sha256=? AND contract_version=? AND extractor_fingerprint=?")
	assert.Contains(t, plan, "SEARCH c USING COVERING INDEX")
	assert.NotContains(t, plan, "json_each")
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

func TestPhotoSidecarIdleScanSkipsJudgedGeneration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		valid    bool
		matching bool
	}{{"valid=true", true, false}, {"valid=false", false, false}, {"matching values", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			f := fileByRole(asset.Files, PhotoRoleRAW)
			if test.matching {
				_, err := s.db.ExecContext(ctx, `UPDATE photo_files SET rating=4,confirmed_fields=1 WHERE file_id=?`, f.ID)
				require.NoError(t, err)
			}
			sidecar, err := s.CreateFile(ctx, s.RootID(), "empty.xmp", fakeHash("c3"), 4, "application/rdf+xml")
			require.NoError(t, err)
			_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &f.ID)
			require.NoError(t, err)
			canonical := photoCanonical(t, document.SourceMetadataFieldV1{Key: "image.xmp.packet_valid", Namespace: "image.xmp", SourceField: "packet", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: new(test.valid)}})
			if test.matching {
				canonical = photoCanonical(t,
					photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean(true)),
					photoMetadataField("image.xmp.rating", "image.xmp", "Rating", photoInteger(4)),
				)
			}
			_, err = s.PublishSourceMetadata(ctx, sidecar.BlobHash, fakeHash("ee"), canonical)
			require.NoError(t, err)
			targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
			require.NoError(t, err)
			assert.Empty(t, receipt.ReceiptID)
			assert.Empty(t, receipt.After)
			targets, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			assert.Empty(t, targets)
			file, err := photoFileByIDQuery(ctx, s.db, f.ID)
			require.NoError(t, err)
			assert.Equal(t, int64(1), file.Revision)
			if test.matching {
				assert.Equal(t, 4, file.Rating)
				require.NoError(t, s.ValidateMetadata(ctx))
			}
			_, err = s.PublishSourceMetadata(ctx, sidecar.BlobHash, fakeHash("ff"), canonical)
			require.NoError(t, err)
			targets, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ff"), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
		})
	}
}

func TestPhotoAuthoredExpressionBrowse(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	raw := fileByRole(asset.Files, PhotoRoleRAW)
	jpg := fileByRole(asset.Files, PhotoRoleImage)
	_, err := s.EditPhotoAuthored(t.Context(), []PhotoAuthoredTarget{{raw.ID, 1, PhotoAuthoredPatch{Rating: new(5), Flag: new("pick")}}, {jpg.ID, 1, PhotoAuthoredPatch{Rating: new(1), Label: new("red")}}})
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(t.Context(), "Selected decisions", "", SavedQueryKindQuery, []byte(`{"syntax":"advanced","text":"rating:1 AND label:red"}`))
	require.NoError(t, err)
	for _, stage := range []string{"raw", "raw with sidecar", "image preference with sidecar"} {
		if stage == "raw with sidecar" {
			sidecar := browsePhotoNode(t, s, "capture.xmp", fakeHash("c3"), "application/rdf+xml")
			asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
		}
		if stage == "image preference with sidecar" {
			_, err = s.SetPhotoSettings(t.Context(), 1, new("image"))
			require.NoError(t, err)
		}
		rawMatch, imageMatch := 1, 0
		if stage == "image preference with sidecar" {
			rawMatch, imageMatch = 0, 1
		}
		for _, tc := range []struct {
			text string
			want int
		}{
			{`rating:5`, rawMatch}, {`rating:1`, imageMatch}, {`rating:4`, 0},
			{`rating_min:3`, rawMatch}, {`rating_min:4`, rawMatch},
			{`rating_max:3`, imageMatch}, {`rating_max:2`, imageMatch},
			{`rating_min:3 AND rating_max:3`, 0},
			{`flag:pick`, rawMatch}, {`label:red`, imageMatch},
			{`NOT rating:5`, imageMatch}, {`NOT rating:1`, rawMatch}, {`NOT rating:4`, 1}, {`NOT flag:pick`, imageMatch},
			{`NOT flag:reject`, 1}, {`NOT label:red`, rawMatch},
			{`NOT (NOT rating:1 OR NOT label:red)`, imageMatch},
			{`rating:5 AND label:red`, 0}, {`rating:1 AND label:red`, imageMatch},
			{`saved:"Selected decisions"`, imageMatch},
		} {
			q := snapshotTestQuery(t, `{"syntax":"advanced"}`)
			q.Text = tc.text
			for _, collapse := range []bool{false, true} {
				q.Filters.CollapseDuplicates = collapse
				page, err := s.ListPhotoAssets(t.Context(), PhotoBrowseRequest{Query: q}, nil)
				require.NoError(t, err, stage, tc.text)
				assert.Len(t, page.Items, tc.want, stage, tc.text)
			}
		}
		page := browsePhotoPage(t, s, `{"filters":{"flags":[""]}}`)
		assert.Len(t, page.Items, imageMatch, stage)

		if stage != "raw" {
			text := `extension:xmp AND rating:1 AND NOT flag:pick`
			assert.Len(t, browsePhotoPage(t, s, `{"syntax":"advanced","text":`+strconv.Quote(text)+`}`).Items, imageMatch)
		}
	}
	asset, err = s.PhotoAssetByID(t.Context(), asset.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, raw.ID, PhotoDetachOptions{ClearDependentSidecars: true})
	require.NoError(t, err)
	snapshot, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"rating:5"}`)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 1)
	assert.Equal(t, raw.NodeID, snapshot.Rows[0].NodeID)
	snapshot, err = s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"NOT rating:5"}`)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 2)
	assert.NotContains(t, []int64{snapshot.Rows[0].NodeID, snapshot.Rows[1].NodeID}, raw.NodeID)
	snapshot, err = s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{"syntax":"advanced","text":"rating:1 AND label:red"}`)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 1)
	assert.Equal(t, jpg.NodeID, snapshot.Rows[0].NodeID)
}
