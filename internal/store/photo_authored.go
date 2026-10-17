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
	Caption   string              `json:"caption" doc:"At most 16384 UTF-8 bytes."`
	Creator   string              `json:"creator" doc:"At most 16384 UTF-8 bytes."`
	Copyright string              `json:"copyright" doc:"At most 16384 UTF-8 bytes."`
	Rotation  int                 `json:"rotation" enum:"0,90,180,270" doc:"Clockwise degrees on EXIF-normalized pixels: 0, 90, 180, or 270."`
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
	for _, field := range []struct{ name, text string }{{"caption", v.Caption}, {"creator", v.Creator}, {"copyright", v.Copyright}} {
		if len(field.text) > MaxPhotoAuthoredTextBytes {
			return fmt.Errorf("%w: %s exceeds %d UTF-8 bytes", ErrInvalidPhotoAsset, field.name, MaxPhotoAuthoredTextBytes)
		}
		if !utf8.ValidString(field.text) || slices.Contains([]byte(field.text), byte(0)) {
			return fmt.Errorf("%w: invalid authored photo text", ErrInvalidPhotoAsset)
		}
	}
	return nil
}

func (f PhotoFile) Authored() PhotoAuthored {
	return PhotoAuthored{Confirmed: f.Confirmed, Rating: f.Rating, Flag: f.Flag, Label: f.Label, Caption: f.Caption, Creator: f.Creator, Copyright: f.Copyright, Rotation: f.Rotation}
}

