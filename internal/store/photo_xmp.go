package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/kit/packstore"
)

const (
	PhotoXMPStateNone        = "none"
	PhotoXMPStatePending     = "pending"
	PhotoXMPStateSynced      = "synced"
	PhotoXMPStateConflict    = "conflict"
	PhotoXMPStateUnavailable = "unavailable"
	PhotoXMPStateInvalid     = "invalid"
	// photoXMPStateWaiting reads as Pending; the worker skips it until the
	// original's source metadata supplies its orientation.
	photoXMPStateWaiting = "waiting"
)

// PhotoXMPKeywordsBit marks keywords beside the authored field bits in sync plans.
const PhotoXMPKeywordsBit PhotoAuthoredFields = 1 << 7

const MaxPhotoXMPKeywords = maxBatchTagTargets * 100

// PhotoXMPPacket is the supported content of one sidecar version, with the
// raw rotation evidence needed to compose it with the original's orientation.
type PhotoXMPPacket struct {
	Rotation    *int                `json:"rotation,omitzero"`
	Ignored     PhotoAuthoredFields `json:"ignored"`
	Values      PhotoAuthored       `json:"values"`
	Keywords    []string            `json:"keywords"`
	Orientation int                 `json:"orientation"`
}

// PhotoXMPSnapshot pairs a packet with the catalog decisions it agreed with.
type PhotoXMPSnapshot struct {
	Packet   PhotoXMPPacket `json:"packet"`
	Catalog  PhotoAuthored  `json:"catalog"`
	Keywords []string       `json:"keywords"`
}

type PhotoXMPRow struct {
	Field   string `json:"field"`
	Catalog string `json:"catalog"`
	Sidecar string `json:"sidecar"`
}

// photoXMPReceipt is the one retained sync receipt of an original. Baseline is
// the last successful sync; Source is the last outside packet accepted, whose
// bytes have digest SourceSHA256. A later outside version merges against
// Source because it was edited from those bytes, not from Docbank's version.
type photoXMPReceipt struct {
	Version           int                    `json:"version"`
	FileID            string                 `json:"file_id"`
	NodeID            int64                  `json:"node_id"`
	Sidecar           PhotoSidecarProvenance `json:"sidecar"`
	Extension         string                 `json:"extension,omitzero"`
	OriginalVersionID string                 `json:"original_version_id"`
	Orientation       int                    `json:"orientation"`
	Baseline          PhotoXMPSnapshot       `json:"baseline"`
	Source            *PhotoXMPSnapshot      `json:"source,omitzero"`
	SourceSHA256      string                 `json:"source_sha256,omitzero"`
	ImportReceiptID   string                 `json:"import_receipt_id,omitzero"`
}

func decodePhotoXMPReceipt(data []byte) (photoXMPReceipt, error) {
	var r photoXMPReceipt
	if len(data) > maxBatchTagReceiptJSONBytes {
		return r, ErrPhotoEditReceiptLimit
	}
	if err := json.Unmarshal(data, &r, json.RejectUnknownMembers(true)); err != nil {
		return r, fmt.Errorf("%w: decoding XMP receipt: %w", ErrInvalidPhotoAsset, err)
	}
	snapshots := []PhotoXMPSnapshot{r.Baseline}
	if r.Source != nil {
		snapshots = append(snapshots, *r.Source)
		if _, err := packstore.ParseHash(r.SourceSHA256); err != nil {
			return r, fmt.Errorf("%w: XMP source digest: %w", ErrInvalidPhotoAsset, err)
		}
	}
	for _, s := range snapshots {
		p := s.Packet
		if ValidatePhotoAuthored(s.Catalog) != nil || ValidatePhotoXMPKeywords(s.Keywords) != nil ||
			!p.Values.Confirmed.Valid() || ValidatePhotoAuthoredValues(p.Values) != nil || ValidatePhotoXMPKeywords(p.Keywords) != nil ||
			p.Ignored&^(PhotoConfirmedAll|PhotoXMPKeywordsBit) != 0 || p.Orientation < 0 || p.Orientation > 8 ||
			p.Rotation != nil && !slices.Contains([]int{0, 90, 180, 270}, *p.Rotation) {
			return r, ErrInvalidPhotoAsset
		}
	}
	if r.Version != 1 || validateUUIDv4(r.FileID) != nil || r.NodeID < 1 || r.Sidecar.NodeID < 1 ||
		validateUUIDv4(r.Sidecar.FileID) != nil || validateUUIDv4(r.Sidecar.VersionID) != nil ||
		validateUUIDv4(r.OriginalVersionID) != nil || r.Orientation < 0 || r.Orientation > 8 ||
		len(r.Extension) > MaxPhotoAuthoredTextBytes || r.Extension != strings.ToLower(r.Extension) ||
		r.Source == nil && r.SourceSHA256 != "" || r.ImportReceiptID != "" && validateUUIDv4(r.ImportReceiptID) != nil {
		return r, ErrInvalidPhotoAsset
	}
	return r, nil
}

