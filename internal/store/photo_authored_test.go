package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/audit"
	"go.kenn.io/docbank/internal/query"
)

func TestPhotoAuthoredConfirmationFields(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		bit   PhotoAuthoredFields
		patch PhotoAuthoredPatch
	}{
		{"rating", PhotoConfirmedRating, PhotoAuthoredPatch{Rating: new(5)}},
		{"flag", PhotoConfirmedFlag, PhotoAuthoredPatch{Flag: new("pick")}},
		{"label", PhotoConfirmedLabel, PhotoAuthoredPatch{Label: new("red")}},
		{"caption", PhotoConfirmedCaption, PhotoAuthoredPatch{Caption: new("River")}},
		{"creator", PhotoConfirmedCreator, PhotoAuthoredPatch{Creator: new("Example photographer")}},
		{"copyright", PhotoConfirmedCopyright, PhotoAuthoredPatch{Copyright: new("Example rights")}},
		{"rotation", PhotoConfirmedRotation, PhotoAuthoredPatch{Rotation: new(90)}},
		{"zero rating", PhotoConfirmedRating, PhotoAuthoredPatch{Rating: new(0)}},
		{"empty flag", PhotoConfirmedFlag, PhotoAuthoredPatch{Flag: new("")}},
		{"empty label", PhotoConfirmedLabel, PhotoAuthoredPatch{Label: new("")}},
		{"empty caption", PhotoConfirmedCaption, PhotoAuthoredPatch{Caption: new("")}},
		{"empty creator", PhotoConfirmedCreator, PhotoAuthoredPatch{Creator: new("")}},
		{"empty copyright", PhotoConfirmedCopyright, PhotoAuthoredPatch{Copyright: new("")}},
		{"zero rotation", PhotoConfirmedRotation, PhotoAuthoredPatch{Rotation: new(0)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := test.patch.apply(PhotoAuthored{})
			require.Equal(t, test.bit, value.Confirmed)
			require.NoError(t, ValidatePhotoAuthored(value))
			value.Confirmed = 0
			if value != (PhotoAuthored{}) {
				require.ErrorIs(t, ValidatePhotoAuthored(value), ErrInvalidPhotoAsset)
			}
		})
	}
	require.ErrorIs(t, ValidatePhotoAuthored(PhotoAuthored{Confirmed: 128}), ErrInvalidPhotoAsset)
}

func TestPhotoAuthoredAuditRejectsUnsupportedConfirmationBits(t *testing.T) {
	t.Parallel()
	snapshot := PhotoAuthoredSnapshot{FileID: "40000000-0000-4000-8000-000000000001", NodeID: 1, Revision: 1}
	for _, confirmed := range []uint64{128, 256} {
		record, err := photoAuthoredAuditRecord(snapshot)
		require.NoError(t, err)
		for i := range record.Fields {
			if record.Fields[i].Name == "confirmed_fields" {
				record.Fields[i].Value = audit.Unsigned(confirmed)
			}
		}
		_, err = photoAuthoredFromAudit(record)
		require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	}
}

func TestPhotoAuthoredReceiptShapeValidation(t *testing.T) {
	t.Parallel()
	const id = "40000000-0000-4000-8000-000000000001"
	before := PhotoAuthoredSnapshot{FileID: id, NodeID: 1<<53 + 1, Revision: 1}
	after := before
	after.Revision++
	r := PhotoAuthoredReceipt{ReceiptID: id, Before: []PhotoAuthoredSnapshot{before}, After: []PhotoAuthoredSnapshot{after}}
	beforeJSON, err := json.Marshal(r.Before, json.Deterministic(true))
	require.NoError(t, err)
	afterJSON, err := json.Marshal(r.afterState(), json.Deterministic(true))
	require.NoError(t, err)
	_, err = decodePhotoAuthoredReceipt(beforeJSON, afterJSON, id)
	require.NoError(t, err)
	var beforeFields []map[string]jsontext.Value
	var afterFields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(beforeJSON, &beforeFields))
	require.NoError(t, json.Unmarshal(afterJSON, &afterFields))
	reorderedBefore, err := json.Marshal(beforeFields, json.Deterministic(true), jsontext.ReorderRawObjects(true), jsontext.Multiline(true))
	require.NoError(t, err)
	reorderedAfter, err := json.Marshal(afterFields, json.Deterministic(true), jsontext.ReorderRawObjects(true), jsontext.Multiline(true))
	require.NoError(t, err)
	reorderedAfter = bytes.ReplaceAll(reorderedAfter, []byte(`"rating"`), []byte(`"rat\u0069ng"`))
	decoded, err := decodePhotoAuthoredReceipt(reorderedBefore, reorderedAfter, id)
	require.NoError(t, err)
	assert.Equal(t, r, decoded)
	type testCase struct {
		name        string
		receipt     PhotoAuthoredReceipt
		side, field string
		null, valid bool
	}
	var cases []testCase
	for _, side := range []string{"before", "after"} {
		for _, field := range []string{"values", "rating"} {
			for _, null := range []bool{false, true} {
				cases = append(cases, testCase{name: fmt.Sprintf("%s/%s/null=%t", side, field, null), receipt: r, side: side, field: field, null: null})
			}
		}
	}
	file := PhotoAuthoredSnapshot{FileID: id, NodeID: 1, Revision: 2}
	for _, mode := range []string{"unchanged", "fence only", "delta only", "other node", "sidecar", "keyword"} {
		receipt := PhotoAuthoredReceipt{ReceiptID: id, Before: []PhotoAuthoredSnapshot{file}, After: []PhotoAuthoredSnapshot{file}}
		if mode != "unchanged" && mode != "delta only" {
			receipt.KeywordNodes = []PhotoKeywordNode{{NodeID: 1, AfterRevision: 3}}
		}
		if mode != "unchanged" && mode != "fence only" {
			receipt.Keywords = []PhotoKeywordDelta{{NodeID: 1, TagID: id, Before: false, After: true}}
		}
		if mode == "other node" {
			receipt.KeywordNodes[0].NodeID = 2
			receipt.Keywords[0].NodeID = 2
		}
		if mode == "sidecar" {
			receipt.Sidecar = &PhotoSidecarProvenance{NodeID: 2, VersionID: id, FileID: id}
		}
		cases = append(cases, testCase{name: mode, receipt: receipt, valid: mode == "keyword"})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(test.receipt.Before, json.Deterministic(true))
			require.NoError(t, err)
			a, err := json.Marshal(test.receipt.afterState(), json.Deterministic(true))
			require.NoError(t, err)
			if test.field != "" {
				side, field, null := test.side, test.field, test.null
				var snapshots []map[string]jsontext.Value
				var state map[string]jsontext.Value
				if side == "before" {
					require.NoError(t, json.Unmarshal(b, &snapshots))
				} else {
					require.NoError(t, json.Unmarshal(a, &state))
					require.NoError(t, json.Unmarshal(state["after"], &snapshots))
				}
				values := snapshots[0]
				if field != "values" {
					rawValues := values["values"]
					values = nil
					require.NoError(t, json.Unmarshal(rawValues, &values))
				}
				if null {
					values[field] = jsontext.Value(`null`)
				} else {
					delete(values, field)
				}
				var encodeErr error
				if field != "values" {
					snapshots[0]["values"], encodeErr = json.Marshal(values, json.Deterministic(true))
					require.NoError(t, encodeErr)
				}
				if side == "before" {
					b, encodeErr = json.Marshal(snapshots, json.Deterministic(true))
				} else {
					state["after"], encodeErr = json.Marshal(snapshots, json.Deterministic(true))
					require.NoError(t, encodeErr)
					a, encodeErr = json.Marshal(state, json.Deterministic(true))
				}
				require.NoError(t, encodeErr)
			}
			_, err = decodePhotoAuthoredReceipt(b, a, id)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrInvalidPhotoAsset)
			}
		})
	}
}