type PhotoAuthoredPatch struct {
	Rating    *int    `json:"rating,omitzero" minimum:"0" maximum:"5"`
	Flag      *string `json:"flag,omitzero" enum:",pick,reject"`
	Label     *string `json:"label,omitzero" enum:",red,yellow,green,blue,purple"`
	Caption   *string `json:"caption,omitzero" doc:"At most 16384 UTF-8 bytes."`
	Creator   *string `json:"creator,omitzero" doc:"At most 16384 UTF-8 bytes."`
	Copyright *string `json:"copyright,omitzero" doc:"At most 16384 UTF-8 bytes."`
	Rotation  *int    `json:"rotation,omitzero" enum:"0,90,180,270" doc:"Clockwise degrees on EXIF-normalized pixels: 0, 90, 180, or 270."`
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

// PhotoRepresentativeChange records one asset's burst representative rank.
// The asset revision guards undo; a nil rank is no explicit choice.
type PhotoRepresentativeChange struct {
	AssetID        string `json:"asset_id" format:"uuid"`
	BeforeRevision int64  `json:"before_revision" minimum:"1"`
	AfterRevision  int64  `json:"after_revision" minimum:"2"`
	Before         *int64 `json:"before,omitzero" minimum:"1"`
	After          *int64 `json:"after,omitzero" minimum:"1"`
}

type PhotoAuthoredReceipt struct {
	ReceiptID       string                      `json:"receipt_id" doc:"Empty for a no-op, with empty before and after arrays; no receipt is created."`
	Before          []PhotoAuthoredSnapshot     `json:"before"`
	After           []PhotoAuthoredSnapshot     `json:"after"`
	UndoOf          string                      `json:"undo_of,omitzero"`
	Sidecar         *PhotoSidecarProvenance     `json:"sidecar,omitzero"`
	Keywords        []PhotoKeywordDelta         `json:"keywords,omitzero"`
	KeywordNodes    []PhotoKeywordNode          `json:"keyword_nodes,omitzero"`
	Representatives []PhotoRepresentativeChange `json:"representatives,omitzero"`
}

type photoAuthoredReceiptAfter struct {
	ReceiptID       string                      `json:"receipt_id"`
	After           []PhotoAuthoredSnapshot     `json:"after"`
	UndoOf          string                      `json:"undo_of,omitzero"`
	Sidecar         *PhotoSidecarProvenance     `json:"sidecar,omitzero"`
	Keywords        []PhotoKeywordDelta         `json:"keywords,omitzero"`
	KeywordNodes    []PhotoKeywordNode          `json:"keyword_nodes,omitzero"`
	Representatives []PhotoRepresentativeChange `json:"representatives,omitzero"`
}

func (r PhotoAuthoredReceipt) afterState() photoAuthoredReceiptAfter {
	return photoAuthoredReceiptAfter{r.ReceiptID, r.After, r.UndoOf, r.Sidecar, r.Keywords, r.KeywordNodes, r.Representatives}
}

const maxPhotoAuthoredReceiptJSONBytes = 2 << 20

func checkPhotoAuthoredReceiptSize(r PhotoAuthoredReceipt) error {
	before, after := slices.Clone(r.Before), slices.Clone(r.After)
	for i := range before {
		before[i].Revision = math.MaxInt64
		after[i].Revision = math.MaxInt64
	}
	const id = "00000000-0000-4000-8000-000000000000"
	// Reserve revision digits and receipt references equally for an edit and its inverse.
	state := photoAuthoredReceiptAfter{id, after, id, &PhotoSidecarProvenance{math.MaxInt64, id, id}, r.Keywords, slices.Clone(r.KeywordNodes), slices.Clone(r.Representatives)}
	for i := range state.KeywordNodes {
		state.KeywordNodes[i].AfterRevision = math.MaxInt64
	}
	rank := int64(math.MaxInt64)
	for i := range state.Representatives {
		state.Representatives[i] = PhotoRepresentativeChange{r.Representatives[i].AssetID, math.MaxInt64, math.MaxInt64, &rank, &rank}
	}
	b, err := json.Marshal(before, json.Deterministic(true))
	if err != nil {
		return err
	}
	a, err := json.Marshal(state, json.Deterministic(true))
	if err != nil {
		return err
	}
	if len(b)+len(a) > maxPhotoAuthoredReceiptJSONBytes {
		return ErrPhotoEditReceiptLimit
	}
	return nil
}

func decodePhotoAuthoredReceipt(beforeJSON, afterJSON []byte, id string) (PhotoAuthoredReceipt, error) {
	var r PhotoAuthoredReceipt
	if len(beforeJSON)+len(afterJSON) > maxPhotoAuthoredReceiptJSONBytes {
		return r, ErrInvalidPhotoAsset
	}
	var after photoAuthoredReceiptAfter
	if err := json.Unmarshal(afterJSON, &after, json.RejectUnknownMembers(true)); err != nil {
		return r, err
	}
	r = PhotoAuthoredReceipt{ReceiptID: after.ReceiptID, After: after.After, UndoOf: after.UndoOf, Sidecar: after.Sidecar, Keywords: after.Keywords, KeywordNodes: after.KeywordNodes, Representatives: after.Representatives}
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

// photoFileLookupColumns matches scanPhotoFile and reads a file without an asset as an empty asset ID.
const photoFileLookupColumns = `file_id,COALESCE(asset_id,''),node_id,role,sidecar_of_file_id,created_at,revision,rating,flag,label,caption,creator,copyright,rotation,confirmed_fields`

func photoFileByIDQuery(ctx context.Context, q metadataQuerier, id string) (PhotoFile, error) {
	f, err := scanPhotoFile(q.QueryRowContext(ctx, `SELECT `+photoFileLookupColumns+` FROM photo_files WHERE file_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// photoAuthoredApply configures applyPhotoAuthoredValuesTx.
type photoAuthoredApply struct {
	undoOf  string
	sidecar *PhotoSidecarProvenance
	// force keeps unchanged targets in the receipt and advances the revision of
	// every target whose asset has another changed target.
	force bool
	// persist drops no-op receipts, checks the receipt size, and writes the receipt.
	persist bool
	// loaded supplies already-checked files and nodes. Files read from the
	// database are added to it, and written node revisions are refreshed in it.
	loaded map[string]photoAuthoredLoaded
}

// applyPhotoAuthoredTx applies targets and persists the receipt.
func (s *Store) applyPhotoAuthoredTx(ctx context.Context, tx *sql.Tx, targets []PhotoAuthoredTarget, opts photoAuthoredApply) (PhotoAuthoredReceipt, error) {
	if len(targets) > maxBatchTagTargets {
		return PhotoAuthoredReceipt{}, ErrInvalidPhotoAsset
	}
	opts.persist = true
	return s.applyPhotoAuthoredValuesTx(ctx, tx, targets, opts)
}

func (s *Store) applyPhotoAuthoredValuesTx(ctx context.Context, tx *sql.Tx, targets []PhotoAuthoredTarget, opts photoAuthoredApply) (PhotoAuthoredReceipt, error) {
	result := PhotoAuthoredReceipt{UndoOf: opts.undoOf, Sidecar: opts.sidecar, Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}
	if len(targets) < 1 {
		return result, ErrInvalidPhotoAsset
	}
	nodes, err := s.planPhotoAuthoredTx(ctx, tx, targets, opts, &result)
	if err != nil {
		return result, err
	}
	if len(result.After) == 0 || opts.persist && !photoAuthoredChanged(result) {
		return PhotoAuthoredReceipt{Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}, nil
	}
	id, err := newUUIDv4()
	if err != nil {
		return result, err
	}
	result.ReceiptID = id
	if opts.persist {
		if err := checkPhotoAuthoredReceiptSize(result); err != nil {
			return result, err
		}
	}
	if err := s.writePhotoAuthoredValuesTx(ctx, tx, result, nodes, opts.loaded); err != nil {
		return result, err
	}
	if !opts.persist {
		return result, nil
	}
	return result, persistPhotoAuthoredReceiptTx(ctx, tx, result)
}

// planPhotoAuthoredTx appends each target's before and after snapshot to result
// in file ID order and returns the matching nodes.
func (s *Store) planPhotoAuthoredTx(ctx context.Context, tx *sql.Tx, targets []PhotoAuthoredTarget, opts photoAuthoredApply, result *PhotoAuthoredReceipt) ([]Node, error) {
	targets = slices.Clone(targets)
	slices.SortFunc(targets, func(a, b PhotoAuthoredTarget) int { return strings.Compare(a.FileID, b.FileID) })
	nodes := []Node{}
	assets := []string{}
	changedAssets := map[string]bool{}
	for i, t := range targets {
		if validateUUIDv4(t.FileID) != nil || t.Revision < 1 || i > 0 && targets[i-1].FileID == t.FileID {
			return nodes, ErrInvalidPhotoAsset
		}
		f, node, err := s.loadPhotoAuthoredTargetTx(ctx, tx, t.FileID, opts)
		if err != nil {
			return nodes, err
		}
		v, err := photoAuthoredTargetValues(t, f, node)
		if err != nil {
			return nodes, err
		}
		if v == f.Authored() && !opts.force {
			continue
		}
		if f.Revision == math.MaxInt64 || node.Revision == math.MaxInt64 {
			return nodes, ErrInvalidPhotoAsset
		}
		before := PhotoAuthoredSnapshot{f.ID, f.NodeID, f.Revision, f.Authored()}
		after := before
		if v != f.Authored() {
			after.Revision++
		}
		after.Values = v
		result.Before = append(result.Before, before)
		result.After = append(result.After, after)
		assets = append(assets, f.AssetID)
		if after.Revision != before.Revision {
			changedAssets[f.AssetID] = true
		}
		nodes = append(nodes, node)
	}
	if opts.force && photoAuthoredChanged(*result) {
		for i := range result.After {
			if result.After[i].Revision == result.Before[i].Revision && assets[i] != "" && changedAssets[assets[i]] {
				result.After[i].Revision++
			}
		}
	}
	return nodes, nil
}

func (s *Store) loadPhotoAuthoredTargetTx(ctx context.Context, tx *sql.Tx, fileID string, opts photoAuthoredApply) (PhotoFile, Node, error) {
	if snapshot, ok := opts.loaded[fileID]; ok {
		return snapshot.file, snapshot.node, nil
	}
	f, err := photoFileByIDQuery(ctx, tx, fileID)
	if err != nil {
		return f, Node{}, err
	}
	node, err := nodeByIDQuery(ctx, tx, f.NodeID)
	if err != nil {
		return f, node, err
	}
	if opts.sidecar == nil && f.AssetID != "" {
		if err := s.photoAuthoredAccessTx(ctx, tx, f.AssetID); err != nil {
			return f, node, err
		}
	}
	if opts.loaded != nil {
		opts.loaded[f.ID] = photoAuthoredLoaded{f, node}
	}
	return f, node, nil
}

// photoAuthoredTargetValues returns the values target t gives file f after
// checking that f is a current, live, editable member.
func photoAuthoredTargetValues(t PhotoAuthoredTarget, f PhotoFile, node Node) (PhotoAuthored, error) {
	if f.Revision != t.Revision {
		return PhotoAuthored{}, ErrStaleRevision
	}
	if f.Role == PhotoRoleSidecar {
		return PhotoAuthored{}, ErrInvalidPhotoAsset
	}
	if node.TrashedAt != nil {
		return PhotoAuthored{}, ErrNotFound
	}
	var v PhotoAuthored
	if t.restore != nil {
		v = *t.restore
	} else {
		v = t.Patch.apply(f.Authored())
	}
	return v, ValidatePhotoAuthored(v)
}

// writePhotoAuthoredValuesTx stores each changed snapshot in result and audits it.
func (s *Store) writePhotoAuthoredValuesTx(ctx context.Context, tx *sql.Tx, result PhotoAuthoredReceipt, nodes []Node, loaded map[string]photoAuthoredLoaded) error {
	active, err := auditAuthorityActiveTx(ctx, tx)
	if err != nil {
		return err
	}
	now := nowRFC3339()
	for i, a := range result.After {
		v := a.Values
		if a.Revision == result.Before[i].Revision {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET revision=?,rating=?,flag=?,label=?,caption=?,creator=?,copyright=?,rotation=?,confirmed_fields=? WHERE file_id=?`, a.Revision, v.Rating, v.Flag, v.Label, v.Caption, v.Creator, v.Copyright, v.Rotation, v.Confirmed, a.FileID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET revision=revision+1,modified_at=? WHERE id=?`, now, a.NodeID); err != nil {
			return err
		}
		if snapshot, ok := loaded[a.FileID]; ok {
			snapshot.node.Revision++
			snapshot.node.ModifiedAt = now
			loaded[a.FileID] = snapshot
		}
		if active {
			if err := s.persistPhotoAuthoredAuditTx(ctx, tx, nodes[i], result.Before[i], a, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// persistPhotoRepresentativeReceiptTx records a choice that changes no file.
func persistPhotoRepresentativeReceiptTx(ctx context.Context, tx *sql.Tx, result PhotoAuthoredReceipt) (PhotoAuthoredReceipt, error) {
	result.Before, result.After = []PhotoAuthoredSnapshot{}, []PhotoAuthoredSnapshot{}
	var err error
	if result.ReceiptID, err = newUUIDv4(); err != nil {
		return result, err
	}
	if err := checkPhotoAuthoredReceiptSize(result); err != nil {
		return result, err
	}
	return result, persistPhotoAuthoredReceiptTx(ctx, tx, result)
}

func persistPhotoAuthoredReceiptTx(ctx context.Context, tx *sql.Tx, result PhotoAuthoredReceipt) error {
	operation := "authored"
	if result.UndoOf != "" {
		operation = "authored_undo"
	}
	if result.Sidecar != nil {
		operation = "authored_sidecar"
	}
	return writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: result.ReceiptID, Operation: operation, BeforeRevision: 0, AfterRevision: 1, Before: result.Before, After: result.afterState()})
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
		for _, fence := range receipt.KeywordNodes {
			node, err := nodeByIDQuery(ctx, tx, fence.NodeID)
			if err != nil {
				return err
			}
			if node.Revision != fence.AfterRevision {
				return ErrStaleRevision
			}
		}
		targets := make([]PhotoAuthoredTarget, len(receipt.After))
		for i, a := range receipt.After {
			targets[i] = PhotoAuthoredTarget{FileID: a.FileID, Revision: a.Revision, restore: &receipt.Before[i].Values}
		}
		loaded := map[string]photoAuthoredLoaded{}
		result = PhotoAuthoredReceipt{UndoOf: id, Before: []PhotoAuthoredSnapshot{}, After: []PhotoAuthoredSnapshot{}}
		if len(targets) > 0 {
			result, err = s.applyPhotoAuthoredValuesTx(ctx, tx, targets, photoAuthoredApply{undoOf: id, force: true, loaded: loaded})
			if err != nil {
				return err
			}
		}
		// A representative choice returns only while its asset still has the revision the choice wrote.
		for _, change := range receipt.Representatives {
			if err := s.photoAuthoredAccessTx(ctx, tx, change.AssetID); err != nil {
				return err
			}
			restored, err := setPhotoRepresentativeTx(ctx, tx, change.AssetID, change.AfterRevision, change.Before)
			if err != nil {
				return err
			}
			result.Representatives = append(result.Representatives, restored)
		}
		if result.ReceiptID == "" && len(result.Representatives) > 0 {
			if result.ReceiptID, err = newUUIDv4(); err != nil {
				return err
			}
		}
		changes := slices.Clone(receipt.Keywords)
		for i := range changes {
			changes[i].Before, changes[i].After = changes[i].After, changes[i].Before
		}
		nodes := map[int64]Node{}
		for _, snapshot := range loaded {
			nodes[snapshot.node.ID] = snapshot.node
		}
		plan := photoKeywordPlan{changes: changes, states: map[string]photoKeywordState{}}
		for group := range photoKeywordGroups(changes) {
			tagID := group[0].TagID
			tagTargets := make([]BatchTagTarget, 0, len(group))
			for _, change := range group {
				node := nodes[change.NodeID]
				tagTargets = append(tagTargets, BatchTagTarget{NodeID: node.ID, Revision: node.Revision})
			}
			states, err := loadBatchTagTargetsTx(ctx, tx, tagID, tagTargets)
			if err != nil {
				return err
			}
			tag, err := tagByIDTx(tx, tagID)
			if errors.Is(err, ErrNotFound) {
				return fmt.Errorf("keyword no longer exists; reload metadata before undoing: %w", ErrStaleRevision)
			}
			if err != nil {
				return err
			}
			plan.states[tagID] = photoKeywordState{tag, states}
		}
		return s.applyPhotoKeywordsTx(ctx, tx, &result, plan, nodes)
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
	if r.ReceiptID != id || validateUUIDv4(id) != nil || len(r.Before) < 1 && len(r.Representatives) == 0 || len(r.Before) > MaxPhotoStackTargets || len(r.Before) != len(r.After) || r.UndoOf != "" && validateUUIDv4(r.UndoOf) != nil {
		return ErrInvalidPhotoAsset
	}
	if r.Sidecar != nil && (r.Sidecar.NodeID < 1 || validateUUIDv4(r.Sidecar.VersionID) != nil || validateUUIDv4(r.Sidecar.FileID) != nil || r.UndoOf != "") {
		return ErrInvalidPhotoAsset
	}
	if err := validatePhotoKeywords(r); err != nil {
		return err
	}
	if len(r.Representatives) > MaxPhotoStackFrames || r.Sidecar != nil && len(r.Representatives) > 0 {
		return ErrInvalidPhotoAsset
	}
	for i, change := range r.Representatives {
		if validateUUIDv4(change.AssetID) != nil || i > 0 && r.Representatives[i-1].AssetID >= change.AssetID || change.BeforeRevision < 1 || change.AfterRevision != change.BeforeRevision+1 ||
			change.Before != nil && *change.Before < 1 || change.After != nil && *change.After < 1 || (change.Before == nil) == (change.After == nil) && (change.Before == nil || *change.Before == *change.After) {
			return ErrInvalidPhotoAsset
		}
	}
	keywordNodes := make(map[int64]bool, len(r.KeywordNodes))
	for _, fence := range r.KeywordNodes {
		keywordNodes[fence.NodeID] = true
	}
	for i, b := range r.Before {
		a := r.After[i]
		if validateUUIDv4(b.FileID) != nil || b.NodeID < 1 || b.Revision < 1 || b.Revision == math.MaxInt64 || a.FileID != b.FileID || a.NodeID != b.NodeID || (a.Revision != b.Revision+1 && (a.Revision != b.Revision || a.Values != b.Values || !keywordNodes[b.NodeID])) || i > 0 && r.Before[i-1].FileID >= b.FileID {
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