func ValidatePhotoXMPKeywords(names []string) error {
	if names == nil || len(names) > MaxPhotoXMPKeywords || !slices.Equal(names, PhotoXMPKeywords(names)) {
		return ErrInvalidPhotoAsset
	}
	for _, name := range names {
		canonical, err := NormalizeTagName(name)
		if err != nil || canonical != name || len(name) > MaxPhotoAuthoredTextBytes {
			return ErrInvalidPhotoAsset
		}
	}
	data, err := json.Marshal(names)
	if err != nil || len(data) > document.MaxSourceMetadataValueBytes {
		return ErrPhotoEditReceiptLimit
	}
	return nil
}

// PhotoXMPKeywords returns the sorted, distinct, non-nil keyword set.
func PhotoXMPKeywords(names []string) []string {
	r := uniqueSorted(names)
	if r == nil {
		return []string{}
	}
	return r
}

func PhotoXMPOverlay(catalog, packet PhotoAuthored, mask PhotoAuthoredFields) PhotoAuthored {
	patch := PhotoAuthoredPatch{}
	for _, field := range photoAuthoredFields {
		if mask&field.bit != 0 {
			field.setPatch(&patch, &packet)
		}
	}
	return patch.apply(catalog)
}

func PhotoXMPValuesEqual(a, b PhotoAuthored, mask PhotoAuthoredFields) bool {
	for _, field := range photoAuthoredFields {
		if mask&field.bit != 0 && !field.equal(&a, &b) {
			return false
		}
	}
	return true
}

// PhotoXMPOrientation composes an EXIF orientation with a clockwise rotation.
func PhotoXMPOrientation(source, rotation int) int {
	clockwise := [9]int{0, 6, 7, 8, 5, 2, 3, 4, 1}
	if source < 1 || source > 8 {
		return 0
	}
	for range rotation / 90 {
		source = clockwise[source]
	}
	return source
}

// photoXMPDetail is the stored explanation of a non-synced outcome.
type photoXMPDetail struct {
	Reason     string        `json:"reason,omitzero"`
	Rows       []PhotoXMPRow `json:"rows,omitzero"`
	ConflictID string        `json:"conflict_id,omitzero"`
}

// PhotoXMPState is the stored sync status of one original.
type PhotoXMPState struct {
	State      string                `json:"state" enum:"none,synced,pending,conflict,unavailable,invalid"`
	Reason     string                `json:"reason,omitzero"`
	Rows       []PhotoXMPRow         `json:"rows,omitzero"`
	ConflictID string                `json:"conflict_id,omitzero"`
	Receipt    *PhotoAuthoredReceipt `json:"receipt,omitzero" doc:"The sidecar import that Undo can still reverse."`
}

