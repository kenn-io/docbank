package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/query"
	"math"
	"slices"
	"unicode/utf8"
)

const MaxPhotoAuthoredTextBytes = 16 << 10

type PhotoAuthored struct {
	Rating    int    `json:"rating"`
	Flag      string `json:"flag"`
	Label     string `json:"label"`
	Caption   string `json:"caption"`
	Creator   string `json:"creator"`
	Copyright string `json:"copyright"`
	Rotation  int    `json:"rotation"`
}

func ValidatePhotoAuthored(v PhotoAuthored) error {
	if v.Rating < 0 || v.Rating > 5 || !query.ValidPhotoFlag(v.Flag) || !query.ValidPhotoColorLabel(v.Label) || !slices.Contains([]int{0, 90, 180, 270}, v.Rotation) {
		return fmt.Errorf("%w: invalid authored photo decision", ErrInvalidPhotoAsset)
	}
	for _, text := range []string{v.Caption, v.Creator, v.Copyright} {
		if len(text) > MaxPhotoAuthoredTextBytes || !utf8.ValidString(text) || slices.Contains([]byte(text), byte(0)) {
			return fmt.Errorf("%w: invalid authored photo text", ErrInvalidPhotoAsset)
		}
	}
	return nil
}

func (f PhotoFile) Authored() PhotoAuthored {
	return PhotoAuthored{f.Rating, f.Flag, f.Label, f.Caption, f.Creator, f.Copyright, f.Rotation}
}

type PhotoAuthoredPatch struct {
	Rating    *int    `json:"rating,omitzero"`
	Flag      *string `json:"flag,omitzero"`
	Label     *string `json:"label,omitzero"`
	Caption   *string `json:"caption,omitzero"`
	Creator   *string `json:"creator,omitzero"`
	Copyright *string `json:"copyright,omitzero"`
	Rotation  *int    `json:"rotation,omitzero"`
}

func (p PhotoAuthoredPatch) apply(v PhotoAuthored) PhotoAuthored {
	if p.Rating != nil {
		v.Rating = *p.Rating
	}
	if p.Flag != nil {
		v.Flag = *p.Flag
	}
	if p.Label != nil {
		v.Label = *p.Label
	}
	if p.Caption != nil {
		v.Caption = *p.Caption
	}
	if p.Creator != nil {
		v.Creator = *p.Creator
	}
	if p.Copyright != nil {
		v.Copyright = *p.Copyright
	}
	if p.Rotation != nil {
		v.Rotation = *p.Rotation
	}
	return v
}

type PhotoAuthoredTarget struct {
	FileID   string
	Revision int64
	Patch    PhotoAuthoredPatch
}
type PhotoAuthoredSnapshot struct {
	FileID   string        `json:"file_id"`
	NodeID   int64         `json:"node_id"`
	Revision int64         `json:"revision"`
	Values   PhotoAuthored `json:"values"`
}
type PhotoSidecarProvenance struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	FileID    string `json:"file_id"`
}
type PhotoAuthoredReceipt struct {
	ReceiptID string                  `json:"receipt_id"`
	Before    []PhotoAuthoredSnapshot `json:"before"`
	After     []PhotoAuthoredSnapshot `json:"after"`
	UndoOf    string                  `json:"undo_of,omitzero"`
	Sidecar   *PhotoSidecarProvenance `json:"sidecar,omitzero"`
}

type photoAuthoredReceiptAfter struct {
	ReceiptID string                  `json:"receipt_id"`
	After     []PhotoAuthoredSnapshot `json:"after"`
	UndoOf    string                  `json:"undo_of,omitzero"`
	Sidecar   *PhotoSidecarProvenance `json:"sidecar,omitzero"`
}

func (r PhotoAuthoredReceipt) afterState() photoAuthoredReceiptAfter {
	return photoAuthoredReceiptAfter{r.ReceiptID, r.After, r.UndoOf, r.Sidecar}
}

func checkPhotoAuthoredReceiptSize(r PhotoAuthoredReceipt) error {
	before, after := slices.Clone(r.Before), slices.Clone(r.After)
	for i := range before {
		before[i].Revision = math.MaxInt64
		after[i].Revision = math.MaxInt64
	}
	const id = "00000000-0000-4000-8000-000000000000"
	// Reserve revision digits and receipt references equally for an edit and its inverse.
	state := photoAuthoredReceiptAfter{id, after, id, &PhotoSidecarProvenance{math.MaxInt64, id, id}}
	b, err := json.Marshal(before, json.Deterministic(true))
	if err != nil {
		return err
	}
	a, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return err
	}
	if len(b)+len(a) > maxBatchTagReceiptJSONBytes {
		return ErrInvalidPhotoAsset
	}
	return nil
}

