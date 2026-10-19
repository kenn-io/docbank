package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
)

type metadataPhotoAsset struct {
	Type                  string  `json:"type"`
	AssetID               string  `json:"asset_id" db:"asset_id"`
	Kind                  string  `json:"kind" db:"kind"`
	Revision              int64   `json:"revision" db:"revision"`
	HiddenAt              *string `json:"hidden_at,omitempty" db:"hidden_at"`
	ExcludedAt            *string `json:"excluded_at" db:"excluded_at"`
	DisplayFileID         *string `json:"display_file_id" db:"display_file_id"`
	DisplayOverrideFileID *string `json:"display_override_file_id" db:"display_override_file_id"`
	CreatedAt             string  `json:"created_at" db:"created_at"`
	UpdatedAt             string  `json:"updated_at" db:"updated_at"`
	RepresentativeRank    *int64  `json:"representative_rank,omitempty" db:"representative_rank"`
}

type metadataPhotoAssetBeforeHidden struct {
	Type                  string  `json:"type"`
	AssetID               string  `json:"asset_id" db:"asset_id"`
	Kind                  string  `json:"kind" db:"kind"`
	Revision              int64   `json:"revision" db:"revision"`
	ExcludedAt            *string `json:"excluded_at" db:"excluded_at"`
	DisplayFileID         *string `json:"display_file_id" db:"display_file_id"`
	DisplayOverrideFileID *string `json:"display_override_file_id" db:"display_override_file_id"`
	CreatedAt             string  `json:"created_at" db:"created_at"`
	UpdatedAt             string  `json:"updated_at" db:"updated_at"`
}

type metadataPhotoFile struct {
	SourceProvenanceIdentity *string             `json:"source_provenance_identity,omitempty" db:"source_provenance_identity"`
	FlagOwner                string              `json:"flag_owner" db:"flag_owner"`
	Confirmed                PhotoAuthoredFields `json:"confirmed_fields" db:"confirmed_fields"`
	Revision                 int64               `json:"revision" db:"revision"`
	Rating                   int                 `json:"rating" db:"rating"`
	Flag                     string              `json:"flag" db:"flag"`
	Label                    string              `json:"label" db:"label"`
	Caption                  string              `json:"caption" db:"caption"`
	Creator                  string              `json:"creator" db:"creator"`
	Copyright                string              `json:"copyright" db:"copyright"`
	Rotation                 int                 `json:"rotation" db:"rotation"`

	Type        string  `json:"type"`
	FileID      string  `json:"file_id" db:"file_id"`
	AssetID     *string `json:"asset_id" db:"asset_id"`
	NodeID      int64   `json:"node_id" db:"node_id"`
	Role        string  `json:"role" db:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id" db:"sidecar_of_file_id"`
	CreatedAt   string  `json:"created_at" db:"created_at"`
}

type metadataPhotoFileBeforeAuthored struct {
	Type        string  `json:"type"`
	FileID      string  `json:"file_id" db:"file_id"`
	AssetID     string  `json:"asset_id" db:"asset_id"`
	NodeID      int64   `json:"node_id" db:"node_id"`
	Role        string  `json:"role" db:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id" db:"sidecar_of_file_id"`
	CreatedAt   string  `json:"created_at" db:"created_at"`
}

func (v metadataPhotoFileBeforeAuthored) current() metadataPhotoFile {
	return metadataPhotoFile{Type: v.Type, FileID: v.FileID, AssetID: new(v.AssetID), NodeID: v.NodeID, Role: v.Role, SidecarOfID: v.SidecarOfID, CreatedAt: v.CreatedAt, Revision: 1}
}

var photoFileBeforeAuthoredMetadata = newMetadataTable(metadataTable[metadataPhotoFileBeforeAuthored]{record: metadataPhotoFileBeforeAuthored{Type: metadataPhotoFileType}, table: "photo_files", suffix: "ORDER BY file_id", validate: func(v metadataPhotoFileBeforeAuthored) error { return validatePhotoFileMetadataRecord(v.current()) }, checkExport: true})

func normalizePhotoFileMetadata(raw jsontext.Value) (jsontext.Value, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"revision", "rating", "flag", "label", "caption", "creator", "copyright", "rotation"} {
		if _, exists := fields[name]; exists {
			return raw, nil
		}
	}
	required, nullable := photoFileBeforeAuthoredMetadata.fields()
	if err := requireMetadataFields(raw, required, nullable); err != nil {
		return nil, err
	}
	var old metadataPhotoFileBeforeAuthored
	if err := json.Unmarshal(raw, &old, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	return json.Marshal(old.current(), json.Deterministic(true))
}

