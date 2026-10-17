package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
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

// photoXMPSync is an original's last successful sync. Baseline is what the
// sidecar and catalog agreed on then; Source is the last outside packet
// accepted, whose bytes have digest SourceSHA256. A later outside version
// merges against Source because it was edited from those bytes, not from
// Docbank's version.
type photoXMPSync struct {
	Sidecar           PhotoSidecarProvenance `json:"sidecar"`
	Extension         string                 `json:"extension,omitzero"`
	OriginalVersionID string                 `json:"original_version_id"`
	Orientation       int                    `json:"orientation"`
	Baseline          PhotoXMPSnapshot       `json:"baseline"`
	Source            *PhotoXMPSnapshot      `json:"source,omitzero"`
	SourceSHA256      string                 `json:"source_sha256,omitzero"`
	ImportReceiptID   string                 `json:"import_receipt_id,omitzero"`
}

// photoXMPReceipt is one immutable sync outcome of an original. Its row's
// after_revision orders the original's receipts; the latest is its status
// while the settled-work fence is current. A receipt that only records an
// outcome carries the previous successful sync forward.
type photoXMPReceipt struct {
	Version    int           `json:"version"`
	FileID     string        `json:"file_id"`
	NodeID     int64         `json:"node_id"`
	State      string        `json:"state"`
	Reason     string        `json:"reason,omitzero"`
	Rows       []PhotoXMPRow `json:"rows,omitzero"`
	ConflictID string        `json:"conflict_id,omitzero"`
	Sync       *photoXMPSync `json:"sync,omitzero"`
}