func TestPhotoAuthoredCompleteManyFileReceipt(t *testing.T) {
	t.Parallel()
	t.Run("snapshot overflow", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		targets := []PhotoAuthoredTarget{}
		for i := range 80 {
			node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("photo-%d.jpg", i), fakeHash("b2"), 4, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			f := asset.Files[0]
			targets = append(targets, PhotoAuthoredTarget{FileID: f.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(4)}})
		}
		receipt, err := editPhotoAuthored(ctx, s, targets)
		require.NoError(t, err)
		require.Len(t, receipt.After, 80)
		stored, err := loadPhotoAuthoredReceipt(ctx, s.db, receipt.ReceiptID)
		require.NoError(t, err)
		assert.Equal(t, receipt, stored)
		text := strings.Repeat("x", MaxPhotoAuthoredTextBytes)
		for i := range targets {
			targets[i].Revision = 2
			targets[i].Patch = PhotoAuthoredPatch{Caption: &text}
		}
		large, err := editPhotoAuthored(ctx, s, targets)
		require.NoError(t, err)
		for i := range targets {
			targets[i].Revision = 3
			targets[i].Patch = PhotoAuthoredPatch{Rating: new(3)}
		}
		_, err = editPhotoAuthored(ctx, s, targets)
		require.ErrorIs(t, err, ErrPhotoEditReceiptLimit)
		current, err := photoFileByIDQuery(ctx, s.db, targets[0].FileID)
		require.NoError(t, err)
		assert.Equal(t, int64(3), current.Revision)
		stored, err = loadPhotoAuthoredReceipt(ctx, s.db, large.ReceiptID)
		require.NoError(t, err)
		assert.Len(t, stored.After, 80)
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
	})
	t.Run("keyword delta overflow", func(t *testing.T) {
		s := newTestStore(t)
		ctx := t.Context()
		receipt := PhotoAuthoredReceipt{}
		nodes := map[int64]Node{}
		var changes []PhotoKeywordDelta
		for i := range 500 {
			nodeID := int64(i + 1)
			file := PhotoAuthoredSnapshot{FileID: fmt.Sprintf("40000000-0000-4000-8000-%012d", i), NodeID: nodeID, Revision: 1}
			receipt.Before = append(receipt.Before, file)
			receipt.After = append(receipt.After, file)
			nodes[nodeID] = Node{ID: nodeID, Revision: 1}
			for tag := range 100 {
				changes = append(changes, PhotoKeywordDelta{NodeID: nodeID, TagID: fmt.Sprintf("50000000-0000-4000-8000-%012d", tag), After: true})
			}
		}
		err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			return s.applyPhotoKeywordsTx(ctx, tx, &receipt, photoKeywordPlan{changes: changes, states: map[string]photoKeywordState{}}, nodes)
		})
		require.ErrorIs(t, err, ErrPhotoEditReceiptLimit)
	})
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
	for _, test := range []string{"released defaults", "unconfirmed value"} {
		t.Run(test, func(t *testing.T) {
			lines := bytes.Split(backup.Bytes(), []byte{'\n'})
			for i, line := range lines {
				if bytes.Contains(line, []byte(`"type":"photo_file"`)) {
					switch test {
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
	human, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(0)}}})
	require.NoError(t, err)
	require.NotEmpty(t, human.ReceiptID)
	require.NoError(t, s.ValidateMetadata(ctx))
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: file.ID, Revision: 2, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
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
	_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Caption: new("Outside-scope caption")}}})
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
		targets = append(targets, PhotoAuthoredTarget{FileID: f.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(5)}})
	}
	_, err := editPhotoAuthored(ctx, s, targets)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	history, err := s.AuditHistory(ctx, asset.Files[0].NodeID, 10, "")
	require.NoError(t, err)
	require.NotNil(t, history.Items[0].Attachment)
	require.NotNil(t, history.Items[0].Attachment.After.Photo)
	cleared, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: targets[0].FileID, Revision: 2, Patch: PhotoAuthoredPatch{Caption: new("")}}})
	require.NoError(t, err)
	require.Len(t, cleared.After, 1)
	assert.Equal(t, PhotoConfirmedRating, cleared.Before[0].Values.Confirmed)
	assert.Equal(t, PhotoConfirmedRating|PhotoConfirmedCaption, cleared.After[0].Values.Confirmed)
	currentAsset, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	assert.True(t, currentAsset.Agreement["caption"])
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	current, err := photoFileByIDQuery(ctx, restored.db, targets[0].FileID)
	require.NoError(t, err)
	assert.Equal(t, cleared.After[0].Values, current.Authored())
	undo, err := restored.UndoPhotoAuthored(ctx, cleared.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, PhotoConfirmedRating, undo.After[0].Values.Confirmed)
	redo, err := restored.UndoPhotoAuthored(ctx, undo.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, cleared.After[0].Values, redo.After[0].Values)
	require.NoError(t, restored.ValidateMetadata(ctx))
	_, err = s.db.Exec(`CREATE TRIGGER reject_photo_audit BEFORE UPDATE ON audit_scopes BEGIN SELECT RAISE(ABORT,'forced photo audit failure'); END`)
	require.NoError(t, err)
	targets[0].Revision = 3
	targets[0].Patch.Rating = new(4)
	_, err = editPhotoAuthored(ctx, s, targets[:1])
	require.ErrorContains(t, err, "forced photo audit failure")
	current, err = photoFileByIDQuery(ctx, s.db, targets[0].FileID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), current.Revision)
	assert.Equal(t, 5, current.Rating)
}