// A missing burst gap restores as NULL, the 2-second default.
type metadataPhotoSettings struct {
	Type                   string  `json:"type"`
	Preference             *string `json:"preference" db:"preference"`
	Revision               int64   `json:"revision" db:"revision"`
	UpdatedAt              string  `json:"updated_at" db:"updated_at"`
	BurstGapSeconds        *int    `json:"burst_gap_seconds,omitempty" db:"burst_gap_seconds"`
	RepresentativeSequence *int64  `json:"representative_sequence" db:"representative_sequence"`
	Singleton              int     `json:"-" db:"singleton"`
}

// Released settings tables predate the burst gap column.
type metadataPhotoSettingsBeforeBurst struct {
	Type       string  `json:"type"`
	Preference *string `json:"preference" db:"preference"`
	Revision   int64   `json:"revision" db:"revision"`
	UpdatedAt  string  `json:"updated_at" db:"updated_at"`
	// Released tables have no choices; their current-format export starts at 0.
	RepresentativeSequence int64 `json:"representative_sequence"`
	Singleton              int   `json:"-" db:"singleton"`
}

var photoSettingsBeforeBurstMetadata = newMetadataTable(metadataTable[metadataPhotoSettingsBeforeBurst]{
	record: metadataPhotoSettingsBeforeBurst{Type: metadataPhotoSettingsType, Singleton: 1}, table: "photo_library_settings",
	suffix: "WHERE singleton=1", validate: func(v metadataPhotoSettingsBeforeBurst) error {
		return validatePhotoSettingsMetadataRecord(metadataPhotoSettings{Type: v.Type, Preference: v.Preference, Revision: v.Revision, UpdatedAt: v.UpdatedAt, Singleton: v.Singleton})
	}, checkExport: true,
})

type metadataPhotoSet struct {
	Type         string  `json:"type"`
	ID           string  `json:"set_id" db:"set_id"`
	Name         string  `json:"name" db:"name"`
	Starred      bool    `json:"starred" db:"starred"`
	Revision     int64   `json:"revision" db:"revision"`
	CoverAssetID *string `json:"cover_asset_id" db:"cover_asset_id"`
	CreatedAt    string  `json:"created_at" db:"created_at"`
	UpdatedAt    string  `json:"updated_at" db:"updated_at"`
	DeletedAt    *string `json:"deleted_at" db:"deleted_at"`
}

type metadataPhotoSetMember struct {
	Type    string `json:"type"`
	SetID   string `json:"set_id" db:"set_id"`
	AssetID string `json:"asset_id" db:"asset_id"`
	AddedAt string `json:"added_at" db:"added_at"`
}

type metadataPhotoReceipt struct {
	Type           string  `json:"type"`
	ReceiptID      string  `json:"receipt_id" db:"receipt_id"`
	Operation      string  `json:"operation" db:"operation"`
	AssetID        *string `json:"asset_id" db:"asset_id"`
	SettingsKey    *string `json:"settings_key" db:"settings_key"`
	SetID          *string `json:"set_id,omitempty" db:"set_id"`
	BeforeRevision int64   `json:"before_revision" db:"before_revision"`
	AfterRevision  int64   `json:"after_revision" db:"after_revision"`
	BeforeJSON     string  `json:"before_json" db:"before_json"`
	AfterJSON      string  `json:"after_json" db:"after_json"`
	CreatedAt      string  `json:"created_at" db:"created_at"`
}

// metadataPhotoReceiptV28 reads v0.15.0 receipts, which predate photo sets.
// Its JSON matches a current receipt whose set_id is absent.
type metadataPhotoReceiptV28 struct {
	Type           string  `json:"type"`
	ReceiptID      string  `json:"receipt_id" db:"receipt_id"`
	Operation      string  `json:"operation" db:"operation"`
	AssetID        *string `json:"asset_id" db:"asset_id"`
	SettingsKey    *string `json:"settings_key" db:"settings_key"`
	BeforeRevision int64   `json:"before_revision" db:"before_revision"`
	AfterRevision  int64   `json:"after_revision" db:"after_revision"`
	BeforeJSON     string  `json:"before_json" db:"before_json"`
	AfterJSON      string  `json:"after_json" db:"after_json"`
	CreatedAt      string  `json:"created_at" db:"created_at"`
}