func decodePhotoXMPReceipt(data []byte) (photoXMPReceipt, error) {
	var r photoXMPReceipt
	if len(data) > maxBatchTagReceiptJSONBytes {
		return r, ErrPhotoEditReceiptLimit
	}
	if err := json.Unmarshal(data, &r, json.RejectUnknownMembers(true)); err != nil {
		return r, fmt.Errorf("%w: decoding XMP receipt: %w", ErrInvalidPhotoAsset, err)
	}
	conflict := r.State == PhotoXMPStateConflict
	if r.Version != 1 || validateUUIDv4(r.FileID) != nil || r.NodeID < 1 || len(r.Reason) > MaxPhotoAuthoredTextBytes ||
		!slices.Contains([]string{PhotoXMPStateNone, PhotoXMPStatePending, PhotoXMPStateSynced, PhotoXMPStateConflict, PhotoXMPStateUnavailable, PhotoXMPStateInvalid}, r.State) ||
		conflict != (len(r.Rows) > 0) || conflict != (r.ConflictID != "") || conflict && validateUUIDv4(r.ConflictID) != nil ||
		r.State == PhotoXMPStateSynced && r.Sync == nil {
		return r, ErrInvalidPhotoAsset
	}
	for _, row := range r.Rows {
		if len(row.Field) > MaxPhotoAuthoredTextBytes || len(row.Catalog) > MaxPhotoAuthoredTextBytes || len(row.Sidecar) > MaxPhotoAuthoredTextBytes {
			return r, ErrInvalidPhotoAsset
		}
	}
	if r.Sync == nil {
		return r, nil
	}
	sync := r.Sync
	snapshots := []PhotoXMPSnapshot{sync.Baseline}
	if sync.Source != nil {
		snapshots = append(snapshots, *sync.Source)
		if _, err := packstore.ParseHash(sync.SourceSHA256); err != nil {
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
	if sync.Sidecar.NodeID < 1 || validateUUIDv4(sync.Sidecar.FileID) != nil || validateUUIDv4(sync.Sidecar.VersionID) != nil ||
		validateUUIDv4(sync.OriginalVersionID) != nil || sync.Orientation < 0 || sync.Orientation > 8 ||
		len(sync.Extension) > MaxPhotoAuthoredTextBytes || sync.Extension != strings.ToLower(sync.Extension) ||
		sync.Source == nil && sync.SourceSHA256 != "" || sync.ImportReceiptID != "" && validateUUIDv4(sync.ImportReceiptID) != nil {
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

// PhotoXMPState is the sync status of one original.
type PhotoXMPState struct {
	State      string                `json:"state" enum:"none,synced,pending,conflict,unavailable,invalid"`
	Reason     string                `json:"reason,omitzero"`
	Rows       []PhotoXMPRow         `json:"rows,omitzero"`
	ConflictID string                `json:"conflict_id,omitzero"`
	Receipt    *PhotoAuthoredReceipt `json:"receipt,omitzero" doc:"The sidecar import that Undo can still reverse."`
}

// The SQL below reads photo original f, its node n and its photo a. A
// receipt lookup compares with f.node_id+0: the column's integer affinity would
// otherwise keep SQLite from seeking the json_extract index.

// photoXMPSignalsSQL is true for an original that has sync work to settle:
// decisions, keywords, an attached sidecar, or an earlier sync outcome.
const photoXMPSignalsSQL = `(f.role IN ('raw','image') AND (f.confirmed_fields<>0
 OR EXISTS(SELECT 1 FROM node_tags WHERE node_id=f.node_id)
 OR EXISTS(SELECT 1 FROM photo_files sf WHERE sf.sidecar_of_file_id=f.file_id)
 OR EXISTS(SELECT 1 FROM photo_change_receipts xr INDEXED BY photo_change_receipts_xmp_node
 WHERE xr.operation='xmp_sync' AND json_extract(xr.after_json,'$.node_id')=f.node_id+0)))`

// photoXMPFenceTokenSQL extends the sidecar member snapshot with whether a
// node holds the original's exact sidecar name and whether audit authority is
// active. Neither changes a member revision.
const photoXMPFenceTokenSQL = photoSidecarMemberRevisionsSQL + ` || '|' ||
 EXISTS(SELECT 1 FROM nodes o WHERE o.parent_id=n.parent_id AND o.name=n.name || '.xmp' AND o.trashed_at IS NULL) ||
 EXISTS(SELECT 1 FROM audit_authority)`

// photoXMPExtractorSQL identifies the extraction of the original's current
// bytes, or is empty before extraction. Re-extraction with another extractor
// publishes a new head generation. The fence stores it in the
// extractor_fingerprint column.
const photoXMPExtractorSQL = `COALESCE((SELECT h.generation_id FROM content_versions xv
 JOIN source_metadata_heads h ON h.source_sha256=xv.blob_hash WHERE xv.version_id=n.current_version_id),'')`

// photoXMPFenceMatchesSQL is true while fence row c still describes every
// sync input. Authored edits, keywords, renames, versions, observations and
// trash advance node revisions; attachment advances the photo revision;
// extraction publishes a new head generation.
const photoXMPFenceMatchesSQL = `c.asset_id=f.asset_id AND c.asset_revision=a.revision AND c.version_id=n.current_version_id
 AND c.extractor_fingerprint=` + photoXMPExtractorSQL + ` AND c.member_revisions=` + photoXMPFenceTokenSQL

// photoXMPFenceCurrentSQL is true while the original's settled work is
// current. The photo_sidecar_considered triggers delete the row when an input
// changes, so its presence stands for photoXMPFenceMatchesSQL.
const photoXMPFenceCurrentSQL = `EXISTS(SELECT 1 FROM photo_sidecar_considered c WHERE c.sidecar_file_id=f.file_id)`

// photoXMPUnfencedSQL joins each photo file f that lacks a fence row. It probes
// fence rows from the file ID index alone, in key order, before reading f.
const photoXMPUnfencedSQL = `photo_files d CROSS JOIN photo_files f ON f.file_id=d.file_id
 AND NOT EXISTS(SELECT 1 FROM photo_sidecar_considered c WHERE c.sidecar_file_id=d.file_id)`

// RepairPhotoXMPFences deletes fence rows whose stored token no longer
// matches, such as rows settled under an older token definition. The daemon
// runs it once before the sync worker starts.
func (s *Store) RepairPhotoXMPFences(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM photo_sidecar_considered WHERE sidecar_file_id NOT IN (SELECT c.sidecar_file_id
 FROM photo_sidecar_considered c JOIN photo_files f ON f.file_id=c.sidecar_file_id JOIN photo_assets a ON a.asset_id=f.asset_id
 JOIN nodes n ON n.id=f.node_id WHERE `+photoXMPFenceMatchesSQL+`)`)
		return err
	})
}

// photoXMPSyncedNodesSQL lists originals with an XMP sync receipt from the
// receipt index alone, so a conflict search seeks them instead of every file.
const photoXMPSyncedNodesSQL = `SELECT json_extract(xr.after_json,'$.node_id') FROM photo_change_receipts xr INDEXED BY photo_change_receipts_xmp_node
 WHERE xr.operation='xmp_sync'`

// photoXMPLatestStateSQL is the outcome of the original's latest receipt.
const photoXMPLatestStateSQL = `(SELECT json_extract(xr.after_json,'$.state') FROM photo_change_receipts xr INDEXED BY photo_change_receipts_xmp_node
 WHERE xr.operation='xmp_sync' AND json_extract(xr.after_json,'$.node_id')=f.node_id+0 AND json_extract(xr.after_json,'$.file_id')=f.file_id
 ORDER BY xr.after_revision DESC LIMIT 1)`

func settlePhotoXMPFenceTx(ctx context.Context, tx *sql.Tx, fileID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_sidecar_considered(sidecar_file_id,asset_id,asset_revision,member_revisions,version_id,extractor_fingerprint)
 SELECT f.file_id,f.asset_id,a.revision,`+photoXMPFenceTokenSQL+`,n.current_version_id,`+photoXMPExtractorSQL+`
 FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE f.file_id=?
 ON CONFLICT(sidecar_file_id) DO UPDATE SET asset_id=excluded.asset_id,asset_revision=excluded.asset_revision,member_revisions=excluded.member_revisions,version_id=excluded.version_id,extractor_fingerprint=excluded.extractor_fingerprint`, fileID)
	return err
}

// latestPhotoXMPReceiptTx loads an original's latest sync outcome and its sequence.
func latestPhotoXMPReceiptTx(ctx context.Context, q metadataQuerier, f PhotoFile) (*photoXMPReceipt, int64, error) {
	var data []byte
	var sequence int64
	err := q.QueryRowContext(ctx, `SELECT after_json,after_revision FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_node
 WHERE operation='xmp_sync' AND json_extract(after_json,'$.node_id')=? AND json_extract(after_json,'$.file_id')=? ORDER BY after_revision DESC LIMIT 1`, f.NodeID, f.ID).Scan(&data, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	r, err := decodePhotoXMPReceipt(data)
	if err != nil {
		return nil, sequence, err
	}
	if r.FileID != f.ID || r.NodeID != f.NodeID {
		return nil, sequence, ErrInvalidPhotoAsset
	}
	return &r, sequence, nil
}

func photoXMPStateTx(ctx context.Context, tx *sql.Tx, f PhotoFile) (PhotoXMPState, error) {
	r := PhotoXMPState{State: PhotoXMPStateNone}
	var signals, settled, extracted bool
	err := tx.QueryRowContext(ctx, `SELECT `+photoXMPSignalsSQL+`,`+photoXMPFenceCurrentSQL+`,`+photoXMPExtractorSQL+`<>''
 FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE f.file_id=?`, f.ID).Scan(&signals, &settled, &extracted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !signals {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	latest, _, err := latestPhotoXMPReceiptTx(ctx, tx, f)
	switch {
	case err != nil && settled:
		return PhotoXMPState{State: PhotoXMPStateUnavailable, Reason: photoXMPReceiptReason}, nil
	case !settled:
		r.State = PhotoXMPStatePending
		if latest != nil && latest.State == PhotoXMPStatePending {
			r.Reason = latest.Reason
		} else if !extracted {
			r.Reason = photoXMPExtractionReason
		}
	case latest != nil:
		r.State, r.Reason, r.Rows, r.ConflictID = latest.State, latest.Reason, latest.Rows, latest.ConflictID
	}
	if latest == nil || latest.Sync == nil || latest.Sync.ImportReceiptID == "" {
		return r, nil
	}
	imported, err := loadPhotoAuthoredReceipt(ctx, tx, latest.Sync.ImportReceiptID)
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

// PhotoXMPSidecar is the one live sidecar attached to an original. Observed
// is the digest of the bytes last read from its outside source file.
type PhotoXMPSidecar struct {
	FileID   string
	Node     Node
	Observed string
	audited  bool
}

// PhotoXMPWork holds every input of one original's sync decision. Publication
// reloads it in its own transaction and refuses a plan made from other inputs.
type PhotoXMPWork struct {
	Original    PhotoFile
	Node        Node
	Keywords    []string
	Orientation int
	Sidecar     *PhotoXMPSidecar

	assetRevision          int64
	fence, extractor       string
	settled, extracted     bool
	keywordLimit, occupied bool
	auditActive, audited   bool
	members                int
	sidecarNames           []string
	receipt                *photoXMPReceipt
	sequence               int64
	receiptErr             bool
	intended               string
}

// synced returns the last successful sync of the current sidecar.
func (w PhotoXMPWork) synced() *photoXMPSync {
	if r := w.receipt; r != nil && r.Sync != nil && w.Sidecar != nil && r.Sync.Sidecar.FileID == w.Sidecar.FileID {
		return r.Sync
	}
	return nil
}

// Base returns the snapshot the current sidecar version descends from and the
// original's orientation then, or false before the sidecar's first sync.
func (w PhotoXMPWork) Base() (PhotoXMPSnapshot, int, bool) {
	r := w.synced()
	if r == nil {
		return PhotoXMPSnapshot{}, w.Orientation, false
	}
	if w.Sidecar.Node.CurrentVersionID != r.Sidecar.VersionID && r.Source != nil && w.Sidecar.Node.BlobHash == w.Sidecar.Observed {
		return *r.Source, r.Orientation, true
	}
	return r.Baseline, r.Orientation, true
}

// photoXMPObservedSQL is the digest of the version bound to a sidecar node's
// latest outside observation, or empty when that version was pruned.
const photoXMPObservedSQL = `SELECT COALESCE((SELECT v.blob_hash FROM provenance_version_bindings b
 JOIN content_versions v ON v.version_id=b.content_version_id WHERE b.provenance_identity=p.identity AND v.node_id=p.node_id),'')
 FROM provenance p JOIN ingests i ON i.id=p.ingest_id WHERE p.node_id=? AND i.source_kind NOT LIKE 'embedded:%'
 ORDER BY i.started_at DESC,p.identity DESC LIMIT 1`

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
	if !photoSidecarTargetRole(f.Role) || f.AssetID == "" {
		return w, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT a.revision,`+photoXMPFenceTokenSQL+`,`+photoXMPExtractorSQL+`,`+photoXMPFenceCurrentSQL+`,
 COALESCE((SELECT t.orientation FROM source_metadata_heads h JOIN photo_technical_metadata t ON t.generation_id=h.generation_id WHERE h.source_sha256=?),1),
 (SELECT count(*) FROM photo_files WHERE asset_id=f.asset_id),
 EXISTS(SELECT 1 FROM nodes o WHERE o.parent_id=n.parent_id AND o.name=n.name || '.xmp' AND o.trashed_at IS NULL),
 EXISTS(SELECT 1 FROM audit_authority),EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=f.node_id)
 FROM photo_files f JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id WHERE f.file_id=?`, n.BlobHash, id).Scan(
		&w.assetRevision, &w.fence, &w.extractor, &w.settled, &w.Orientation, &w.members, &w.occupied, &w.auditActive, &w.audited)
	if err != nil {
		return w, err
	}
	w.extracted = w.extractor != ""
	err = queryRows(ctx, tx, `SELECT `+nodeCols+`,sf.file_id,EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=n.id)
 FROM `+nodeFrom+` JOIN photo_files sf ON sf.node_id=n.id
 WHERE sf.sidecar_of_file_id=? AND sf.asset_id=? AND n.trashed_at IS NULL ORDER BY n.name,sf.file_id`, []any{f.ID, f.AssetID}, func(rows *sql.Rows) error {
		var sc PhotoXMPSidecar
		var err error
		sc.Node, err = scanNode(rows, &sc.FileID, &sc.audited)
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
	w.receipt, w.sequence, err = latestPhotoXMPReceiptTx(ctx, tx, f)
	if err != nil {
		w.receiptErr = true
	}
	if w.Sidecar == nil || len(w.sidecarNames) != 1 {
		return w, nil
	}
	err = tx.QueryRowContext(ctx, photoXMPObservedSQL, w.Sidecar.Node.ID).Scan(&w.Sidecar.Observed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return w, err
	}
	err = nil
	// A pruned or never-bound observation keeps the digest the last sync accepted.
	if synced := w.synced(); synced != nil && w.Sidecar.Observed == "" {
		w.Sidecar.Observed = synced.SourceSHA256
	}
	// Pairing evidence is an input: a synced sidecar must still name this original.
	if synced := w.synced(); synced != nil {
		w.intended, err = w.pairing(ctx, tx, synced.Extension)
	}
	return w, err
}

func (w PhotoXMPWork) pairing(ctx context.Context, tx *sql.Tx, extension string) (string, error) {
	return photoSidecarIntendedTarget(ctx, tx, PhotoFile{Role: PhotoRoleSidecar, AssetID: w.Original.AssetID, SidecarOfID: &w.Original.ID}, w.Sidecar.Node, extension)
}

const (
	photoXMPExtractionReason = "Source metadata extraction is pending. Wait for Background jobs, then refresh XMP."
	photoXMPReceiptReason    = "The saved XMP sync receipt could not be read."
)

// classify decides every outcome that needs no sidecar bytes. Pending means
// the worker must read and merge the packet; an empty state waits for
// extraction.
func (w PhotoXMPWork) classify() (string, string) {
	r := w.receipt
	catalogUnchanged := r != nil && r.Sync != nil && w.Original.Authored() == r.Sync.Baseline.Catalog && slices.Equal(w.Keywords, r.Sync.Baseline.Keywords)
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
		return PhotoXMPStateUnavailable, photoXMPReceiptReason
	case w.keywordLimit:
		return PhotoXMPStateInvalid, "Catalog keywords exceed the XMP limits. Remove keywords or shorten their names to resume sync."
	case len(w.sidecarNames) > 1:
		names := w.sidecarNames
		if len(names) > 5 {
			names = append(names[:5:5], fmt.Sprintf("%d more", len(w.sidecarNames)-5))
		}
		return PhotoXMPStateInvalid, "More than one XMP sidecar is attached: " + strings.Join(names, ", ") + ". Remove all but one sidecar to resume sync."
	case !w.extracted:
		return "", photoXMPExtractionReason
	case w.Sidecar == nil && w.members >= PhotoMaxFiles:
		return PhotoXMPStateInvalid, "This photo has 256 members. Detach a member so sync can create a sidecar."
	case w.Sidecar == nil && w.occupied:
		return PhotoXMPStateInvalid, "The sidecar name is already occupied. Rename or trash that file to resume sync."
	}
	if s := w.synced(); s != nil && catalogUnchanged && s.Sidecar.VersionID == w.Sidecar.Node.CurrentVersionID &&
		s.OriginalVersionID == w.Node.CurrentVersionID && s.Orientation == w.Orientation && w.intended == w.Original.ID && s.SourceSHA256 == w.Sidecar.Observed {
		return PhotoXMPStateSynced, ""
	}
	return PhotoXMPStatePending, ""
}

// ClassifyPhotoXMP settles an unsettled original that needs no sidecar bytes
// and reports whether the worker must still merge it.
func (s *Store) ClassifyPhotoXMP(ctx context.Context, id string) (w PhotoXMPWork, run bool, err error) {
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		w, err = loadPhotoXMPWorkTx(ctx, tx, id)
		if err != nil || w.settled {
			return err
		}
		state, reason := w.classify()
		if run = state == PhotoXMPStatePending; run || state == "" {
			return nil
		}
		return settlePhotoXMPTx(ctx, tx, w, photoXMPReceipt{State: state, Reason: reason})
	})
	return
}

// settlePhotoXMPTx records an outcome and settles the original's fence. An
// outcome equal to the latest receipt appends nothing; a Pending outcome
// stays unsettled so the worker retries it.
func settlePhotoXMPTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork, outcome photoXMPReceipt) error {
	if w.receiptErr {
		// A newer receipt would hide the unreadable one and its baseline.
		return settlePhotoXMPFenceTx(ctx, tx, w.Original.ID)
	}
	outcome.Version, outcome.FileID, outcome.NodeID = 1, w.Original.ID, w.Original.NodeID
	pending := outcome.State == PhotoXMPStatePending
	latest := w.receipt
	if outcome.Sync == nil && latest != nil {
		outcome.Sync = latest.Sync
	}
	if latest != nil && latest.State == PhotoXMPStateConflict && outcome.State == PhotoXMPStateConflict && reflect.DeepEqual(latest.Rows, outcome.Rows) {
		outcome.ConflictID = latest.ConflictID
	}
	if latest == nil && outcome.State == PhotoXMPStateNone || latest != nil && reflect.DeepEqual(*latest, outcome) {
		outcome.State = ""
	} else if outcome.State == PhotoXMPStateConflict && outcome.ConflictID == "" {
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		outcome.ConflictID = id
	}
	if outcome.State != "" {
		id, err := newUUIDv4()
		if err != nil {
			return err
		}
		if err := writePhotoReceiptTx(ctx, tx, photoReceipt{ReceiptID: id, Operation: "xmp_sync", BeforeRevision: w.sequence, AfterRevision: w.sequence + 1, After: outcome}); err != nil {
			return err
		}
	}
	if pending {
		return nil
	}
	return settlePhotoXMPFenceTx(ctx, tx, w.Original.ID)
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
		return w.conflict(conflictID)
	})
	return
}

func (w PhotoXMPWork) conflict(id string) error {
	if !w.settled || w.receipt == nil || w.receipt.State != PhotoXMPStateConflict || w.receipt.ConflictID != id {
		return ErrStaleRevision
	}
	return nil
}

// PhotoXMPTargetsAfter lists unsettled, extracted originals with sync work
// after a file ID cursor. File ID order follows the fence key, so an idle scan
// probes fence rows in index order.
func (s *Store) PhotoXMPTargetsAfter(ctx context.Context, after string, limit int) ([]PhotoSidecarTarget, error) {
	if limit < 1 || limit > maxBatchTagTargets {
		return nil, ErrInvalidPhotoAsset
	}
	result := []PhotoSidecarTarget{}
	err := queryRows(ctx, s.db, `SELECT f.file_id,f.node_id FROM `+photoXMPUnfencedSQL+` JOIN photo_assets a ON a.asset_id=f.asset_id JOIN nodes n ON n.id=f.node_id
 WHERE d.file_id>? AND `+photoXMPSignalsSQL+` AND `+photoXMPExtractorSQL+`<>''
 ORDER BY d.file_id LIMIT ?`, []any{after, limit}, func(rows *sql.Rows) error {
		var t PhotoSidecarTarget
		if err := rows.Scan(&t.FileID, &t.NodeID); err != nil {
			return err
		}
		result = append(result, t)
		return nil
	})
	return result, err
}

// PhotoSidecarTarget is one unsettled original for the sidecar worker.
type PhotoSidecarTarget struct {
	FileID string
	NodeID int64
}