func authoredSidecar(t *testing.T) (*Store, PhotoAsset, PhotoFile, Node) {
	t.Helper()
	s := newTestStore(t)
	asset := authoredPair(t, s)
	file := fileByRole(asset.Files, PhotoRoleRAW)
	sidecar, err := s.CreateFile(t.Context(), s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &file.ID)
	require.NoError(t, err)
	return s, asset, file, sidecar
}

func TestPhotoSidecarHumanDecisionBlocksInitialization(t *testing.T) {
	t.Parallel()
	s, _, raw, sidecar := authoredSidecar(t)
	ctx := t.Context()
	publishPhotoPacket(t, s, sidecar.BlobHash, 5)
	targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, sidecar.ID, targets[0].NodeID)
	human, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(0)}}})
	require.NoError(t, err)
	require.NotEmpty(t, human.ReceiptID)
	require.Len(t, human.After, 1)
	assert.Equal(t, int64(2), human.After[0].Revision)
	receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.Empty(t, receipt.ReceiptID)
	targets, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	receipt, err = s.InitializePhotoSidecar(ctx, targets[0])
	require.NoError(t, err)
	assert.Empty(t, receipt.ReceiptID)
	got, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, got.Rating)
	assert.Equal(t, int64(2), got.Revision)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoSidecarCompetingPacketsSettle(t *testing.T) {
	t.Parallel()
	s, asset, raw, sidecar := authoredSidecar(t)
	ctx := t.Context()
	publishPhotoPacket(t, s, sidecar.BlobHash, 5)
	second, err := s.CreateFile(ctx, s.RootID(), "another.xmp", fakeHash("d4"), 4, "application/rdf+xml")
	require.NoError(t, err)
	currentAsset, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, currentAsset.Revision, second.ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	publishPhotoPacket(t, s, second.BlobHash, 4)
	targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	require.Len(t, targets, 1, "the higher-ID sidecar waits for its lower-ID sibling")
	assert.Equal(t, sidecar.ID, targets[0].NodeID)
	receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
	require.NoError(t, err)
	require.NotEmpty(t, receipt.ReceiptID)
	assert.Equal(t, int64(2), receipt.After[0].Revision)
	repeated, err := s.InitializePhotoSidecar(ctx, targets[0])
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.Empty(t, repeated.ReceiptID)
	pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, second.ID, pending[0].NodeID)
	competing, err := s.InitializePhotoSidecar(ctx, pending[0])
	require.NoError(t, err)
	assert.Empty(t, competing.ReceiptID)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	for range 2 {
		pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		settled, err := s.InitializePhotoSidecar(ctx, pending[0])
		require.NoError(t, err)
		assert.Empty(t, settled.ReceiptID)
	}
	pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	file, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Zero(t, file.Rating, "undo leaves the original decided, so neither packet reapplies")
}

func TestPhotoAuthoredRestoreRejectsDifferentReceiptNode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"different", "missing detached", "missing sidecar"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			f := asset.Files[0]
			var receipt PhotoAuthoredReceipt
			var err error
			missingFile := f.ID
			if strings.HasPrefix(mode, "missing sidecar") {
				sidecar, createErr := s.CreateFile(ctx, s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml")
				require.NoError(t, createErr)
				asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &f.ID)
				require.NoError(t, err)
				publishPhotoPacket(t, s, sidecar.BlobHash, 4)
				targets, listErr := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
				require.NoError(t, listErr)
				require.Len(t, targets, 1)
				receipt, err = s.InitializePhotoSidecar(ctx, targets[0])
				require.NoError(t, err)
				missingFile = targets[0].SidecarFileID
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, missingFile, PhotoDetachOptions{})
			} else {
				receipt, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: f.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(4)}}})
			}
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
				if (mode == "missing detached" || mode == "missing sidecar") && bytes.Contains(line, []byte(`"type":"photo_file"`)) && bytes.Contains(line, []byte(missingFile)) {
					lines[i] = nil
				}
			}
			restored := newTestStore(t)
			err = restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
			if strings.HasPrefix(mode, "missing sidecar") {
				require.ErrorContains(t, err, "sidecar provenance references missing or different nodes")
			} else {
				require.ErrorContains(t, err, "missing or different file nodes")
			}
		})
	}
}

func TestPhotoAuthoredRegroupingPreservesAuthority(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	restore := func(t *testing.T, s *Store) *Store {
		t.Helper()
		var backup bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &backup))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		require.NoError(t, restored.ValidateMetadata(ctx))
		return restored
	}
	t.Run("file", func(t *testing.T) {
		s := newTestStore(t)
		asset := authoredPair(t, s)
		f := fileByRole(asset.Files, PhotoRoleRAW)
		receipt, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: f.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(5), Caption: new("River"), Creator: new("Example photographer")}}})
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
			restored := restore(t, s)
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
	})
	t.Run("sidecar", func(t *testing.T) {
		s, asset, _, sidecar := authoredSidecar(t)
		publishPhotoPacket(t, s, sidecar.BlobHash, 4)
		targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
		require.NoError(t, err)
		assert.NotEmpty(t, receipt.ReceiptID)
		asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, targets[0].SidecarFileID, PhotoDetachOptions{})
		require.NoError(t, err)
		_, _, err = s.ReplaceContent(ctx, sidecar.ID, sidecar.Revision, fakeHash("d4"), 4, "image/jpeg")
		require.NoError(t, err)
		_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleImage, nil)
		require.NoError(t, err)
		require.NoError(t, s.ValidateMetadata(ctx))
		restore(t, s)
	})
}