func photoXMPStateTx(ctx context.Context, tx *sql.Tx, f PhotoFile) (PhotoXMPState, error) {
	var r PhotoXMPState
	var detail, receiptID string
	if err := tx.QueryRowContext(ctx, `SELECT xmp_state,xmp_detail,xmp_receipt_id FROM photo_files WHERE file_id=?`, f.ID).Scan(&r.State, &detail, &receiptID); err != nil {
		return r, err
	}
	if r.State == photoXMPStateWaiting {
		r.State = PhotoXMPStatePending
	}
	if detail != "" {
		var d photoXMPDetail
		if err := json.Unmarshal([]byte(detail), &d, json.RejectUnknownMembers(true)); err != nil {
			return r, fmt.Errorf("decoding XMP status detail: %w", err)
		}
		r.Reason, r.Rows, r.ConflictID = d.Reason, d.Rows, d.ConflictID
	}
	if receiptID == "" {
		return r, nil
	}
	receipt, err := loadPhotoXMPReceiptTx(ctx, tx, receiptID)
	if err != nil || receipt.ImportReceiptID == "" {
		return r, nil //nolint:nilerr // An unreadable receipt surfaces as Unavailable once the worker classifies it.
	}
	imported, err := loadPhotoAuthoredReceipt(ctx, tx, receipt.ImportReceiptID)
	if errors.Is(err, ErrNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	for _, after := range imported.After {
		if after.FileID == f.ID && after.Revision == f.Revision {
			r.Receipt = &imported
		}
	}
	return r, nil
}

func loadPhotoXMPReceiptTx(ctx context.Context, q metadataQuerier, id string) (photoXMPReceipt, error) {
	var data []byte
	if err := q.QueryRowContext(ctx, `SELECT after_json FROM photo_change_receipts WHERE receipt_id=? AND operation='xmp_sync'`, id).Scan(&data); err != nil {
		return photoXMPReceipt{}, err
	}
	return decodePhotoXMPReceipt(data)
}

// PhotoXMPSidecar is the one live sidecar attached to an original.
type PhotoXMPSidecar struct {
	FileID       string
	Node         Node
	SourceSHA256 string
	audited      bool
}

// PhotoXMPWork holds every input of one original's sync decision. Publication
// reloads it in its own transaction and refuses a plan made from other inputs.
type PhotoXMPWork struct {
	Original    PhotoFile
	Node        Node
	Keywords    []string
	Orientation int
	Sidecar     *PhotoXMPSidecar

	receiptID              string
	extracted              bool
	keywordLimit, occupied bool
	auditActive, audited   bool
	members                int
	sidecarNames           []string
	receipt                *photoXMPReceipt
	receiptErr             bool
	intended               string
	state                  string
	detail                 photoXMPDetail
}

// Base returns the snapshot the current sidecar version descends from and the
// original's orientation then, or false before the sidecar's first sync.
func (w PhotoXMPWork) Base() (PhotoXMPSnapshot, int, bool) {
	r := w.receipt
	if r == nil || w.Sidecar == nil || r.Sidecar.FileID != w.Sidecar.FileID {
		return PhotoXMPSnapshot{}, w.Orientation, false
	}
	if w.Sidecar.Node.CurrentVersionID != r.Sidecar.VersionID && r.Source != nil && w.Sidecar.Node.BlobHash == w.Sidecar.SourceSHA256 {
		return *r.Source, r.Orientation, true
	}
	return r.Baseline, r.Orientation, true
}

func loadPhotoXMPWorkTx(ctx context.Context, tx *sql.Tx, id string) (PhotoXMPWork, error) {
	f, err := photoFileByIDQuery(ctx, tx, id)
	if err != nil {
		return PhotoXMPWork{}, err
	}
	n, err := nodeByIDQuery(ctx, tx, f.NodeID)
	if err != nil {
		return PhotoXMPWork{}, err
	}
	w := PhotoXMPWork{Original: f, Node: n}
	var detail string
	err = tx.QueryRowContext(ctx, `SELECT f.xmp_state,f.xmp_detail,f.xmp_receipt_id,
 EXISTS(SELECT 1 FROM source_metadata_heads WHERE source_sha256=?),
 COALESCE((SELECT t.orientation FROM source_metadata_heads h JOIN photo_technical_metadata t ON t.generation_id=h.generation_id WHERE h.source_sha256=?),1),
 (SELECT count(*) FROM photo_files WHERE asset_id=f.asset_id),
 EXISTS(SELECT 1 FROM nodes o WHERE o.parent_id IS ? AND lower(o.name)=lower(?) AND o.trashed_at IS NULL),
 EXISTS(SELECT 1 FROM audit_authority),EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=f.node_id)
 FROM photo_files f WHERE f.file_id=?`, n.BlobHash, n.BlobHash, n.ParentID, n.Name+".xmp", id).Scan(
		&w.state, &detail, &w.receiptID, &w.extracted, &w.Orientation, &w.members, &w.occupied, &w.auditActive, &w.audited)
	if err != nil {
		return w, err
	}
	if detail != "" {
		if err := json.Unmarshal([]byte(detail), &w.detail); err != nil {
			return w, fmt.Errorf("decoding XMP status detail: %w", err)
		}
	}
	if !photoSidecarTargetRole(f.Role) || f.AssetID == "" {
		return w, nil
	}
	err = queryRows(ctx, tx, `SELECT `+nodeCols+`,sf.file_id,sf.source_sha256,
 EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=n.id)
 FROM `+nodeFrom+` JOIN photo_files sf ON sf.node_id=n.id
 WHERE sf.sidecar_of_file_id=? AND sf.asset_id=? AND n.trashed_at IS NULL ORDER BY n.name,sf.file_id`, []any{f.ID, f.AssetID}, func(rows *sql.Rows) error {
		var sc PhotoXMPSidecar
		var err error
		sc.Node, err = scanNode(rows, &sc.FileID, &sc.SourceSHA256, &sc.audited)
		if err != nil {
			return err
		}
		w.Sidecar = &sc
		w.sidecarNames = append(w.sidecarNames, sc.Node.Name)
		return nil
	})
	if err != nil {
		return w, err
	}
	w.Keywords, err = photoKeywordNames(ctx, tx, n.ID)
	if errors.Is(err, bundle.ErrLimit) || err == nil && ValidatePhotoXMPKeywords(w.Keywords) != nil {
		w.Keywords, w.keywordLimit, err = []string{}, true, nil
	}
	if err != nil {
		return w, err
	}
	if w.receiptID != "" {
		receipt, err := loadPhotoXMPReceiptTx(ctx, tx, w.receiptID)
		if err != nil || receipt.FileID != f.ID {
			w.receiptErr = true
		} else {
			w.receipt = &receipt
		}
	}
	// Pairing evidence is an input: a synced sidecar must still name this original.
	if w.Sidecar != nil && len(w.sidecarNames) == 1 && w.receipt != nil && w.receipt.Sidecar.FileID == w.Sidecar.FileID {
		w.intended, err = w.pairing(ctx, tx, w.receipt.Extension)
	}
	return w, err
}

func (w PhotoXMPWork) pairing(ctx context.Context, tx *sql.Tx, extension string) (string, error) {
	return photoSidecarIntendedTarget(ctx, tx, PhotoFile{Role: PhotoRoleSidecar, AssetID: w.Original.AssetID, SidecarOfID: &w.Original.ID}, w.Sidecar.Node, extension)
}

const photoXMPExtractionReason = "Source metadata extraction is pending. Wait for Background jobs, then refresh XMP."

// classify decides every outcome that needs no sidecar bytes. Pending means
// the worker must read and merge the packet.
func (w PhotoXMPWork) classify() (string, string) {
	r := w.receipt
	catalogUnchanged := r != nil && w.Original.Authored() == r.Baseline.Catalog && slices.Equal(w.Keywords, r.Baseline.Keywords)
	switch {
	case !photoSidecarTargetRole(w.Original.Role) || w.Original.AssetID == "" || w.Node.TrashedAt != nil ||
		w.Original.Authored().Confirmed == 0 && len(w.Keywords) == 0 && !w.keywordLimit && w.Sidecar == nil:
		return PhotoXMPStateNone, ""
	case w.Sidecar == nil && catalogUnchanged:
		// The person removed the sidecar Docbank last synced; a later decision reopens sync.
		return PhotoXMPStateNone, ""
	case w.auditActive && !w.audited:
		return PhotoXMPStateUnavailable, "The photo is outside the active audit scope."
	case w.auditActive && w.Sidecar == nil:
		return PhotoXMPStateUnavailable, "Audit authority blocks creating a sidecar attachment."
	case w.auditActive && !w.Sidecar.audited:
		return PhotoXMPStateUnavailable, "The sidecar is outside the active audit scope."
	case w.receiptErr:
		return PhotoXMPStateUnavailable, "The saved XMP sync receipt could not be read."
	case w.keywordLimit:
		return PhotoXMPStateInvalid, "Catalog keywords exceed the XMP limits. Remove keywords or shorten their names to resume sync."
	case len(w.sidecarNames) > 1:
		names := w.sidecarNames
		if len(names) > 5 {
			names = append(names[:5:5], fmt.Sprintf("%d more", len(w.sidecarNames)-5))
		}
		return PhotoXMPStateInvalid, "More than one XMP sidecar is attached: " + strings.Join(names, ", ") + ". Remove all but one sidecar to resume sync."
	case !w.extracted:
		return photoXMPStateWaiting, photoXMPExtractionReason
	case w.Sidecar == nil && w.members >= PhotoMaxFiles:
		return PhotoXMPStateInvalid, "This photo has 256 members. Detach a member so sync can create a sidecar."
	case w.Sidecar == nil && w.occupied:
		return PhotoXMPStateInvalid, "The sidecar name is already occupied. Rename or trash that file to resume sync."
	case w.Sidecar != nil && catalogUnchanged && r.Sidecar.FileID == w.Sidecar.FileID && r.Sidecar.VersionID == w.Sidecar.Node.CurrentVersionID &&
		r.OriginalVersionID == w.Node.CurrentVersionID && r.Orientation == w.Orientation && w.intended == w.Original.ID && r.SourceSHA256 == w.Sidecar.SourceSHA256:
		return PhotoXMPStateSynced, ""
	}
	return PhotoXMPStatePending, ""
}

// ClassifyPhotoXMP stores the status of a Pending original that needs no
// sidecar bytes and reports whether the worker must still merge it.
func (s *Store) ClassifyPhotoXMP(ctx context.Context, id string) (w PhotoXMPWork, run bool, err error) {
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		w, err = loadPhotoXMPWorkTx(ctx, tx, id)
		if err != nil || w.state != PhotoXMPStatePending {
			return err
		}
		state, reason := w.classify()
		if run = state == PhotoXMPStatePending; run {
			return nil
		}
		return setPhotoXMPStateTx(ctx, tx, id, state, photoXMPDetail{Reason: reason})
	})
	return
}