var (
	photoAssetMetadata = newMetadataTable(metadataTable[metadataPhotoAsset]{
		record: metadataPhotoAsset{Type: metadataPhotoAssetType}, table: "photo_assets", suffix: "ORDER BY asset_id",
		validate: validatePhotoAssetMetadataRecord, checkExport: true})
	photoFileMetadata = newMetadataTable(metadataTable[metadataPhotoFile]{
		record: metadataPhotoFile{Type: metadataPhotoFileType}, table: "photo_files", suffix: "ORDER BY file_id",
		validate: validatePhotoFileMetadataRecord, checkExport: true})
	photoSettingsMetadata = newMetadataTable(metadataTable[metadataPhotoSettings]{
		record: metadataPhotoSettings{Type: metadataPhotoSettingsType, Singleton: 1}, table: "photo_library_settings",
		suffix: "WHERE singleton=1", validate: validatePhotoSettingsMetadataRecord, checkExport: true})
)

// importPhotoSettingsBeforeV3 defaults an absent sequence while preserving
// explicit null and numeric values through the current typed codec.
func importPhotoSettingsBeforeV3(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	required, nullable := photoSettingsMetadata.fields()
	required = slices.DeleteFunc(slices.Clone(required), func(field string) bool { return field == "representative_sequence" })
	if err := requireMetadataFields(raw, required, nullable); err != nil {
		return err
	}
	codec := *photoSettingsMetadata
	codec.record.RepresentativeSequence = new(int64(0))
	return codec.importRecord(ctx, tx, raw)
}

// photoMetadataTables exports the photo records in dependency order.
var photoMetadataTables = []metadataRecordCodec{
	photoAssetMetadata, photoFileMetadata, photoSettingsMetadata,
	newMetadataTable(metadataTable[metadataPhotoSet]{record: metadataPhotoSet{Type: "photo_set"}, table: "photo_sets", suffix: "ORDER BY set_id", validate: validatePhotoSetRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoSetMember]{record: metadataPhotoSetMember{Type: "photo_set_member"}, table: "photo_set_members", suffix: "ORDER BY set_id,added_at,asset_id", validate: validatePhotoSetMemberRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoReceipt]{record: metadataPhotoReceipt{Type: metadataPhotoReceiptType},
		table: "photo_change_receipts", suffix: "ORDER BY receipt_id", validate: validatePhotoReceiptMetadataRecord,
		checkExport: true}),
}

// photoMetadataTablesV28 exports the photo records of v0.15.0, which predate
// photo sets.
var photoMetadataTablesV28 = []metadataRecordCodec{
	photoAssetMetadata, photoFileBeforeAuthoredMetadata, photoSettingsMetadata,
	newMetadataTable(metadataTable[metadataPhotoReceiptV28]{record: metadataPhotoReceiptV28{Type: metadataPhotoReceiptType},
		table: "photo_change_receipts", suffix: "ORDER BY receipt_id", validate: func(v metadataPhotoReceiptV28) error {
			return validatePhotoReceiptMetadataRecord(metadataPhotoReceipt{
				Type: v.Type, ReceiptID: v.ReceiptID, Operation: v.Operation, AssetID: v.AssetID,
				SettingsKey: v.SettingsKey, BeforeRevision: v.BeforeRevision, AfterRevision: v.AfterRevision,
				BeforeJSON: v.BeforeJSON, AfterJSON: v.AfterJSON, CreatedAt: v.CreatedAt,
			})
		}, checkExport: true}),
}

// photoSetsStorageSchemaVersion is the first schema with photo sets (v0.15.1).
const photoSetsStorageSchemaVersion = 29

const photoHiddenStorageSchemaVersion = 30

const photoAuthoredStorageSchemaVersion = 32

func validatePhotoAssetMetadataRecord(v metadataPhotoAsset) error {
	if v.Type != metadataPhotoAssetType || validateUUIDv4(v.AssetID) != nil || !photoKindValid(v.Kind) || v.Revision < 1 || v.RepresentativeRank != nil && *v.RepresentativeRank < 1 {
		return errors.New("invalid photo asset metadata")
	}
	if v.HiddenAt != nil {
		if err := validateMetadataTime("photo asset hidden_at", *v.HiddenAt); err != nil {
			return err
		}
	}
	if v.ExcludedAt != nil {
		if err := validateMetadataTime("photo asset excluded_at", *v.ExcludedAt); err != nil {
			return err
		}
	}
	if err := validateMetadataTime("photo asset created_at", v.CreatedAt); err != nil {
		return err
	}
	return validateMetadataTime("photo asset updated_at", v.UpdatedAt)
}