func TestPhotoAuthoredPurgeRemovesReceipts(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"attached", "detached", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			raw := fileByRole(asset.Files, PhotoRoleRAW)
			jpg := fileByRole(asset.Files, PhotoRoleImage)
			seed, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{
				{FileID: raw.ID, Revision: 1, Patch: PhotoAuthoredPatch{Caption: new("Deleted caption"), Creator: new("Deleted creator"), Copyright: new("Deleted copyright")}},
			})
			require.NoError(t, err)
			receipt, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{
				{FileID: raw.ID, Revision: 2, Patch: PhotoAuthoredPatch{Rating: new(4)}},
				{FileID: jpg.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(4)}},
			})
			require.NoError(t, err)
			require.Len(t, receipt.After, 2)
			undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			redo, err := s.UndoPhotoAuthored(ctx, undo.ReceiptID)
			require.NoError(t, err)
			other, err := s.CreateFile(ctx, s.RootID(), "unrelated.jpg", fakeHash("d4"), 4, "image/jpeg")
			require.NoError(t, err)
			otherAsset, err := s.PhotoAssetForNode(ctx, other.ID)
			require.NoError(t, err)
			unrelated, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: otherAsset.Files[0].ID, Revision: 1, Patch: PhotoAuthoredPatch{Caption: new("Retained caption")}}})
			require.NoError(t, err)
			if mode == "attached" {
				_, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
			} else {
				_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, PhotoDetachOptions{})
				require.NoError(t, err)
				node, nodeErr := s.NodeByID(ctx, raw.NodeID)
				require.NoError(t, nodeErr)
				_, _, err = s.Trash(ctx, node.ID, node.Revision)
			}
			require.NoError(t, err)
			if mode == "rollback" {
				_, err = s.db.Exec(`CREATE TRIGGER reject_authored_purge BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT,'forced purge failure'); END`)
				require.NoError(t, err)
				_, err = s.TrashEmpty(ctx, 0, true)
				require.ErrorContains(t, err, "forced purge failure")
				for _, id := range []string{seed.ReceiptID, receipt.ReceiptID, undo.ReceiptID, redo.ReceiptID} {
					_, err = loadPhotoAuthoredReceipt(ctx, s.db, id)
					require.NoError(t, err)
				}
				_, err = s.db.Exec(`DROP TRIGGER reject_authored_purge`)
				require.NoError(t, err)
			}
			_, err = s.TrashEmpty(ctx, 0, true)
			require.NoError(t, err)
			for _, id := range []string{seed.ReceiptID, receipt.ReceiptID, undo.ReceiptID, redo.ReceiptID} {
				_, err = loadPhotoAuthoredReceipt(ctx, s.db, id)
				require.ErrorIs(t, err, ErrNotFound)
			}
			if mode != "attached" {
				_, err = photoFileByIDQuery(ctx, s.db, jpg.ID)
				require.NoError(t, err)
			}
			_, err = loadPhotoAuthoredReceipt(ctx, s.db, unrelated.ReceiptID)
			require.NoError(t, err)
			require.NoError(t, s.ValidateMetadata(ctx))
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &backup))
			for _, text := range []string{"Deleted caption", "Deleted creator", "Deleted copyright"} {
				assert.NotContains(t, backup.String(), text)
			}
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
			_, err = restored.UndoPhotoAuthored(ctx, unrelated.ReceiptID)
			require.NoError(t, err)
		})
	}
	t.Run("sidecar", func(t *testing.T) {
		t.Parallel()
		s, asset, _, sidecar := authoredSidecar(t)
		ctx := t.Context()
		publishPhotoPacket(t, s, sidecar.BlobHash, 4)
		targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
		require.NoError(t, err)
		_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, targets[0].SidecarFileID, PhotoDetachOptions{})
		require.NoError(t, err)
		node, err := s.NodeByID(ctx, sidecar.ID)
		require.NoError(t, err)
		_, _, err = s.Trash(ctx, node.ID, node.Revision)
		require.NoError(t, err)
		_, err = s.TrashEmpty(ctx, 0, true)
		require.NoError(t, err)
		_, err = loadPhotoAuthoredReceipt(ctx, s.db, receipt.ReceiptID)
		require.ErrorIs(t, err, ErrNotFound)
		require.NoError(t, s.ValidateMetadata(ctx))
		var backup bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &backup))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
	})
}

func publishPhotoPacket(t *testing.T, s *Store, hash string, rating int64) {
	t.Helper()
	canonical := photoCanonical(t,
		photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean()),
		photoMetadataField("image.xmp.rating", "image.xmp", "Rating", photoInteger(rating)),
	)
	_, err := s.PublishSourceMetadata(t.Context(), hash, fakeHash("ee"), canonical)
	require.NoError(t, err)
}

func TestPhotoSidecarEvidenceFencesAndProvenance(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"version", "trash", "membership", "checksum", "decode", "fingerprint", "rename sidecar", "rename original"} {
		t.Run(fence, func(t *testing.T) {
			s, asset, f, sidecar := authoredSidecar(t)
			ctx := t.Context()
			pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
			publishPhotoPacket(t, s, sidecar.BlobHash, 4)
			targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.Equal(t, sidecar.ID, targets[0].NodeID)
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
			case "rename sidecar", "rename original":
				node := sidecar
				if fence == "rename original" {
					node, err = s.NodeByID(ctx, f.NodeID)
					require.NoError(t, err)
				}
				_, _, err = s.Move(ctx, node.ID, s.RootID(), "renamed"+filepath.Ext(node.Name), node.Revision)
			}
			require.NoError(t, err)
			if errors.Is(expected, ErrSourceMetadataCorrupt) {
				targets, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
				require.NoError(t, err)
				require.Len(t, targets, 1)
			}
			_, err = s.InitializePhotoSidecar(ctx, targets[0])
			require.ErrorIs(t, err, expected)
			if strings.HasPrefix(fence, "rename ") {
				pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				receipt, err := s.InitializePhotoSidecar(ctx, pending[0])
				require.NoError(t, err)
				require.Len(t, receipt.After, 1)
				assert.Equal(t, f.ID, receipt.After[0].FileID)
			}
		})
	}
}

