package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
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
	PhotoAuthored `json:"values"`

	Present     PhotoAuthoredFields `json:"present"`
	Ignored     PhotoAuthoredFields `json:"ignored,omitzero"`
	Rotation    *int                `json:"rotation,omitzero"`
	Keywords    []string            `json:"keywords"`
	Orientation int                 `json:"orientation"`
	// LegacyReject marks a sidecar that spells reject as rating -1, which is no unsupported rating.
	LegacyReject bool `json:"legacy_reject,omitzero"`
}

// PhotoXMPSnapshot pairs a packet with its per-property catalog ancestors.
type PhotoXMPSnapshot struct {
	Digest            string         `json:"digest"`
	VersionID         string         `json:"version_id"`
	SourceOrientation int            `json:"source_orientation"`
	Packet            PhotoXMPPacket `json:"packet"`
	Catalog           PhotoAuthored  `json:"catalog"`
	Keywords          []string       `json:"keywords"`
}

// PhotoXMPSource is the latest packet read from one outside source file.
type PhotoXMPSource struct {
	PhotoXMPSnapshot

	Path string `json:"path"`
}

type PhotoXMPRow struct {
	Field   string `json:"field"`
	Catalog string `json:"catalog"`
	Sidecar string `json:"sidecar"`
}

// photoXMPSync retains semantic histories, without duplicating packet bytes.
type photoXMPSync struct {
	Sidecar         PhotoSidecarProvenance `json:"sidecar"`
	Settled         PhotoXMPSnapshot       `json:"settled"`
	Outside         []PhotoXMPSource       `json:"outside,omitzero"`
	WrittenVersion  string                 `json:"written_version,omitzero"`
	ImportReceiptID string                 `json:"import_receipt_id,omitzero"`
}

// photoXMPRefusalReserve is the room a refusal needs beside the history it keeps: its
// fields and a reason of at most maxPhotoXMPReasonBytes. A receipt that is not itself a
// refusal stays that much under the limit, so recording a refusal never fails.
const (
	photoXMPRefusalReserve = 4 << 10
	maxPhotoXMPReasonBytes = 1 << 10
	// photoXMPRemovalReserve holds the largest removal record: three texts of control
	// characters that JSON writes as six bytes each, and its fixed fields.
	photoXMPRemovalReserve = 3*6*MaxPhotoAuthoredTextBytes + 1<<10
)

// photoXMPReceiptLimit is the size a receipt may reach. Any receipt that is not a refusal
// leaves room for a refusal and for the largest removal record a later detach can write.
func photoXMPReceiptLimit(after any) int {
	r, ok := after.(photoXMPReceipt)
	if ok && (r.State == PhotoXMPStateInvalid || r.State == PhotoXMPStateUnavailable) {
		return maxBatchTagReceiptJSONBytes
	}
	limit := maxBatchTagReceiptJSONBytes - photoXMPRefusalReserve - photoXMPRemovalReserve
	if ok && r.Removed != nil {
		removed, _ := json.Marshal(r.Removed, json.Deterministic(true))
		limit += len(removed) + len(`,"removed":`)
	}
	return limit
}

// boundPhotoXMPReason cuts a refusal reason to a size every receipt has room for.
func boundPhotoXMPReason(reason string) string {
	if len(reason) <= maxPhotoXMPReasonBytes {
		return reason
	}
	return strings.ToValidUTF8(reason[:maxPhotoXMPReasonBytes], "")
}

// photoXMPFold is what the copies compared before a conflict left in the plan.
// A choice resumes from it, so it shows and acts on the same catalog; it is
// used only while the inputs still match its token, which a retryable failure keeps.
type photoXMPFold struct {
	Catalog   PhotoAuthored    `json:"catalog"`
	Keywords  []string         `json:"keywords"`
	Outside   []PhotoXMPSource `json:"outside,omitzero"`
	Suppliers []string         `json:"suppliers,omitzero"`
	Token     string           `json:"token"`
}

// PhotoXMPRemoval records decisions when the last sidecar was removed.
type PhotoXMPRemoval struct {
	Catalog            PhotoAuthored `json:"catalog"`
	KeywordFingerprint string        `json:"keyword_fingerprint"`
}