func validatePhotoFileMetadataRecord(v metadataPhotoFile) error {
	if v.SourceProvenanceIdentity != nil && validateCatalogSHA256(*v.SourceProvenanceIdentity, "photo source provenance identity") != nil {
		return errors.New("invalid photo source provenance identity")
	}
	if v.Type != metadataPhotoFileType || validateUUIDv4(v.FileID) != nil || v.AssetID != nil && validateUUIDv4(*v.AssetID) != nil || v.NodeID < 1 || !photoRoleValid(v.Role) {
		return errors.New("invalid photo file metadata")
	}
	if v.AssetID == nil && v.SidecarOfID != nil {
		return errors.New("detached photo file has a sidecar pointer")
	}
	if v.AssetID != nil && v.Role == PhotoRoleSidecar && v.SidecarOfID == nil {
		return errors.New("invalid photo sidecar pointer")
	}
	if v.SidecarOfID != nil && validateUUIDv4(*v.SidecarOfID) != nil {
		return errors.New("invalid photo sidecar pointer")
	}
	if v.Revision < 1 {
		return errors.New("invalid photo file revision")
	}
	values := PhotoAuthored{FlagOwner: v.FlagOwner, Confirmed: v.Confirmed, Rating: v.Rating, Flag: v.Flag, Label: v.Label, Caption: v.Caption, Creator: v.Creator, Copyright: v.Copyright, Rotation: v.Rotation}
	if err := ValidatePhotoAuthored(values); err != nil {
		return err
	}
	// Every decision advances the revision; sidecar initialization treats revision 1 as undecided.
	if v.Revision == 1 && values != (PhotoAuthored{}) {
		return errors.New("photo file at revision 1 has authored decisions")
	}
	return validateMetadataTime("photo file created_at", v.CreatedAt)
}

func validatePhotoSettingsMetadataRecord(v metadataPhotoSettings) error {
	if v.RepresentativeSequence != nil && *v.RepresentativeSequence < 0 {
		return errors.New("invalid photo representative sequence")
	}
	if v.Type != metadataPhotoSettingsType || v.Revision < 1 || !photoPreferenceValid(v.Preference) || v.BurstGapSeconds != nil && (*v.BurstGapSeconds < 1 || *v.BurstGapSeconds > 30) {
		return errors.New("invalid photo settings metadata")
	}
	if v.UpdatedAt != "" {
		return validateMetadataTime("photo settings updated_at", v.UpdatedAt)
	}
	return nil
}