// singlePhotoSidecar adds a photo with its own sidecar, outside the authoredSidecar asset.
func singlePhotoSidecar(t *testing.T, s *Store, name, hash, sidecarHash string) Node {
	t.Helper()
	ctx := t.Context()
	photo, err := s.CreateFile(ctx, s.RootID(), name+".jpg", hash, 4, "image/jpeg")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, photo.ID)
	require.NoError(t, err)
	sidecar, err := s.CreateFile(ctx, s.RootID(), name+".xmp", sidecarHash, 4, "application/rdf+xml")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &asset.Files[0].ID)
	require.NoError(t, err)
	return sidecar
}

func TestPhotoSidecarListingPaginatesByNodeID(t *testing.T) {
	t.Parallel()
	s, _, _, sidecar := authoredSidecar(t)
	ctx := t.Context()
	second := singlePhotoSidecar(t, s, "other", fakeHash("e5"), fakeHash("d4"))
	publishPhotoPacket(t, s, sidecar.BlobHash, 5)
	publishPhotoPacket(t, s, second.BlobHash, 4)
	firstPage, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	require.Len(t, firstPage, 1)
	assert.Equal(t, sidecar.ID, firstPage[0].NodeID)
	secondPage, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), strconv.FormatInt(sidecar.ID, 10), 1)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	assert.Equal(t, second.ID, secondPage[0].NodeID)
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
	assert.NotEqual(t, targets[0].SidecarFileID, targets[1].SidecarFileID)
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
	valid := photoCanonical(t, photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean()))
	// The extractor reports a malformed packet with a warning and no packet_valid fact.
	rejected, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{
		ContractVersion: document.SourceMetadataContractV1,
		Warnings: []document.SourceMetadataWarningV1{{
			Code: "unparseable_metadata", Namespace: "image.xmp", SourceField: "packet",
			Detail: "standalone XMP packet could not be read",
		}},
	})
	require.NoError(t, err)
	for _, test := range []struct {
		name      string
		canonical []byte
	}{{"valid empty packet", valid}, {"rejected packet", rejected}} {
		t.Run(test.name, func(t *testing.T) {
			s, _, f, sidecar := authoredSidecar(t)
			ctx := t.Context()
			canonical := test.canonical
			_, err := s.PublishSourceMetadata(ctx, sidecar.BlobHash, fakeHash("ee"), canonical)
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
	_, err := editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(5), Flag: new("pick")}}})
	require.NoError(t, err)
	_, err = editPhotoAuthored(t.Context(), s, []PhotoAuthoredTarget{{FileID: jpg.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(1), Label: new("red")}}})
	require.NoError(t, err)
	for _, stage := range []string{"raw", "raw with sidecar", "image preference with sidecar"} {
		if stage == "raw with sidecar" {
			sidecar := browsePhotoNode(t, s, "capture.xmp", fakeHash("c3"), "application/rdf+xml")
			asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
		}
		if stage == "image preference with sidecar" {
			_, err = s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image")})
			require.NoError(t, err)
		}
		displayed := browsePhotoPage(t, s, `{}`).Items
		require.Len(t, displayed, 1)
		if stage == "image preference with sidecar" {
			assert.Equal(t, 1, displayed[0].Rating)
			assert.Empty(t, displayed[0].Flag)
		} else {
			assert.Equal(t, 5, displayed[0].Rating)
			assert.Equal(t, "pick", displayed[0].Flag)
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
			{`rating_min:3`, rawMatch},
			{`rating_max:3`, imageMatch},
			{`rating_min:3 AND rating_max:3`, 0},
			{`flag:pick`, rawMatch}, {`label:red`, imageMatch},
			{`NOT rating:5`, imageMatch}, {`NOT rating:4`, 1},
			{`rating:5 AND label:red`, 0}, {`rating:1 AND label:red`, imageMatch},
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
}

func TestPhotoSidecarRenamesRetryCachedExclusion(t *testing.T) {
	t.Parallel()
	for _, member := range []string{"sidecar", "original"} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%t", member, batch), func(t *testing.T) {
				s, asset, raw, sidecar := authoredSidecar(t)
				ctx := t.Context()
				move := func(node Node, name string) Node {
					t.Helper()
					if batch {
						results, err := s.BatchMove(ctx, []BatchMoveRequest{{NodeID: node.ID, IfRevision: node.Revision, DestinationPath: "/" + name}})
						require.NoError(t, err)
						return results[0].Node
					}
					moved, _, err := s.Move(ctx, node.ID, s.RootID(), name, node.Revision)
					require.NoError(t, err)
					return moved
				}
				sidecar = move(sidecar, "missing.cr2.xmp")
				publishPhotoPacket(t, s, sidecar.BlobHash, 4)
				targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				require.Len(t, targets, 1)
				receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
				require.NoError(t, err)
				assert.Empty(t, receipt.After)
				pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				require.Empty(t, pending)
				if member == "sidecar" {
					move(sidecar, "capture.cr2.xmp")
				} else {
					node, err := s.NodeByID(ctx, raw.NodeID)
					require.NoError(t, err)
					move(node, "missing.cr2")
				}
				pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				assert.Equal(t, asset.Revision, pending[0].AssetRevision)
				assert.NotEqual(t, targets[0].MemberRevisions, pending[0].MemberRevisions)
				receipt, err = s.InitializePhotoSidecar(ctx, pending[0])
				require.NoError(t, err)
				require.Len(t, receipt.After, 1)
				assert.Equal(t, raw.ID, receipt.After[0].FileID)
				assert.Equal(t, 4, receipt.After[0].Values.Rating)
				pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				assert.Empty(t, pending)
				require.NoError(t, s.ValidateMetadata(ctx))
			})
		}
	}
}