func decodePhotoAuthoredReceipt(beforeJSON, afterJSON []byte, id string) (PhotoAuthoredReceipt, error) {
	var r PhotoAuthoredReceipt
	if len(beforeJSON)+len(afterJSON) > maxBatchTagReceiptJSONBytes {
		return r, ErrInvalidPhotoAsset
	}
	var after photoAuthoredReceiptAfter
	if err := json.Unmarshal(afterJSON, &after, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	r = PhotoAuthoredReceipt{ReceiptID: after.ReceiptID, After: after.After, UndoOf: after.UndoOf, Sidecar: after.Sidecar}
	if err := json.Unmarshal(beforeJSON, &r.Before, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	return r, validatePhotoAuthoredReceipt(r, id)
}

func photoAgreement(files []PhotoFile) map[string]bool {
	result := map[string]bool{"rating": true, "flag": true, "label": true, "caption": true, "creator": true, "copyright": true, "rotation": true}
	var first *PhotoAuthored
	for _, f := range files {
		if f.Role == PhotoRoleSidecar {
			continue
		}
		v := f.Authored()
		if first == nil {
			first = &v
			continue
		}
		result["rating"] = result["rating"] && v.Rating == first.Rating
		result["flag"] = result["flag"] && v.Flag == first.Flag
		result["label"] = result["label"] && v.Label == first.Label
		result["caption"] = result["caption"] && v.Caption == first.Caption
		result["creator"] = result["creator"] && v.Creator == first.Creator
		result["copyright"] = result["copyright"] && v.Copyright == first.Copyright
		result["rotation"] = result["rotation"] && v.Rotation == first.Rotation
	}
	return result
}

func photoFileByIDQuery(ctx context.Context, q metadataQuerier, id string) (PhotoFile, error) {
	var f PhotoFile
	err := q.QueryRowContext(ctx, `SELECT file_id,COALESCE(asset_id,''),node_id,role,sidecar_of_file_id,created_at,revision,rating,flag,label,caption,creator,copyright,rotation FROM photo_files WHERE file_id=?`, id).Scan(&f.ID, &f.AssetID, &f.NodeID, &f.Role, &f.SidecarOfID, &f.CreatedAt, &f.Revision, &f.Rating, &f.Flag, &f.Label, &f.Caption, &f.Creator, &f.Copyright, &f.Rotation)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

func (s *Store) EditPhotoAuthored(ctx context.Context, targets []PhotoAuthoredTarget) (PhotoAuthoredReceipt, error) {
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.applyPhotoAuthoredTx(ctx, tx, targets, "", nil, false)
		return err
	})
	return result, err
}

func (s *Store) EditPhotoPair(ctx context.Context, assetID string, revision int64, targets []PhotoAuthoredTarget) (PhotoAuthoredReceipt, error) {
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		asset, err := s.photoAssetForMutationTx(ctx, tx, assetID, revision)
		if err != nil {
			return err
		}
		ids := map[string]bool{}
		for _, t := range targets {
			ids[t.FileID] = true
		}
		count := 0
		for _, f := range asset.Files {
			if f.Role != PhotoRoleSidecar {
				count++
				if !ids[f.ID] {
					return ErrStaleRevision
				}
			}
		}
		if count != len(targets) {
			return ErrInvalidPhotoAsset
		}
		result, err = s.applyPhotoAuthoredTx(ctx, tx, targets, "", nil, true)
		return err
	})
	return result, err
}