func validatePhotoReceiptMetadataRecord(v metadataPhotoReceipt) error {
	limit := maxBatchTagReceiptJSONBytes
	if slices.Contains([]string{"authored", "authored_undo", "authored_sidecar"}, v.Operation) {
		limit = maxPhotoAuthoredReceiptJSONBytes
	}
	if v.Type != metadataPhotoReceiptType || validateUUIDv4(v.ReceiptID) != nil || v.BeforeRevision < 0 || v.AfterRevision < 0 || len(v.BeforeJSON)+len(v.AfterJSON) > limit {
		return errors.New("invalid photo receipt metadata")
	}
	if v.Operation == "checkout_commit" {
		return validateCheckoutCommitReceipt(v)
	}
	if v.Operation == "face_decision" {
		if v.AssetID == nil || validateUUIDv4(*v.AssetID) != nil || v.SettingsKey != nil || v.SetID != nil || v.BeforeRevision != 0 || v.AfterRevision != 1 {
			return ErrInvalidPhotoFace
		}
		_, err := decodeFaceDecisionReceipt([]byte(v.BeforeJSON), []byte(v.AfterJSON), v.ReceiptID)
		if err != nil {
			return err
		}
		return validateMetadataTime("photo receipt created_at", v.CreatedAt)
	}
	if slices.Contains([]string{"authored", "authored_undo", "authored_sidecar"}, v.Operation) {
		if v.AssetID != nil || v.SettingsKey != nil || v.SetID != nil || v.BeforeRevision != 0 || v.AfterRevision != 1 {
			return errors.New("invalid authored receipt identity")
		}
		r, err := decodePhotoAuthoredReceipt([]byte(v.BeforeJSON), []byte(v.AfterJSON), v.ReceiptID)
		if err != nil {
			return err
		}
		if (v.Operation == "authored_undo") != (r.UndoOf != "") || (v.Operation == "authored_sidecar") != (r.Sidecar != nil) {
			return errors.New("authored receipt provenance differs from operation")
		}
		return validateMetadataTime("photo receipt created_at", v.CreatedAt)
	}
	if len(v.BeforeJSON) > maxPhotoReceiptBytes || len(v.AfterJSON) > maxPhotoReceiptBytes {
		return errors.New("photo graph receipt too large")
	}
	switch v.Operation {
	case "set_create", "set_duplicate", "set_update", "set_add", "set_remove", "set_delete":
		if v.SetID == nil || validateUUIDv4(*v.SetID) != nil || v.AssetID != nil || v.SettingsKey != nil {
			return errors.New("invalid photo set receipt identity")
		}
	case "create", "promote", "attach", "detach", "hide", "unhide", "exclude", "display", "purge", "settings_recompute", "import", "trash", "restore":
		if v.AssetID == nil || v.SettingsKey != nil || v.SetID != nil {
			return errors.New("invalid photo receipt asset/settings identity")
		}
	case "settings":
		if v.AssetID != nil || v.SetID != nil || v.SettingsKey == nil || *v.SettingsKey != "library" {
			return errors.New("invalid photo settings receipt identity")
		}
	default:
		return fmt.Errorf("invalid photo receipt operation %q", v.Operation)
	}
	if v.AfterRevision != v.BeforeRevision+1 || (v.Operation == "create" || v.Operation == "set_create" || v.Operation == "set_duplicate") && v.BeforeRevision != 0 ||
		v.Operation != "create" && v.Operation != "promote" && v.Operation != "set_create" && v.Operation != "set_duplicate" && v.BeforeRevision < 1 {
		return errors.New("invalid photo receipt revision transition")
	}
	if v.AssetID != nil && validateUUIDv4(*v.AssetID) != nil {
		return errors.New("invalid photo receipt asset")
	}
	if !jsontext.Value(v.BeforeJSON).IsValid() || !jsontext.Value(v.AfterJSON).IsValid() {
		return errors.New("invalid photo receipt JSON")
	}
	return validateMetadataTime("photo receipt created_at", v.CreatedAt)
}