type photoXMPReceipt struct {
	Version          int                 `json:"version"`
	FileID           string              `json:"file_id"`
	NodeID           int64               `json:"node_id"`
	Token            string              `json:"token"`
	State            string              `json:"state"`
	Reason           string              `json:"reason,omitzero"`
	Conflicts        PhotoAuthoredFields `json:"conflicts,omitzero"`
	ConflictValues   *PhotoAuthored      `json:"conflict_values,omitzero"`
	ConflictKeywords []string            `json:"conflict_keywords,omitzero"`
	// ConflictPath is the outside source whose version conflicts, empty for Docbank's own.
	ConflictPath string `json:"conflict_path,omitzero"`
	// ConflictFileID is the sidecar file the conflict compares; no other file adopts it.
	ConflictFileID string           `json:"conflict_file_id,omitzero"`
	Fold           *photoXMPFold    `json:"fold,omitzero"`
	Sync           *photoXMPSync    `json:"sync,omitzero"`
	Removed        *PhotoXMPRemoval `json:"removed,omitzero"`
}

// unmarshalPhotoXMPReceipt decodes a receipt and refuses versions this build
// does not understand.
func unmarshalPhotoXMPReceipt(data []byte, r *photoXMPReceipt) error {
	if err := json.Unmarshal(data, r, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%w: decoding XMP receipt: %w", ErrInvalidPhotoAsset, err)
	}
	if r.Version != 1 {
		return fmt.Errorf("%w: unsupported XMP receipt version %d", ErrInvalidPhotoAsset, r.Version)
	}
	return nil
}

func decodePhotoXMPReceipt(data []byte) (photoXMPReceipt, error) {
	var r photoXMPReceipt
	if len(data) > maxBatchTagReceiptJSONBytes {
		return r, ErrPhotoEditReceiptLimit
	}
	if err := unmarshalPhotoXMPReceipt(data, &r); err != nil {
		return r, err
	}
	return r, validatePhotoXMPReceipt(r)
}

func validatePhotoXMPReceipt(r photoXMPReceipt) error {
	if validateUUIDv4(r.FileID) != nil || r.NodeID < 1 || len(r.Reason) > MaxPhotoAuthoredTextBytes {
		return ErrInvalidPhotoAsset
	}
	if !validPhotoXMPReceiptState(r.State) || r.State == PhotoXMPStateSynced && r.Sync == nil {
		return ErrInvalidPhotoAsset
	}
	if err := validatePhotoXMPConflictReceipt(r); err != nil {
		return err
	}
	if r.Removed != nil && (ValidatePhotoAuthored(r.Removed.Catalog) != nil || validateCatalogSHA256(r.Removed.KeywordFingerprint, "removed keyword fingerprint") != nil) {
		return ErrInvalidPhotoAsset
	}
	if r.Sync != nil {
		return validatePhotoXMPSync(*r.Sync)
	}
	return nil
}

func validPhotoXMPReceiptState(state string) bool {
	switch state {
	case PhotoXMPStateNone, PhotoXMPStatePending, PhotoXMPStateSynced, PhotoXMPStateConflict, PhotoXMPStateUnavailable, PhotoXMPStateInvalid:
		return true
	}
	return false
}

func validatePhotoXMPConflictReceipt(r photoXMPReceipt) error {
	conflict := r.Conflicts != 0
	if conflict != (r.ConflictValues != nil) || r.State == PhotoXMPStateConflict && !conflict {
		return ErrInvalidPhotoAsset
	}
	if r.Conflicts&^(PhotoConfirmedAll|PhotoXMPKeywordsBit) != 0 || r.ConflictPath != "" && !conflict || conflict && validateUUIDv4(r.ConflictFileID) != nil {
		return ErrInvalidPhotoAsset
	}
	if r.Fold != nil && (!conflict || validatePhotoXMPFold(*r.Fold) != nil) {
		return ErrInvalidPhotoAsset
	}
	if r.ConflictValues != nil && (ValidatePhotoAuthoredValues(*r.ConflictValues) != nil || r.Conflicts&PhotoXMPKeywordsBit != 0 && ValidatePhotoXMPKeywords(r.ConflictKeywords) != nil) {
		return ErrInvalidPhotoAsset
	}
	return nil
}