func TestPhotoSidecarOrdinaryIngestPairingCorrection(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	run, err := s.BeginIngest(ctx, "cli", "/synthetic/camera")
	require.NoError(t, err)
	rawNode, _, err := s.IngestFile(ctx, run, s.RootID(), "capture.cr2", fakeHash("a1"), 4, "application/octet-stream", "/synthetic/camera/capture.cr2", "")
	require.NoError(t, err)
	jpgNode, _, err := s.IngestFile(ctx, run, s.RootID(), "capture.jpg", fakeHash("b2"), 4, "image/jpeg", "/synthetic/camera/capture.jpg", "")
	require.NoError(t, err)
	sidecar, _, err := s.IngestFile(ctx, run, s.RootID(), "capture.jpg.xmp", fakeHash("c3"), 4, "application/rdf+xml", "/synthetic/camera/capture.jpg.xmp", "")
	require.NoError(t, err)
	jpgAsset, err := s.PhotoAssetForNode(ctx, jpgNode.ID)
	require.NoError(t, err)
	jpg := jpgAsset.Files[0]
	_, err = s.DetachPhotoFile(ctx, jpgAsset.ID, jpgAsset.Revision, jpg.ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err := s.PromotePhotoNode(ctx, rawNode.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	raw := asset.Files[0]
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpgNode.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	publishPhotoPacket(t, s, sidecar.BlobHash, 4)
	targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	repeated, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	assert.Equal(t, targets, repeated)
	var considered int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_sidecar_considered`).Scan(&considered))
	assert.Zero(t, considered, "listing must leave outcome writes to the processor")
	receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
	require.NoError(t, err)
	assert.Empty(t, receipt.After, "an ordinary ingest cannot override an explicit binding")
	pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	assert.Empty(t, pending)
	asset, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, targets[0].SidecarFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &jpg.ID)
	require.NoError(t, err)
	_, err = s.InitializePhotoSidecar(ctx, targets[0])
	require.ErrorIs(t, err, ErrStaleRevision)
	pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, asset.Revision, pending[0].AssetRevision)
	receipt, err = s.InitializePhotoSidecar(ctx, pending[0])
	require.NoError(t, err)
	require.Len(t, receipt.After, 1)
	assert.Equal(t, jpg.ID, receipt.After[0].FileID)
	assert.Equal(t, 4, receipt.After[0].Values.Rating)
	untouched, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Zero(t, untouched.Rating)
	pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	assert.Empty(t, pending)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoSidecarProvenanceCorrectionRetriesCachedOutcome(t *testing.T) {
	t.Parallel()
	for _, role := range []string{PhotoRoleRAW, PhotoRoleImage, PhotoRoleSidecar, "ordinary origin"} {
		t.Run(role, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			root := filepath.Join(t.TempDir(), "camera")
			run, err := s.BeginIngest(ctx, "photo-import", root)
			require.NoError(t, err)
			if role == "ordinary origin" {
				path := filepath.Join(root, "capture.xmp")
				ordinary, err := s.BeginIngest(ctx, "cli", root)
				require.NoError(t, err)
				ordinaryIdentity, err := provenanceIdentity(metadataProvenance{NodeID: 2, IngestID: ordinary.ID(), OriginalPath: path})
				require.NoError(t, err)
				importedIdentity, err := provenanceIdentity(metadataProvenance{NodeID: 2, IngestID: run.ID(), OriginalPath: path})
				require.NoError(t, err)
				if importedIdentity > ordinaryIdentity {
					ordinary.record.ID, run.record.ID = run.record.ID, ordinary.record.ID
				}
				node, err := s.IngestFileExact(ctx, ordinary, s.RootID(), "capture.xmp", fakeHash("c3"), 4, "application/rdf+xml", path, "")
				require.NoError(t, err)
				require.Equal(t, int64(2), node.ID)
			}

			imported, err := s.IngestPhotoGroup(ctx, run, photoImportTestGroup(
				photoImportTestMember(filepath.Join(root, "capture.ARW"), PhotoRoleRAW, fakeHash("a1"), "image/x-sony-arw"),
				photoImportTestMember(filepath.Join(root, "capture.JPG"), PhotoRoleImage, fakeHash("b2"), "image/jpeg"),
				photoImportTestMember(filepath.Join(root, "capture.xmp"), PhotoRoleSidecar, fakeHash("c3"), "application/rdf+xml"),
			))
			require.NoError(t, err)
			asset := imported.Asset
			raw := fileByRole(asset.Files, PhotoRoleRAW)
			jpg := fileByRole(asset.Files, PhotoRoleImage)
			sidecar := fileByRole(asset.Files, PhotoRoleSidecar)
			if role == "ordinary origin" {
				var sourceKind string
				require.NoError(t, s.db.QueryRow(`SELECT i.source_kind FROM photo_files f JOIN provenance p ON p.identity=f.source_provenance_identity JOIN ingests i ON i.id=p.ingest_id WHERE f.file_id=?`, sidecar.ID).Scan(&sourceKind))
				require.Equal(t, "cli", sourceKind)
				require.Equal(t, &raw.ID, sidecar.SidecarOfID)
			}

			var wrong ProvenanceAppendResult
			var node Node
			if role != "ordinary origin" {
				member := fileByRole(asset.Files, role)
				node, err = s.NodeByID(ctx, member.NodeID)
				require.NoError(t, err)
				wrong, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
					NodeID: node.ID, IfRevision: node.Revision, SourceKind: "agent",
					SourceDescription: "caller assertion", OriginalPath: "https://example.com/unrelated",
				})
				require.NoError(t, err)
			}
			canonical := photoCanonical(t,
				photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean()),
			)
			if role == "ordinary origin" {
				canonical = photoCanonical(t,
					photoMetadataField("image.xmp.packet_valid", "image.xmp", "packet", photoBoolean()),
					photoMetadataField("image.xmp.rating", "image.xmp", "Rating", photoInteger(4)),
					photoMetadataField("image.xmp.sidecar_for_extension", "image.xmp", "SidecarForExtension", photoString("JPG")),
				)
			}
			_, err = s.PublishSourceMetadata(ctx, fakeHash("c3"), fakeHash("ee"), canonical)
			require.NoError(t, err)
			targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			if role != "ordinary origin" {
				receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
				require.NoError(t, err)
				assert.Empty(t, receipt.After)
				pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				require.Empty(t, pending)
				_, err = s.AppendNodeProvenance(ctx, ProvenanceAppendInput{
					NodeID: node.ID, IfRevision: wrong.Node.Revision, SourceKind: "agent",
					SourceDescription: "corrected caller assertion", OriginalPath: filepath.Join(root, node.Name), Supersedes: &wrong.Fact.Identity,
				})
				require.NoError(t, err)
				pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				assert.Equal(t, asset.Revision, pending[0].AssetRevision)
				require.NoError(t, s.ValidateMetadata(ctx))
				return
			}
			receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
			require.NoError(t, err)
			require.Len(t, receipt.After, 1)
			assert.Equal(t, jpg.ID, receipt.After[0].FileID)
			assert.Equal(t, 4, receipt.After[0].Values.Rating)
			untouched, err := photoFileByIDQuery(ctx, s.db, raw.ID)
			require.NoError(t, err)
			assert.Zero(t, untouched.Rating)
			assert.Equal(t, int64(1), untouched.Revision)
			assert.Equal(t, sidecar.ID, receipt.Sidecar.FileID)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoSidecarTrashedFallbackSettlesUntilRestore(t *testing.T) {
	t.Parallel()
	for _, audited := range []bool{false, true} {
		t.Run(fmt.Sprintf("audited=%t", audited), func(t *testing.T) {
			s, asset, raw, sidecar := authoredSidecar(t)
			ctx := t.Context()
			if audited {
				seedInitialAuditAuthority(t, s, s.RootID())
			}
			publishPhotoPacket(t, s, sidecar.BlobHash, 4)
			node, err := s.NodeByID(ctx, raw.NodeID)
			require.NoError(t, err)
			trashed, _, err := s.Trash(ctx, node.ID, node.Revision)
			require.NoError(t, err)
			targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
			require.NoError(t, err)
			assert.Empty(t, receipt.After)
			pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
			require.NoError(t, err)
			assert.Empty(t, pending)
			_, _, err = s.Restore(ctx, trashed.ID, trashed.Revision)
			require.NoError(t, err)
			restored, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			require.Greater(t, restored.Revision, asset.Revision)
			pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, restored.Revision, pending[0].AssetRevision)
			receipt, err = s.InitializePhotoSidecar(ctx, pending[0])
			require.NoError(t, err)
			require.Len(t, receipt.After, 1)
			assert.Equal(t, raw.ID, receipt.After[0].FileID)
			assert.Equal(t, 4, receipt.After[0].Values.Rating)
			pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
			require.NoError(t, err)
			assert.Empty(t, pending)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoSidecarSettlesOutsideAuditScopeUntilEnrolled(t *testing.T) {
	t.Parallel()
	s, _, raw, sidecar := authoredSidecar(t)
	ctx := t.Context()
	other, err := s.Mkdir(ctx, s.RootID(), "Other")
	require.NoError(t, err)
	asset, err := s.PhotoAssetForNode(ctx, raw.NodeID)
	require.NoError(t, err)
	for _, f := range asset.Files {
		node, err := s.NodeByID(ctx, f.NodeID)
		require.NoError(t, err)
		_, _, err = s.Move(ctx, node.ID, other.ID, node.Name, node.Revision)
		require.NoError(t, err)
	}
	scope, err := s.Mkdir(ctx, s.RootID(), "Audited")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, scope.ID)
	publishPhotoPacket(t, s, sidecar.BlobHash, 4)
	targets, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	receipt, err := s.InitializePhotoSidecar(ctx, targets[0])
	require.NoError(t, err)
	assert.Empty(t, receipt.After)
	pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	assert.Empty(t, pending)
	current, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), current.Revision)
	assert.Zero(t, current.Rating)
	plan, err := s.PreviewInitialAudit(ctx, other.ID, "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	pending, err = s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 1)
	require.NoError(t, err)
	require.Len(t, pending, 1, "enrolling the photo makes its sidecar eligible again")
	receipt, err = s.InitializePhotoSidecar(ctx, pending[0])
	require.NoError(t, err)
	require.Len(t, receipt.After, 1)
	assert.Equal(t, raw.ID, receipt.After[0].FileID)
	assert.Equal(t, 4, receipt.After[0].Values.Rating)
	require.NoError(t, s.ValidateMetadata(ctx))
}

func TestPhotoAuthoredHiddenEditsRequireUnlock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := authoredPair(t, s)
	visible := albumAsset(t, s, "visible.jpg")
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := WithPhotoHiddenToken(ctx, token)
	t.Run("prepare exact visibility", func(t *testing.T) {
		q, err := query.Parse([]byte(`{}`))
		require.NoError(t, err)
		_, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{Query: &q, Hidden: true})
		require.ErrorIs(t, err, ErrHiddenLocked)
		_, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true, SkipUnavailable: true})
		require.ErrorIs(t, err, ErrHiddenLocked)
		for _, hidden := range []bool{true, false} {
			asset, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, hidden)
			require.NoError(t, err)
			_, err = s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: !hidden})
			require.ErrorIs(t, err, ErrNotFound)
			recovered, err := s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: !hidden, SkipUnavailable: true})
			require.NoError(t, err)
			require.Empty(t, recovered.Targets)
			prepared, err := s.PreparePhotoAuthored(unlocked, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: hidden})
			require.NoError(t, err)
			require.Len(t, prepared.Targets, 2)
			if hidden {
				for _, isHidden := range []bool{true, false} {
					prepared, err := s.PreparePhotoAuthored(unlocked, PhotoSetSelection{Query: &q, Hidden: isHidden})
					require.NoError(t, err)
					require.Len(t, prepared.Assets, 1)
					expected := visible
					if isHidden {
						expected = asset
					}
					require.Equal(t, expected.ID, prepared.Assets[0].ID)
					for _, target := range prepared.Targets {
						require.Equal(t, expected.ID, target.AssetID)
					}
				}
			}
		}
	})
	t.Run("edit and undo require unlock", func(t *testing.T) {
		asset, err = s.SetPhotoAssetHidden(unlocked, asset.ID, asset.Revision, true)
		require.NoError(t, err)
		targets := preparedPhotoTargets(unlocked, t, s, PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true})
		request := PhotoAuthoredBatch{Targets: targets[:1], Patch: PhotoAuthoredPatch{Caption: new("Private caption")}}
		_, err = s.EditPhotoAuthoredBatch(ctx, request)
		require.ErrorIs(t, err, ErrHiddenLocked)
		current, err := photoFileByIDQuery(ctx, s.db, request.Targets[0].FileID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), current.Revision)
		assert.Empty(t, current.Caption)
		receipt, err := s.EditPhotoAuthoredBatch(unlocked, request)
		require.NoError(t, err)
		_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
		require.ErrorIs(t, err, ErrHiddenLocked)
		_, err = s.UndoPhotoAuthored(unlocked, receipt.ReceiptID)
		require.NoError(t, err)
		current, err = photoFileByIDQuery(ctx, s.db, request.Targets[0].FileID)
		require.NoError(t, err)
		assert.Equal(t, int64(3), current.Revision)
		assert.Empty(t, current.Caption)
	})
}

func TestPhotoAuthoredRestoreRejectsDecidedRevisionOne(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	node, err := s.CreateFile(ctx, s.RootID(), "photo.jpg", fakeHash("a1"), 4, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	for _, c := range []struct {
		revision int64
		rating   int
		valid    bool
	}{{1, 4, false}, {1, 0, false}, {2, 4, true}} {
		t.Run(fmt.Sprintf("revision=%d rating=%d", c.revision, c.rating), func(t *testing.T) {
			lines := bytes.Split(backup.Bytes(), []byte{'\n'})
			for i, line := range lines {
				if bytes.Contains(line, []byte(`"type":"photo_file"`)) {
					var record metadataPhotoFile
					require.NoError(t, json.Unmarshal(line, &record))
					record.Revision, record.Rating, record.Confirmed = c.revision, c.rating, PhotoConfirmedRating
					lines[i], err = json.Marshal(record, json.Deterministic(true))
					require.NoError(t, err)
				}
			}
			restored, err := Open(filepath.Join(t.TempDir(), "restored.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, restored.Close()) })
			err = restored.ImportMetadata(ctx, bytes.NewReader(bytes.Join(lines, []byte{'\n'})))
			if c.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "photo file at revision 1 has authored decisions")
			}
		})
	}
}

func TestPhotoSidecarLowerSiblingWinsDespiteExtractionAndCursor(t *testing.T) {
	t.Parallel()
	s, asset, raw, lower := authoredSidecar(t)
	ctx := t.Context()
	unrelated := singlePhotoSidecar(t, s, "other", fakeHash("e5"), fakeHash("e6"))
	higher, err := s.CreateFile(ctx, s.RootID(), "another.xmp", fakeHash("d4"), 4, "application/rdf+xml")
	require.NoError(t, err)
	require.Less(t, lower.ID, unrelated.ID)
	require.Less(t, unrelated.ID, higher.ID)
	current, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, asset.ID, current.Revision, higher.ID, PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	publishPhotoPacket(t, s, unrelated.BlobHash, 1)
	publishPhotoPacket(t, s, higher.BlobHash, 2)
	page, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
	require.NoError(t, err)
	require.Len(t, page, 1, "the higher-ID packet waits while the lower-ID packet is unextracted")
	require.Equal(t, unrelated.ID, page[0].NodeID)
	_, err = s.InitializePhotoSidecar(ctx, page[0])
	require.NoError(t, err)
	publishPhotoPacket(t, s, lower.BlobHash, 5)
	afterCursor, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), strconv.FormatInt(unrelated.ID, 10), 10)
	require.NoError(t, err)
	assert.Empty(t, afterCursor, "an extracted but unsettled lower-ID sibling still holds back the higher-ID packet")
	for _, want := range []struct {
		nodeID  int64
		applied bool
	}{{lower.ID, true}, {higher.ID, false}} {
		pending, err := s.MissingPhotoSidecarsAfter(ctx, fakeHash("ee"), "", 10)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, want.nodeID, pending[0].NodeID)
		receipt, err := s.InitializePhotoSidecar(ctx, pending[0])
		require.NoError(t, err)
		assert.Equal(t, want.applied, len(receipt.After) == 1)
	}
	file, err := photoFileByIDQuery(ctx, s.db, raw.ID)
	require.NoError(t, err)
	assert.Equal(t, 5, file.Rating)
}

func TestPhotoAuthoredMultibyteTextBoundary(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("界", MaxPhotoAuthoredTextBytes/3)
	require.NoError(t, ValidatePhotoAuthored(PhotoAuthored{Confirmed: PhotoConfirmedCaption, Caption: text + "x"}))
	err := ValidatePhotoAuthored(PhotoAuthored{Confirmed: PhotoConfirmedCaption, Caption: text + "xx"})
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	require.ErrorContains(t, err, fmt.Sprintf("caption exceeds %d UTF-8 bytes", MaxPhotoAuthoredTextBytes))
}

func editPhotoAuthored(ctx context.Context, s *Store, targets []PhotoAuthoredTarget) (PhotoAuthoredReceipt, error) {
	request := PhotoAuthoredBatch{Patch: targets[0].Patch}
	for _, target := range targets {
		if !reflect.DeepEqual(target.Patch, request.Patch) {
			return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
		}
		file, err := photoFileByIDQuery(ctx, s.db, target.FileID)
		if err != nil {
			return PhotoAuthoredReceipt{}, err
		}
		asset, err := s.PhotoAssetByID(ctx, file.AssetID)
		if err != nil {
			return PhotoAuthoredReceipt{}, err
		}
		node, err := s.NodeByID(ctx, file.NodeID)
		if err != nil {
			return PhotoAuthoredReceipt{}, err
		}
		request.Targets = append(request.Targets, PhotoAuthoredEditTarget{AssetID: file.AssetID, AssetRevision: asset.Revision, FileID: file.ID, Revision: target.Revision, NodeRevision: node.Revision})
	}
	return s.EditPhotoAuthoredBatch(ctx, request)
}