func validatePhotoMetadataState(ctx context.Context, tx metadataQuerier, version int) error {
	if err := validatePhotoGraph(ctx, tx); err != nil {
		return err
	}
	if version >= photoCheckoutsStorageSchemaVersion {
		var invalid int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_files f LEFT JOIN provenance p ON p.identity=f.source_provenance_identity LEFT JOIN ingests i ON i.id=p.ingest_id
 WHERE f.source_provenance_identity IS NOT NULL AND (p.identity IS NULL OR p.node_id<>f.node_id OR i.source_kind LIKE 'embedded:%')`).Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return errors.New("invalid photo source provenance reference")
		}
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM photo_change_receipts r
		LEFT JOIN photo_assets a ON a.asset_id=r.asset_id
		WHERE (r.asset_id IS NOT NULL AND a.asset_id IS NULL)
		   OR (r.settings_key IS NOT NULL AND NOT EXISTS (SELECT 1 FROM photo_library_settings))`).Scan(&orphans); err != nil {
		return fmt.Errorf("checking photo receipt references: %w", err)
	}
	if orphans > 0 {
		return fmt.Errorf("%w: %d photo receipts reference missing assets or settings", ErrInvalidPhotoAsset, orphans)
	}
	if version >= photoSetsStorageSchemaVersion {
		if err := validatePhotoSetGraph(ctx, tx); err != nil {
			return err
		}
	}
	for _, table := range photoMetadataTablesForSchema(version) {
		if err := table.validateRows(ctx, tx); err != nil {
			return fmt.Errorf("validating photo metadata: %w", err)
		}
	}
	if version >= photoCheckoutsStorageSchemaVersion {
		if err := validatePhotoCheckoutReferences(ctx, tx); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts r
 LEFT JOIN photo_checkouts c ON c.checkout_id=json_extract(r.after_json,'$.checkout_id')
 LEFT JOIN photo_checkout_entries e ON e.checkout_id=c.checkout_id AND e.file_id=json_extract(r.after_json,'$.file_id') AND e.asset_id=r.asset_id
 WHERE r.operation='checkout_commit' AND (c.checkout_id IS NULL OR e.file_id IS NULL)`).Scan(&orphans); err != nil {
			return fmt.Errorf("checking checkout commit receipt references: %w", err)
		}
		if orphans > 0 {
			return fmt.Errorf("%w: %d checkout commit receipts reference missing checkouts or entries", ErrInvalidPhotoCheckout, orphans)
		}
	}
	if version >= photoAuthoredStorageSchemaVersion {
		if err := validatePhotoAuthoredReferences(ctx, tx); err != nil {
			return err
		}
	}
	if version >= photoBurstStorageSchemaVersion {
		var behind bool
		if err := tx.QueryRowContext(ctx, `SELECT
 COALESCE((SELECT representative_sequence FROM photo_library_settings WHERE singleton=1),0) < COALESCE(MAX(rank),0)
 FROM (
 SELECT representative_rank AS rank FROM photo_assets
 UNION ALL
 SELECT MAX(COALESCE(json_extract(entry.value,'$.before'),0),COALESCE(json_extract(entry.value,'$.after'),0))
 FROM photo_change_receipts r, json_each(r.after_json,'$.representatives') entry
 WHERE r.operation IN ('authored','authored_undo')
 )`).Scan(&behind); err != nil {
			return fmt.Errorf("checking photo representative sequence: %w", err)
		}
		if behind {
			return errors.New("photo representative sequence is below retained ranks")
		}
	}
	return nil
}

func validatePhotoAuthoredReferences(ctx context.Context, q metadataQuerier) error {
	var invalid int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts r,
 json_each(r.after_json,'$.after') entry
 LEFT JOIN photo_files f ON f.file_id=json_extract(entry.value,'$.file_id')
 WHERE r.operation IN ('authored','authored_undo','authored_sidecar')
 AND (f.file_id IS NULL OR f.node_id<>json_extract(entry.value,'$.node_id'))`).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("checking authored receipt references: %w", err)
	}
	if invalid != 0 {
		return errors.New("authored receipts reference missing or different file nodes")
	}
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts r,
 json_each(r.after_json,'$.representatives') entry
 LEFT JOIN photo_assets a ON a.asset_id=json_extract(entry.value,'$.asset_id')
 WHERE r.operation IN ('authored','authored_undo') AND a.asset_id IS NULL`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return errors.New("representative receipts reference missing photo assets")
	}

	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts r
 LEFT JOIN photo_files sidecar ON sidecar.file_id=json_extract(r.after_json,'$.sidecar.file_id')
 LEFT JOIN content_versions version ON version.version_id=json_extract(r.after_json,'$.sidecar.version_id')
 WHERE r.operation='authored_sidecar' AND
 (sidecar.file_id IS NULL OR sidecar.node_id<>json_extract(r.after_json,'$.sidecar.node_id')
 OR (version.version_id IS NOT NULL AND version.node_id<>json_extract(r.after_json,'$.sidecar.node_id')))`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return errors.New("authored receipt sidecar provenance references missing or different nodes")
	}
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_change_receipts r
 LEFT JOIN photo_change_receipts original ON original.receipt_id=json_extract(r.after_json,'$.undo_of')
 WHERE r.operation='authored_undo' AND (original.receipt_id IS NULL OR original.receipt_id=r.receipt_id OR original.operation NOT IN ('authored','authored_undo','authored_sidecar'))`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return errors.New("authored undo references an invalid receipt")
	}
	return nil
}

func validatePhotoSetRecord(v metadataPhotoSet) error {
	if v.Type != "photo_set" || validateUUIDv4(v.ID) != nil || !validPhotoSetName(v.Name) || v.Revision < 1 {
		return errors.New("invalid photo set metadata")
	}
	if v.CoverAssetID != nil && validateUUIDv4(*v.CoverAssetID) != nil {
		return errors.New("invalid photo set cover")
	}
	if v.DeletedAt != nil {
		if err := validateMetadataTime("photo set deleted_at", *v.DeletedAt); err != nil {
			return err
		}
	}
	if err := validateMetadataTime("photo set created_at", v.CreatedAt); err != nil {
		return err
	}
	return validateMetadataTime("photo set updated_at", v.UpdatedAt)
}

func validatePhotoSetMemberRecord(v metadataPhotoSetMember) error {
	if v.Type != "photo_set_member" || validateUUIDv4(v.SetID) != nil || validateUUIDv4(v.AssetID) != nil {
		return errors.New("invalid photo set member")
	}
	return validateMetadataTime("photo set member added_at", v.AddedAt)
}

func validatePhotoSetGraph(ctx context.Context, q metadataQuerier) error {
	var invalid int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_sets s WHERE
 (s.deleted_at IS NOT NULL AND (s.cover_asset_id IS NOT NULL OR EXISTS(SELECT 1 FROM photo_set_members m WHERE m.set_id=s.set_id)))
 OR (s.cover_asset_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM photo_set_members m WHERE m.set_id=s.set_id AND m.asset_id=s.cover_asset_id))`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid > 0 {
		return errors.New("invalid photo set membership or cover")
	}
	return nil
}

