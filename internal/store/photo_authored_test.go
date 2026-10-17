package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		cases = append(cases, testCase{name: mode, receipt: receipt, valid: mode == "keyword" || mode == "sidecar"})
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
		for i := range 40 {
			node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("photo-%d.jpg", i), fakeHash("b2"), 4, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			f := asset.Files[0]
			targets = append(targets, PhotoAuthoredTarget{FileID: f.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(4)}})
		}
		receipt, err := editPhotoAuthored(ctx, s, targets)
		require.NoError(t, err)
		require.Len(t, receipt.After, 40)
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
				receipt = importXMP(t, s, f.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
				missingFile = receipt.Sidecar.FileID
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
		s, asset, raw, sidecar := authoredSidecar(t)
		receipt := importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
		asset, err := s.DetachPhotoFile(ctx, asset.ID, asset.Revision, receipt.Sidecar.FileID, PhotoDetachOptions{})
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
		request.Targets = append(request.Targets, PhotoAuthoredEditTarget{file.AssetID, asset.Revision, file.ID, target.Revision, node.Revision})
	}
	return s.EditPhotoAuthoredBatch(ctx, request)
}