func validatePhotoXMPSync(sync photoXMPSync) error {
	if validatePhotoXMPSnapshots(&sync.Settled, sync.Outside) != nil {
		return ErrInvalidPhotoAsset
	}
	if sync.Settled.VersionID != sync.Sidecar.VersionID || sync.Sidecar.NodeID < 1 || validateUUIDv4(sync.Sidecar.FileID) != nil || validateUUIDv4(sync.Sidecar.VersionID) != nil {
		return ErrInvalidPhotoAsset
	}
	if sync.ImportReceiptID != "" && validateUUIDv4(sync.ImportReceiptID) != nil || sync.WrittenVersion != "" && validateUUIDv4(sync.WrittenVersion) != nil {
		return ErrInvalidPhotoAsset
	}
	return nil
}

// validatePhotoXMPSnapshots checks the settled snapshot, when there is one, and every source's.
func validatePhotoXMPSnapshots(settled *PhotoXMPSnapshot, outside []PhotoXMPSource) error {
	snapshots := []*PhotoXMPSnapshot{}
	if settled != nil {
		snapshots = append(snapshots, settled)
	}
	paths := map[string]bool{}
	for i := range outside {
		path := outside[i].Path
		if path == "" || paths[path] {
			return ErrInvalidPhotoAsset
		}
		paths[path] = true
		snapshots = append(snapshots, &outside[i].PhotoXMPSnapshot)
	}
	for _, s := range snapshots {
		if _, err := packstore.ParseHash(s.Digest); err != nil || validateUUIDv4(s.VersionID) != nil || s.SourceOrientation < 1 || s.SourceOrientation > 8 {
			return ErrInvalidPhotoAsset
		}
		if ValidatePhotoAuthored(s.Catalog) != nil || ValidatePhotoXMPKeywords(s.Keywords) != nil || validatePhotoXMPPacket(s.Packet) != nil {
			return ErrInvalidPhotoAsset
		}
	}
	return nil
}

func validatePhotoXMPFold(f photoXMPFold) error {
	if ValidatePhotoAuthored(f.Catalog) != nil || ValidatePhotoXMPKeywords(f.Keywords) != nil || slices.ContainsFunc(f.Suppliers, func(id string) bool { return validateUUIDv4(id) != nil }) {
		return ErrInvalidPhotoAsset
	}
	return validatePhotoXMPSnapshots(nil, f.Outside)
}