// PhotoXMPConflict loads a recorded conflict for a person's choice.
func (s *Store) PhotoXMPConflict(ctx context.Context, id, conflictID string) (w PhotoXMPWork, err error) {
	err = s.photoReadTx(ctx, func(tx *sql.Tx) error {
		w, err = loadPhotoXMPWorkTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.photoAuthoredAccessTx(ctx, tx, w.Original.AssetID); err != nil {
			return err
		}
		if w.state != PhotoXMPStateConflict || w.detail.ConflictID != conflictID {
			return ErrStaleRevision
		}
		return nil
	})
	return
}

func setPhotoXMPStateTx(ctx context.Context, tx *sql.Tx, id, state string, detail photoXMPDetail) error {
	encoded, err := json.Marshal(detail, json.Deterministic(true))
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE photo_files SET xmp_state=?,xmp_detail=? WHERE file_id=?`, state, string(encoded), id)
	}
	return err
}

// PhotoXMPTargetsAfter lists Pending originals after a node ID cursor.
func (s *Store) PhotoXMPTargetsAfter(ctx context.Context, after string, limit int) ([]PhotoSidecarTarget, error) {
	cursor, err := strconv.ParseInt(cmp.Or(after, "0"), 10, 64)
	if err != nil || limit < 1 || limit > maxBatchTagTargets {
		return nil, ErrInvalidPhotoAsset
	}
	result := []PhotoSidecarTarget{}
	err = queryRows(ctx, s.db, `SELECT file_id,node_id FROM photo_files INDEXED BY photo_files_xmp_pending
 WHERE xmp_state IN ('pending','waiting') AND xmp_state='pending' AND node_id>? ORDER BY node_id LIMIT ?`, []any{cursor, limit}, func(rows *sql.Rows) error {
		var t PhotoSidecarTarget
		if err := rows.Scan(&t.FileID, &t.NodeID); err != nil {
			return err
		}
		result = append(result, t)
		return nil
	})
	return result, err
}

// PhotoSidecarTarget is one Pending original for the sidecar worker.
type PhotoSidecarTarget struct {
	FileID string
	NodeID int64
}