func (s *Store) applyPhotoAuthoredTx(ctx context.Context, tx *sql.Tx, targets []PhotoAuthoredTarget, undoOf string, sidecar *PhotoSidecarProvenance, force bool) (PhotoAuthoredReceipt, error) {
	result := PhotoAuthoredReceipt{UndoOf: undoOf, Sidecar: sidecar, Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}
	if len(targets) < 1 || len(targets) > maxBatchTagTargets {
		return result, ErrInvalidPhotoAsset
	}
	targets = slices.Clone(targets)
	slices.SortFunc(targets, func(a, b PhotoAuthoredTarget) int {
		if a.FileID < b.FileID {
			return -1
		}
		if a.FileID > b.FileID {
			return 1
		}
		return 0
	})
	nodes := []Node{}
	for i, t := range targets {
		if validateUUIDv4(t.FileID) != nil || t.Revision < 1 || i > 0 && targets[i-1].FileID == t.FileID {
			return result, ErrInvalidPhotoAsset
		}
		f, err := photoFileByIDQuery(ctx, tx, t.FileID)
		if err != nil {
			return result, err
		}
		if f.Revision != t.Revision {
			return result, ErrStaleRevision
		}
		if f.Role == PhotoRoleSidecar {
			return result, ErrInvalidPhotoAsset
		}
		node, err := nodeByIDQuery(ctx, tx, f.NodeID)
		if err != nil {
			return result, err
		}
		if node.TrashedAt != nil {
			return result, ErrNotFound
		}
		v := t.Patch.apply(f.Authored())
		if err := ValidatePhotoAuthored(v); err != nil {
			return result, err
		}
		// Revision one is still eligible for sidecar initialization until a person makes a decision.
		claim := sidecar == nil && f.Revision == 1 && t.Patch != (PhotoAuthoredPatch{})
		if v == f.Authored() && !force && !claim {
			continue
		}
		if f.Revision == math.MaxInt64 || node.Revision == math.MaxInt64 {
			return result, ErrInvalidPhotoAsset
		}
		before := PhotoAuthoredSnapshot{f.ID, f.NodeID, f.Revision, f.Authored()}
		after := before
		after.Revision++
		after.Values = v
		result.Before = append(result.Before, before)
		result.After = append(result.After, after)
		nodes = append(nodes, node)
	}
	if len(result.After) == 0 {
		return result, nil
	}
	id, err := newUUIDv4()
	if err != nil {
		return result, err
	}
	result.ReceiptID = id
	if err := checkPhotoAuthoredReceiptSize(result); err != nil {
		return result, err
	}
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return result, err
	}
	now := nowRFC3339()
	for i, a := range result.After {
		v := a.Values
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET revision=?,rating=?,flag=?,label=?,caption=?,creator=?,copyright=?,rotation=? WHERE file_id=?`, a.Revision, v.Rating, v.Flag, v.Label, v.Caption, v.Creator, v.Copyright, v.Rotation, a.FileID); err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET revision=revision+1,modified_at=? WHERE id=?`, now, a.NodeID); err != nil {
			return result, err
		}
		if active {
			if err := s.persistPhotoAuthoredAuditTx(ctx, tx, nodes[i], result.Before[i], a, now); err != nil {
				return result, err
			}
		}
	}
	operation := "authored"
	if undoOf != "" {
		operation = "authored_undo"
	}
	if sidecar != nil {
		operation = "authored_sidecar"
	}
	err = writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: id, Operation: operation, BeforeRevision: 0, AfterRevision: 1, Before: result.Before, After: result.afterState()})
	return result, err
}

func (s *Store) UndoPhotoAuthored(ctx context.Context, id string) (PhotoAuthoredReceipt, error) {
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		receipt, err := loadPhotoAuthoredReceipt(ctx, tx, id)
		if err != nil {
			return err
		}
		targets := make([]PhotoAuthoredTarget, len(receipt.After))
		for i, a := range receipt.After {
			v := receipt.Before[i].Values
			targets[i] = PhotoAuthoredTarget{a.FileID, a.Revision, PhotoAuthoredPatch{&v.Rating, &v.Flag, &v.Label, &v.Caption, &v.Creator, &v.Copyright, &v.Rotation}}
		}
		result, err = s.applyPhotoAuthoredTx(ctx, tx, targets, id, nil, true)
		return err
	})
	return result, err
}

func loadPhotoAuthoredReceipt(ctx context.Context, q metadataQuerier, id string) (PhotoAuthoredReceipt, error) {
	var encoded []byte
	var before []byte
	var operation string
	err := q.QueryRowContext(ctx, `SELECT operation,before_json,after_json FROM photo_change_receipts WHERE receipt_id=?`, id).Scan(&operation, &before, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoAuthoredReceipt{}, ErrNotFound
	}
	if err != nil {
		return PhotoAuthoredReceipt{}, err
	}
	if !slices.Contains([]string{"authored", "authored_undo", "authored_sidecar"}, operation) || len(encoded) > maxBatchTagReceiptJSONBytes {
		return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
	}
	return decodePhotoAuthoredReceipt(before, encoded, id)
}

func validatePhotoAuthoredReceipt(r PhotoAuthoredReceipt, id string) error {
	if r.ReceiptID != id || validateUUIDv4(id) != nil || len(r.Before) < 1 || len(r.Before) > maxBatchTagTargets || len(r.Before) != len(r.After) || r.UndoOf != "" && validateUUIDv4(r.UndoOf) != nil {
		return ErrInvalidPhotoAsset
	}
	if r.Sidecar != nil && (r.Sidecar.NodeID < 1 || validateUUIDv4(r.Sidecar.VersionID) != nil || validateUUIDv4(r.Sidecar.FileID) != nil || r.UndoOf != "") {
		return ErrInvalidPhotoAsset
	}
	for i, b := range r.Before {
		a := r.After[i]
		if validateUUIDv4(b.FileID) != nil || b.NodeID < 1 || b.Revision < 1 || b.Revision == math.MaxInt64 || a.FileID != b.FileID || a.NodeID != b.NodeID || a.Revision != b.Revision+1 || i > 0 && r.Before[i-1].FileID >= b.FileID {
			return ErrInvalidPhotoAsset
		}
		if err := ValidatePhotoAuthored(b.Values); err != nil {
			return err
		}
		if err := ValidatePhotoAuthored(a.Values); err != nil {
			return err
		}
	}
	return checkPhotoAuthoredReceiptSize(r)
}