func photoMetadataTablesForSchema(version int) []metadataRecordCodec {
	tables := photoMetadataTables
	if version < photoSetsStorageSchemaVersion {
		tables = photoMetadataTablesV28
	}
	tables = append([]metadataRecordCodec(nil), tables...)
	for i, table := range tables {
		if version < photoBurstStorageSchemaVersion && table.kind() == metadataPhotoSettingsType {
			tables[i] = photoSettingsBeforeBurstMetadata
		}
		if version < photoAuthoredStorageSchemaVersion && table.kind() == metadataPhotoFileType {
			tables[i] = photoFileBeforeAuthoredMetadata
		}
		if version < photoHiddenStorageSchemaVersion && table.kind() == metadataPhotoAssetType {
			tables[i] = photoAssetMetadataBeforeHidden
		}
	}
	if version >= photoCheckoutsStorageSchemaVersion {
		tables = append(tables, photoCheckoutMetadataTables...)
	}
	if version >= photoHiddenStorageSchemaVersion {
		return append(tables, photoHiddenMetadataTables...)
	}
	return tables
}

type metadataHiddenCredential struct {
	Type      string `json:"type"`
	Singleton int    `json:"-" db:"singleton"`
	Hash      string `json:"passcode_hash" db:"passcode_hash"`
}

type metadataHiddenFailure struct {
	Type       string `json:"type"`
	ID         int64  `json:"failure_id" db:"failure_id"`
	OccurredAt string `json:"occurred_at" db:"occurred_at"`
}

type metadataHiddenLockout struct {
	Type        string `json:"type"`
	Singleton   int    `json:"-" db:"singleton"`
	LockedUntil string `json:"locked_until" db:"locked_until"`
}

var photoHiddenMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataHiddenCredential]{record: metadataHiddenCredential{Type: "photo_hidden_credential", Singleton: 1}, table: "photo_hidden_credentials", suffix: "WHERE singleton=1", validate: func(v metadataHiddenCredential) error { _, _, err := hiddenHashParts(v.Hash); return err }, checkExport: true}),
	newMetadataTable(metadataTable[metadataHiddenFailure]{record: metadataHiddenFailure{Type: "photo_hidden_failure"}, table: "photo_hidden_failures", suffix: "ORDER BY failure_id", validate: func(v metadataHiddenFailure) error {
		if v.ID < 1 {
			return errors.New("invalid hidden failure ID")
		}
		return validateMetadataTime("hidden failure occurred_at", v.OccurredAt)
	}, checkExport: true}),
	newMetadataTable(metadataTable[metadataHiddenLockout]{record: metadataHiddenLockout{Type: "photo_hidden_lockout", Singleton: 1}, table: "photo_hidden_lockout", suffix: "WHERE singleton=1", validate: func(v metadataHiddenLockout) error {
		return validateMetadataTime("hidden lockout locked_until", v.LockedUntil)
	}, checkExport: true}),
}

var photoAssetMetadataBeforeHidden = newMetadataTable(metadataTable[metadataPhotoAssetBeforeHidden]{record: metadataPhotoAssetBeforeHidden{Type: metadataPhotoAssetType}, table: "photo_assets", suffix: "ORDER BY asset_id", validate: func(v metadataPhotoAssetBeforeHidden) error {
	return validatePhotoAssetMetadataRecord(metadataPhotoAsset{Type: v.Type, AssetID: v.AssetID, Kind: v.Kind, Revision: v.Revision, ExcludedAt: v.ExcludedAt, DisplayFileID: v.DisplayFileID, DisplayOverrideFileID: v.DisplayOverrideFileID, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt})
}, checkExport: true})