func ValidatePhotoXMPKeywords(names []string) error {
	if names == nil || len(names) > MaxPhotoXMPKeywords || !slices.Equal(names, PhotoXMPKeywords(names)) {
		return ErrInvalidPhotoAsset
	}
	for _, name := range names {
		if len(name) > MaxPhotoAuthoredTextBytes {
			return fmt.Errorf("XMP keyword exceeds %d UTF-8 bytes", MaxPhotoAuthoredTextBytes)
		}
		canonical, err := NormalizeTagName(name)
		if err != nil || canonical != name {
			return ErrInvalidPhotoAsset
		}
		if err := ValidatePhotoXMPText("keyword", name); err != nil {
			return err
		}
	}
	data, err := json.Marshal(names)
	if err != nil || len(data) > document.MaxSourceMetadataValueBytes {
		return fmt.Errorf("XMP keywords exceed %d encoded bytes", document.MaxSourceMetadataValueBytes)
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

// PhotoXMPChangedFields includes confirmation-only decisions, such as an explicit clear.
func PhotoXMPChangedFields(a, b PhotoAuthored) PhotoAuthoredFields {
	var mask PhotoAuthoredFields
	for _, field := range photoAuthoredFields {
		if !field.sameState(&a, &b) {
			mask |= field.bit
		}
	}
	return mask
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
	State           string        `json:"state" enum:"none,synced,pending,conflict,unavailable,invalid"`
	Reason          string        `json:"reason,omitzero"`
	Rows            []PhotoXMPRow `json:"rows,omitzero"`
	ConflictID      string        `json:"conflict_id,omitzero"`
	ImportReceiptID string        `json:"import_receipt_id,omitzero" doc:"The sidecar import that Undo can still reverse."`
}

// Candidate discovery narrows the shared view to assets queued by schema triggers.
const photoXMPCandidatesSQL = `photo_xmp_unsettled u CROSS JOIN photo_xmp_inputs f ON f.asset_id=u.asset_id`

func validatePhotoXMPPacket(p PhotoXMPPacket) error {
	if (p.Present|p.Ignored)&^(PhotoConfirmedAll|PhotoXMPKeywordsBit) != 0 || !p.Confirmed.Valid() {
		return ErrInvalidPhotoAsset
	}
	return validatePhotoXMPPacketValues(p)
}

func validatePhotoXMPPacketValues(p PhotoXMPPacket) error {
	if ValidatePhotoAuthoredValues(p.PhotoAuthored) != nil || ValidatePhotoXMPKeywords(p.Keywords) != nil {
		return ErrInvalidPhotoAsset
	}
	if p.Orientation < 0 || p.Orientation > 8 || p.Rotation != nil && !slices.Contains([]int{0, 90, 180, 270}, *p.Rotation) {
		return ErrInvalidPhotoAsset
	}
	return nil
}

func photoXMPStateTx(ctx context.Context, tx *sql.Tx, f PhotoFile, undo bool) (PhotoXMPState, error) {
	w, err := loadPhotoXMPWorkTx(ctx, tx, f.ID)
	if err != nil {
		return PhotoXMPState{}, err
	}
	if w.receiptErr {
		return PhotoXMPState{State: PhotoXMPStateUnavailable, Reason: photoXMPReceiptReason}, nil
	}
	latest, receiptID := w.receipt, w.receiptID
	r := PhotoXMPState{State: PhotoXMPStateNone}
	if latest != nil {
		r.State, r.Reason = latest.State, latest.Reason
	}
	eligible := w.eligible
	if eligible && !w.settled {
		r.State, r.Reason = PhotoXMPStatePending, ""
		if !w.extracted {
			r.Reason = photoXMPExtractionReason
		}
	}
	if latest != nil && latest.State == PhotoXMPStatePending && r.Reason == "" {
		r.Reason = latest.Reason
	}
	if w.Conflicts != 0 {
		r.Rows, r.ConflictID = w.conflictRows(), receiptID
	}
	if !undo || latest == nil || latest.Sync == nil || latest.Sync.ImportReceiptID == "" {
		return r, nil
	}
	imported, err := loadPhotoAuthoredReceipt(ctx, tx, latest.Sync.ImportReceiptID)
	if errors.Is(err, ErrNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	for _, fence := range imported.KeywordNodes {
		node, err := nodeByIDQuery(ctx, tx, fence.NodeID)
		if err != nil {
			return r, err
		}
		if node.Revision != fence.AfterRevision {
			return r, nil
		}
	}
	for _, after := range imported.After {
		if after.FileID == f.ID && after.Revision == f.Revision {
			r.ImportReceiptID = imported.ReceiptID
		}
	}
	return r, nil
}

// PhotoXMPSidecar is a live packet attached to an original.
type PhotoXMPSidecar struct {
	FileID  string
	Node    Node
	audited bool
}

// PhotoXMPWork holds every input of one original's sync decision. Publication
// reloads it in its own transaction and refuses a plan made from other inputs.
type PhotoXMPWork struct {
	Original           PhotoFile
	Node               Node
	Keywords           []string
	KeywordFingerprint string
	Orientation        int
	// ClaimExtension is the delivered original's extension, which a new sidecar claims.
	ClaimExtension string
	Sidecar        *PhotoXMPSidecar
	History        *photoXMPSync
	Removal        *PhotoXMPRemoval
	Conflicts      PhotoAuthoredFields
	ConflictPath   string
	// Heads are the outside sources with a version the receipt has not seen,
	// oldest observation first.
	Heads []PhotoXMPHead

	// fold is what the copies before the open conflict left, while its inputs are unchanged.
	fold                         *photoXMPFold
	token                        string
	receiptID                    string
	settled, extracted, eligible bool
	occupied                     bool
	keywordError                 string
	auditActive, audited         bool
	members, sidecars            int
	receipt                      *photoXMPReceipt
	receiptErr                   bool
}

func loadPhotoXMPWorkTx(ctx context.Context, tx metadataQuerier, id string) (PhotoXMPWork, error) {
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
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT token,extracted,eligible,pending,orientation,members,occupied,audit_active,audited
 FROM photo_xmp_inputs WHERE file_id=?`, id).Scan(
		&w.token, &w.extracted, &w.eligible, &pending, &w.Orientation, &w.members, &w.occupied, &w.auditActive, &w.audited)
	if err != nil {
		return w, err
	}
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT after_json,receipt_id FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_file WHERE operation='xmp_sync' AND CAST(json_extract(after_json,'$.file_id') AS TEXT)=?`, f.ID).Scan(&data, &w.receiptID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return w, err
	}
	if err == nil {
		receipt, err := decodePhotoXMPReceipt(data)
		w.receipt, w.receiptErr = &receipt, err != nil
	}
	w.settled = !pending
	sc := new(PhotoXMPSidecar)
	sc.Node, err = scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeCols+`,sf.file_id,EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=n.id), (SELECT count(*) FROM photo_files cf JOIN nodes cn ON cn.id=cf.node_id WHERE cf.sidecar_of_file_id=sf.sidecar_of_file_id AND cf.asset_id=sf.asset_id AND cn.trashed_at IS NULL) FROM `+nodeFrom+` JOIN photo_files sf ON sf.node_id=n.id WHERE sf.sidecar_of_file_id=? AND sf.asset_id=?
 ORDER BY n.trashed_at IS NOT NULL,sf.file_id=COALESCE((SELECT json_extract(after_json,'$.sync.sidecar.file_id') FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_file WHERE operation='xmp_sync' AND CAST(json_extract(after_json,'$.file_id') AS TEXT)=?), '') DESC,n.id LIMIT 1`, f.ID, f.AssetID, f.ID), &sc.FileID, &sc.audited, &w.sidecars)
	if err == nil {
		if sc.Node.TrashedAt == nil {
			w.Sidecar = sc
		}
	} else if !errors.Is(err, ErrNotFound) {
		return w, err
	}
	if w.Sidecar == nil {
		if w.ClaimExtension, err = photoDeliveredExtensionTx(ctx, tx, f, n); err != nil {
			return w, err
		}
	}

	w.Keywords, w.KeywordFingerprint, err = photoKeywordNames(ctx, tx, n.ID)
	if err != nil && !errors.Is(err, bundle.ErrLimit) {
		return w, err
	}
	if err == nil {
		err = ValidatePhotoXMPKeywords(w.Keywords)
	}
	if err != nil {
		w.Keywords, w.keywordError = []string{}, err.Error()
	}
	if w.receipt != nil {
		w.Removal = w.receipt.Removed
		w = w.openConflict()
		if w.receipt.Sync != nil && (w.Sidecar == nil || w.receipt.Sync.Sidecar.FileID == w.Sidecar.FileID) {
			w.History = w.receipt.Sync
		}
	}
	return w, nil
}

