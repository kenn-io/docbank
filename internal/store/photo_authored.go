package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/query"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxPhotoAuthoredTextBytes = 16 << 10

// PhotoAuthoredFields is a bitmask of authored fields that hold a confirmed decision.
// The bit values are persisted in photo_files, backups, and audit records, so they never change.
type PhotoAuthoredFields uint8

const (
	PhotoConfirmedRating PhotoAuthoredFields = 1 << iota
	PhotoConfirmedFlag
	PhotoConfirmedLabel
	PhotoConfirmedCaption
	PhotoConfirmedCreator
	PhotoConfirmedCopyright
	PhotoConfirmedRotation
)

// PhotoConfirmedAll is the union of every supported confirmation bit.
const PhotoConfirmedAll = PhotoConfirmedRating | PhotoConfirmedFlag | PhotoConfirmedLabel |
	PhotoConfirmedCaption | PhotoConfirmedCreator | PhotoConfirmedCopyright | PhotoConfirmedRotation

// Valid reports whether all confirmation bits are supported.
func (f PhotoAuthoredFields) Valid() bool {
	return f & ^PhotoConfirmedAll == 0
}

// Names lists the confirmed fields in their stable display order.
func (f PhotoAuthoredFields) Names() string {
	var names []string
	for _, field := range photoAuthoredFields {
		if f&field.bit != 0 {
			names = append(names, field.name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

type PhotoAuthored struct {
	Confirmed PhotoAuthoredFields `json:"confirmed_fields,omitzero" maximum:"127" doc:"Confirmation mask. See PhotoFile.confirmed_fields for supported bits."`
	Rating    int                 `json:"rating" minimum:"0" maximum:"5"`
	Flag      string              `json:"flag" enum:",pick,reject"`
	Label     string              `json:"label" enum:",red,yellow,green,blue,purple"`
	Caption   string              `json:"caption" maxLength:"16384" doc:"At most 16384 UTF-8 bytes."`
	Creator   string              `json:"creator" maxLength:"16384" doc:"At most 16384 UTF-8 bytes."`
	Copyright string              `json:"copyright" maxLength:"16384" doc:"At most 16384 UTF-8 bytes."`
	Rotation  int                 `json:"rotation" enum:"0,90,180,270"`
}

// ValidatePhotoAuthored checks stored authored values, including that every non-default value is
// confirmed.
func ValidatePhotoAuthored(v PhotoAuthored) error {
	if !v.Confirmed.Valid() {
		return fmt.Errorf("%w: invalid authored photo decision", ErrInvalidPhotoAsset)
	}
	for _, field := range photoAuthoredFields {
		if v.Confirmed&field.bit == 0 && field.nonDefault(&v) {
			return fmt.Errorf("%w: invalid authored photo decision: unconfirmed %s", ErrInvalidPhotoAsset, field.name)
		}
	}
	return ValidatePhotoAuthoredValues(v)
}

// ValidatePhotoAuthoredValues checks value ranges and text limits only. It ignores Confirmed, so
// parsers can use it before any decision exists.
func ValidatePhotoAuthoredValues(v PhotoAuthored) error {
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
	return PhotoAuthored{Confirmed: f.Confirmed, Rating: f.Rating, Flag: f.Flag, Label: f.Label, Caption: f.Caption, Creator: f.Creator, Copyright: f.Copyright, Rotation: f.Rotation}
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

type photoAuthoredField struct {
	name       string
	bit        PhotoAuthoredFields
	nonDefault func(*PhotoAuthored) bool
	equal      func(*PhotoAuthored, *PhotoAuthored) bool
	setPatch   func(*PhotoAuthoredPatch, *PhotoAuthored)
	apply      func(PhotoAuthoredPatch, *PhotoAuthored) bool
}

func authoredField[T comparable](name string, bit PhotoAuthoredFields, value func(*PhotoAuthored) *T, patch func(*PhotoAuthoredPatch) **T) photoAuthoredField {
	var zero T
	return photoAuthoredField{
		name: name, bit: bit,
		nonDefault: func(v *PhotoAuthored) bool { return *value(v) != zero },
		equal:      func(a, b *PhotoAuthored) bool { return *value(a) == *value(b) },
		setPatch:   func(p *PhotoAuthoredPatch, v *PhotoAuthored) { *patch(p) = value(v) },
		apply: func(p PhotoAuthoredPatch, v *PhotoAuthored) bool {
			if supplied := *patch(&p); supplied != nil {
				*value(v) = *supplied
				return true
			}
			return false
		},
	}
}

var photoAuthoredFields = [...]photoAuthoredField{
	authoredField("rating", PhotoConfirmedRating,
		func(v *PhotoAuthored) *int { return &v.Rating }, func(p *PhotoAuthoredPatch) **int { return &p.Rating }),
	authoredField("flag", PhotoConfirmedFlag,
		func(v *PhotoAuthored) *string { return &v.Flag }, func(p *PhotoAuthoredPatch) **string { return &p.Flag }),
	authoredField("label", PhotoConfirmedLabel,
		func(v *PhotoAuthored) *string { return &v.Label }, func(p *PhotoAuthoredPatch) **string { return &p.Label }),
	authoredField("caption", PhotoConfirmedCaption,
		func(v *PhotoAuthored) *string { return &v.Caption }, func(p *PhotoAuthoredPatch) **string { return &p.Caption }),
	authoredField("creator", PhotoConfirmedCreator,
		func(v *PhotoAuthored) *string { return &v.Creator }, func(p *PhotoAuthoredPatch) **string { return &p.Creator }),
	authoredField("copyright", PhotoConfirmedCopyright,
		func(v *PhotoAuthored) *string { return &v.Copyright }, func(p *PhotoAuthoredPatch) **string { return &p.Copyright }),
	authoredField("rotation", PhotoConfirmedRotation,
		func(v *PhotoAuthored) *int { return &v.Rotation }, func(p *PhotoAuthoredPatch) **int { return &p.Rotation }),
}

func (p PhotoAuthoredPatch) apply(v PhotoAuthored) PhotoAuthored {
	for _, field := range photoAuthoredFields {
		if field.apply(p, &v) {
			v.Confirmed |= field.bit
		}
	}
	return v
}

type PhotoAuthoredTarget struct {
	FileID   string
	Revision int64
	Patch    PhotoAuthoredPatch
	// restore replaces the values and confirmation mask instead of applying Patch; undo uses it.
	restore *PhotoAuthored
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
	if err := validatePhotoAuthoredReceipt(r, id); err != nil {
		return r, err
	}
	canonicalBefore, err := json.Marshal(r.Before, json.Deterministic(true))
	if err != nil {
		return r, err
	}
	canonicalAfter, err := json.Marshal(r.afterState(), json.Deterministic(true))
	if err != nil {
		return r, err
	}
	values := []jsontext.Value{jsontext.Value(beforeJSON).Clone(), jsontext.Value(afterJSON).Clone(), canonicalBefore, canonicalAfter}
	for i := range values {
		if err := values[i].Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
			return r, err
		}
	}
	if !bytes.Equal(values[0], values[2]) || !bytes.Equal(values[1], values[3]) {
		return r, ErrInvalidPhotoAsset
	}
	return r, nil
}

func photoAgreement(files []PhotoFile) map[string]bool {
	result := make(map[string]bool, len(photoAuthoredFields))
	for _, field := range photoAuthoredFields {
		result[field.name] = true
	}
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
		for _, field := range photoAuthoredFields {
			result[field.name] = result[field.name] && field.equal(&v, first)
		}
	}
	return result
}

func photoFileByIDQuery(ctx context.Context, q metadataQuerier, id string) (PhotoFile, error) {
	f, err := scanPhotoFile(q.QueryRowContext(ctx, `SELECT file_id,COALESCE(asset_id,''),node_id,role,sidecar_of_file_id,created_at,revision,rating,flag,label,caption,creator,copyright,rotation,confirmed_fields FROM photo_files WHERE file_id=?`, id))
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
		if sidecar == nil && f.AssetID != "" {
			if err := s.photoAuthoredAccessTx(ctx, tx, f.AssetID); err != nil {
				return result, err
			}
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
		var v PhotoAuthored
		if t.restore != nil {
			v = *t.restore
		} else {
			v = t.Patch.apply(f.Authored())
		}
		if err := ValidatePhotoAuthored(v); err != nil {
			return result, err
		}
		if v == f.Authored() && !force {
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
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET revision=?,rating=?,flag=?,label=?,caption=?,creator=?,copyright=?,rotation=?,confirmed_fields=? WHERE file_id=?`, a.Revision, v.Rating, v.Flag, v.Label, v.Caption, v.Creator, v.Copyright, v.Rotation, v.Confirmed, a.FileID); err != nil {
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

// photoAuthoredAccessTx keeps a person's edits and undo of hidden photos behind the unlock session.
func (s *Store) photoAuthoredAccessTx(ctx context.Context, tx *sql.Tx, assetID string) error {
	var hidden bool
	if err := tx.QueryRowContext(ctx, `SELECT hidden_at IS NOT NULL FROM photo_assets WHERE asset_id=?`, assetID).Scan(&hidden); err != nil {
		return fmt.Errorf("reading hidden state of photo asset %s: %w", assetID, err)
	}
	if !hidden {
		return nil
	}
	_, err := s.hiddenSession(ctx, tx)
	return err
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
			targets[i] = PhotoAuthoredTarget{FileID: a.FileID, Revision: a.Revision, restore: &receipt.Before[i].Values}
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
	if !slices.Contains([]string{"authored", "authored_undo", "authored_sidecar"}, operation) {
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