// photoDeliveredExtensionTx is the extension of the name checkout delivers an original under.
func photoDeliveredExtensionTx(ctx context.Context, q metadataQuerier, f PhotoFile, n Node) (string, error) {
	assets, err := json.Marshal([]string{f.AssetID})
	if err != nil {
		return "", err
	}
	sources, err := photoSourcesTx(ctx, q, string(assets), true)
	return strings.TrimPrefix(path.Ext(photoDeliveredName(sources, f.ID, n.Name)), "."), err
}

// openConflict adopts the receipt's choice binding and fold when the sidecar is the file
// the conflict compares, or none is attached.
func (w PhotoXMPWork) openConflict() PhotoXMPWork {
	if r := w.receipt; w.Sidecar == nil || w.Sidecar.FileID == r.ConflictFileID {
		w.Conflicts, w.ConflictPath, w.fold = r.Conflicts, r.ConflictPath, w.foldFor(r.Fold)
	}
	return w
}

// foldFor returns the fold while the inputs still match the token it was made against.
func (w PhotoXMPWork) foldFor(f *photoXMPFold) *photoXMPFold {
	if w.Sidecar != nil && f != nil && f.Token == w.token {
		return f
	}
	return nil
}

// catalog is what a choice acts on: the catalog after the copies compared so far.
func (w PhotoXMPWork) catalog() (PhotoAuthored, []string) {
	if w.fold != nil {
		return w.fold.Catalog, w.fold.Keywords
	}
	return w.Original.Authored(), w.Keywords
}

// outside lists the sources' packets, as the compared copies left them.
func (w PhotoXMPWork) outside() []PhotoXMPSource {
	switch {
	case w.fold != nil:
		return w.fold.Outside
	case w.History != nil:
		return w.History.Outside
	}
	return nil
}

func (w PhotoXMPWork) conflictRows() []PhotoXMPRow {
	catalog, keywords := w.catalog()
	return PhotoXMPConflictRows(catalog, keywords, PhotoXMPPacket{PhotoAuthored: *w.receipt.ConflictValues, Keywords: w.receipt.ConflictKeywords}, w.receipt.Conflicts)
}

// loadPhotoXMPSyncWorkTx also lists the sources a sync must compare; status
// does not need them.
func loadPhotoXMPSyncWorkTx(ctx context.Context, tx *sql.Tx, id string) (w PhotoXMPWork, err error) {
	if w, err = loadPhotoXMPWorkTx(ctx, tx, id); err == nil && w.Sidecar != nil {
		w.Heads, err = photoXMPHeadsTx(ctx, tx, w)
	}
	return
}

func photoXMPSourceIndex(entries []PhotoXMPSource, path string) int {
	return slices.IndexFunc(entries, func(s PhotoXMPSource) bool { return s.Path == path })
}

const (
	photoXMPExtractionReason = "Source metadata has not been extracted. If the source bytes are missing, restore them. Extraction and XMP sync retry automatically."
	photoXMPReceiptReason    = "The saved XMP sync receipt could not be read."
	photoXMPOccupiedReason   = "The sidecar name is already occupied. Rename or trash that file to resume sync."
)

// classify decides every outcome that needs no sidecar bytes. Pending means
// the worker must read and merge the packet; an empty state waits for
// extraction.
func (w PhotoXMPWork) classify() (string, string) {
	r := w.receipt
	catalogUnchanged := w.Removal != nil && w.Original.Authored() == w.Removal.Catalog && w.KeywordFingerprint == w.Removal.KeywordFingerprint
	switch {
	case !w.eligible:
		return PhotoXMPStateNone, ""
	case w.Sidecar == nil && r != nil && r.Conflicts != 0:
		return PhotoXMPStateUnavailable, "The conflicting sidecar is detached or trashed. Restore or reattach it to choose."
	case w.Sidecar == nil && (r == nil || r.Sync == nil) && w.Original.Authored().Confirmed == 0 && len(w.Keywords) == 0 && w.keywordError == "":
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
	case w.sidecars > 1:
		return PhotoXMPStateInvalid, "More than one XMP sidecar is attached to this original. Detach or trash all but one to resume sync."
	case w.keywordError != "":
		return PhotoXMPStateInvalid, "Catalog keywords: " + w.keywordError + ". Shorten or remove keywords to resume sync."
	case !w.extracted:
		return "", photoXMPExtractionReason
	case w.Sidecar == nil && w.members >= PhotoMaxFiles:
		return PhotoXMPStateInvalid, "This photo has 256 members. Detach a member so sync can create a sidecar."
	case w.Sidecar == nil && w.occupied:
		return PhotoXMPStateInvalid, photoXMPOccupiedReason
	}
	return PhotoXMPStatePending, ""
}

// ClassifyPhotoXMP settles an unsettled original that needs no sidecar bytes
// and reports whether the worker must still merge it.
func (s *Store) ClassifyPhotoXMP(ctx context.Context, id string) (w PhotoXMPWork, run bool, err error) {
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		w, err = s.resumeDetachedPhotoXMPTx(ctx, tx, id)
		if err != nil || !w.eligible || w.settled || w.receiptErr {
			return err
		}
		state, reason := w.classify()
		if run = state == PhotoXMPStatePending; run || state == "" {
			return nil
		}
		outcome := photoXMPReceipt{State: state, Reason: reason}
		return settlePhotoXMPTx(ctx, tx, w, outcome)
	})
	return
}

// resumeDetachedPhotoXMPTx reuses a generated sidecar when a later edit reopens
// sync and that original's detached file still occupies the generated name.
func (s *Store) resumeDetachedPhotoXMPTx(ctx context.Context, tx *sql.Tx, id string) (PhotoXMPWork, error) {
	w, err := loadPhotoXMPSyncWorkTx(ctx, tx, id)
	if err != nil || w.settled || w.receiptErr || w.Removal == nil || w.History == nil {
		return w, err
	}
	if _, reason := w.classify(); reason != photoXMPOccupiedReason {
		return w, nil
	}
	sidecar := w.History.Sidecar
	var detached bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_files f JOIN nodes n ON n.id=f.node_id
 WHERE f.file_id=? AND n.id=? AND f.asset_id IS NULL AND n.trashed_at IS NULL
 AND n.parent_id=? AND n.name=? AND EXISTS(SELECT 1 FROM provenance p JOIN ingests i ON i.id=p.ingest_id
 WHERE p.node_id=n.id AND i.source_kind=? AND p.original_path=?))`,
		sidecar.FileID, sidecar.NodeID, w.Node.ParentID, w.Node.Name+".xmp", callerSuppliedSourceKindPrefix+"photo-xmp", "docbank:xmp:"+w.Original.ID).Scan(&detached)
	if err != nil || !detached {
		return w, err
	}
	before, err := photoAssetByIDQuery(ctx, tx, w.Original.AssetID)
	if err != nil {
		return w, err
	}
	if err := s.insertPhotoFileTx(ctx, tx, PhotoFile{AssetID: before.ID, NodeID: sidecar.NodeID, Role: PhotoRoleSidecar, SidecarOfID: &w.Original.ID}); err != nil {
		return w, err
	}
	if _, err := commitPhotoAssetTx(ctx, tx, before, before, "attach"); err != nil {
		return w, err
	}
	if err := validatePhotoAssetGraph(ctx, tx, before.ID); err != nil {
		return w, err
	}
	return loadPhotoXMPSyncWorkTx(ctx, tx, id)
}

// settlePhotoXMPTx records the outcome atomically.
func settlePhotoXMPTx(ctx context.Context, tx *sql.Tx, w PhotoXMPWork, outcome photoXMPReceipt) error {
	if w.receiptErr {
		return ErrInvalidPhotoAsset
	}
	outcome.Version, outcome.FileID, outcome.NodeID = 1, w.Original.ID, w.Original.NodeID
	if latest := w.receipt; latest != nil {
		if outcome.Sync == nil {
			outcome.Sync = latest.Sync
		}
		if outcome.Removed == nil && outcome.State != PhotoXMPStateSynced {
			outcome.Removed = latest.Removed
		}
		if latest.Conflicts != 0 && outcome.State != PhotoXMPStateSynced && outcome.State != PhotoXMPStateConflict {
			outcome.Conflicts, outcome.ConflictKeywords, outcome.ConflictValues, outcome.ConflictPath, outcome.ConflictFileID, outcome.Fold = latest.Conflicts, latest.ConflictKeywords, latest.ConflictValues, latest.ConflictPath, latest.ConflictFileID, latest.Fold
		}
	}
	// Imports and new versions change the inputs inside this same transaction.
	if err := tx.QueryRowContext(ctx, `SELECT token FROM photo_xmp_inputs WHERE file_id=?`, w.Original.ID).Scan(&outcome.Token); err != nil {
		return err
	}
	if outcome.State == PhotoXMPStatePending {
		outcome.Token = ""
	}
	if reflect.DeepEqual(w.receipt, &outcome) {
		return nil
	}
	if err := deletePhotoXMPReceiptTx(ctx, tx, w.Original.ID); err != nil {
		return err
	}
	return writePhotoReceiptTx(ctx, tx, photoReceipt{AssetID: w.Original.AssetID, Operation: "xmp_sync", AfterRevision: 1, After: outcome})
}

func deletePhotoXMPReceiptTx(ctx context.Context, tx *sql.Tx, fileID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_file WHERE operation='xmp_sync' AND CAST(json_extract(after_json,'$.file_id') AS TEXT)=?`, fileID)
	return err
}

// PhotoXMPConflict loads a recorded conflict for a person's choice.
func (s *Store) PhotoXMPConflict(ctx context.Context, id, conflictID string) (w PhotoXMPWork, err error) {
	err = s.photoReadTx(ctx, func(tx *sql.Tx) error {
		w, err = loadPhotoXMPSyncWorkTx(ctx, tx, id)
		if errors.Is(err, ErrNotFound) || err == nil && w.Original.AssetID == "" {
			return ErrStaleRevision
		}
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
	if !w.eligible || !w.settled || w.receipt == nil || w.receipt.State != PhotoXMPStateConflict || w.receiptID != id {
		return ErrStaleRevision
	}
	return nil
}

// The queue is keyed by asset, so a cursor seeks it and a page reads only the assets it returns.
const photoXMPTargetsQuery = `SELECT f.file_id,f.node_id,f.asset_id FROM ` + photoXMPCandidatesSQL + `
 WHERE u.asset_id>=?1 AND f.eligible AND f.pending AND f.extracted AND (f.asset_id>?1 OR f.file_id>?2)
 ORDER BY u.asset_id,f.file_id LIMIT ?3`

// PhotoXMPTargetsAfter pages unsettled originals in the daemon's existing pages;
// after is a PhotoSidecarTarget's Cursor, or empty for the first page.
func (s *Store) PhotoXMPTargetsAfter(ctx context.Context, after string, limit int) ([]PhotoSidecarTarget, error) {
	if limit < 1 || limit > maxBatchTagTargets {
		return nil, ErrInvalidPhotoAsset
	}
	assetID, fileID, _ := strings.Cut(after, "|")
	result := []PhotoSidecarTarget{}
	err := queryRows(ctx, s.db, photoXMPTargetsQuery, []any{assetID, fileID, limit}, func(rows *sql.Rows) error {
		var t PhotoSidecarTarget
		if err := rows.Scan(&t.FileID, &t.NodeID, &t.AssetID); err != nil {
			return err
		}
		result = append(result, t)
		return nil
	})
	return result, err
}

type PhotoSidecarTarget struct {
	FileID  string
	NodeID  int64
	AssetID string
}

// Cursor orders targets by asset, then original.
func (t PhotoSidecarTarget) Cursor() string { return t.AssetID + "|" + t.FileID }

// photoXMPRemembersTx reports whether an original has decisions, keywords, or a
// sync receipt to remember; any other has nothing to record at removal.
func photoXMPRemembersTx(ctx context.Context, tx *sql.Tx, fileID string) (remembers bool, err error) {
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM photo_files f WHERE f.file_id=? AND f.asset_id IS NOT NULL AND (f.confirmed_fields<>0
 OR EXISTS(SELECT 1 FROM node_tags t WHERE t.node_id=f.node_id)
 OR EXISTS(SELECT 1 FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_file WHERE operation='xmp_sync' AND CAST(json_extract(after_json,'$.file_id') AS TEXT)=f.file_id)))`, fileID).Scan(&remembers)
	return
}

// recordPhotoXMPDetachedTx captures decisions at removal, so later edits reopen
// sync even when the worker never saw the attached packet.
// Detach captures before membership changes; trash records only after the last live sidecar disappears.
func recordPhotoXMPDetachedTx(ctx context.Context, tx *sql.Tx, fileID string, beforeDetach bool) error {
	if remembers, err := photoXMPRemembersTx(ctx, tx, fileID); err != nil || !remembers {
		return err
	}
	w, err := loadPhotoXMPWorkTx(ctx, tx, fileID)
	if err != nil || w.Sidecar != nil && !beforeDetach || w.Original.AssetID == "" {
		return err
	}
	state := PhotoXMPStateNone
	if w.receipt != nil && w.receipt.Conflicts != 0 {
		state = PhotoXMPStateUnavailable
	}
	outcome := photoXMPReceipt{State: state, Removed: &PhotoXMPRemoval{Catalog: w.Original.Authored(), KeywordFingerprint: w.KeywordFingerprint}}
	return settlePhotoXMPTx(ctx, tx, w, outcome)
}

// PhotoXMPConflictRows derives display values; receipts retain semantic keys only.
func PhotoXMPConflictRows(catalog PhotoAuthored, keywords []string, sidecar PhotoXMPPacket, mask PhotoAuthoredFields) []PhotoXMPRow {
	rows := []PhotoXMPRow{}
	for _, field := range photoAuthoredFields {
		if mask&field.bit == 0 {
			continue
		}
		rows = append(rows, PhotoXMPRow{Field: strings.ToUpper(field.name[:1]) + field.name[1:], Catalog: field.text(&catalog), Sidecar: field.text(&sidecar.PhotoAuthored)})
	}
	if mask&PhotoXMPKeywordsBit != 0 {
		rows = append(rows, PhotoXMPRow{Field: "Keywords", Catalog: strings.Join(keywords, ", "), Sidecar: strings.Join(sidecar.Keywords, ", ")})
	}
	return rows
}
