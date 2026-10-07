package store

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

type metadataPhotoAsset struct {
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
	Type        string  `json:"type"`
	FileID      string  `json:"file_id" db:"file_id"`
	AssetID     string  `json:"asset_id" db:"asset_id"`
	NodeID      int64   `json:"node_id" db:"node_id"`
	Role        string  `json:"role" db:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id" db:"sidecar_of_file_id"`
	CreatedAt   string  `json:"created_at" db:"created_at"`
}

type metadataPhotoSettings struct {
	Type       string  `json:"type"`
	Preference *string `json:"preference" db:"preference"`
	Revision   int64   `json:"revision" db:"revision"`
	UpdatedAt  string  `json:"updated_at" db:"updated_at"`
	Singleton  int     `json:"-" db:"singleton"`
}

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

// photoMetadataTables exports the photo records in dependency order.
var photoMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataPhotoAsset]{record: metadataPhotoAsset{Type: metadataPhotoAssetType},
		table: "photo_assets", suffix: "ORDER BY asset_id", validate: validatePhotoAssetMetadataRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoFile]{record: metadataPhotoFile{Type: metadataPhotoFileType},
		table: "photo_files", suffix: "ORDER BY file_id", validate: validatePhotoFileMetadataRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoSettings]{
		record: metadataPhotoSettings{Type: metadataPhotoSettingsType, Singleton: 1}, table: "photo_library_settings",
		suffix: "WHERE singleton=1", validate: validatePhotoSettingsMetadataRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoSet]{record: metadataPhotoSet{Type: "photo_set"}, table: "photo_sets", suffix: "ORDER BY set_id", validate: validatePhotoSetRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoSetMember]{record: metadataPhotoSetMember{Type: "photo_set_member"}, table: "photo_set_members", suffix: "ORDER BY set_id,added_at,asset_id", validate: validatePhotoSetMemberRecord, checkExport: true}),
	newMetadataTable(metadataTable[metadataPhotoReceipt]{record: metadataPhotoReceipt{Type: metadataPhotoReceiptType},
		table: "photo_change_receipts", suffix: "ORDER BY receipt_id", validate: validatePhotoReceiptMetadataRecord,
		checkExport: true}),
}

func validatePhotoAssetMetadataRecord(v metadataPhotoAsset) error {
	if v.Type != metadataPhotoAssetType || validateUUIDv4(v.AssetID) != nil || !photoKindValid(v.Kind) || v.Revision < 1 {
		return errors.New("invalid photo asset metadata")
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
	if v.Type != metadataPhotoFileType || validateUUIDv4(v.FileID) != nil || validateUUIDv4(v.AssetID) != nil || v.NodeID < 1 || !photoRoleValid(v.Role) {
		return errors.New("invalid photo file metadata")
	}
	if v.Role == PhotoRoleSidecar && v.SidecarOfID == nil {
		return errors.New("invalid photo sidecar pointer")
	}
	if v.SidecarOfID != nil && validateUUIDv4(*v.SidecarOfID) != nil {
		return errors.New("invalid photo sidecar pointer")
	}
	return validateMetadataTime("photo file created_at", v.CreatedAt)
}

func validatePhotoSettingsMetadataRecord(v metadataPhotoSettings) error {
	if v.Type != metadataPhotoSettingsType || v.Revision < 1 || !photoPreferenceValid(v.Preference) {
		return errors.New("invalid photo settings metadata")
	}
	if v.UpdatedAt != "" {
		return validateMetadataTime("photo settings updated_at", v.UpdatedAt)
	}
	return nil
}

func validatePhotoReceiptMetadataRecord(v metadataPhotoReceipt) error {
	if v.Type != metadataPhotoReceiptType || validateUUIDv4(v.ReceiptID) != nil || v.BeforeRevision < 0 || v.AfterRevision < 0 || len(v.BeforeJSON) > maxPhotoReceiptBytes || len(v.AfterJSON) > maxPhotoReceiptBytes {
		return errors.New("invalid photo receipt metadata")
	}
	switch v.Operation {
	case "set_create", "set_duplicate", "set_update", "set_add", "set_remove", "set_delete":
		if v.SetID == nil || validateUUIDv4(*v.SetID) != nil || v.AssetID != nil || v.SettingsKey != nil {
			return errors.New("invalid photo set receipt identity")
		}
	case "create", "promote", "attach", "detach", "exclude", "display", "purge", "settings_recompute", "import":
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

func validatePhotoMetadataState(ctx context.Context, tx metadataQuerier) error {
	if err := validatePhotoGraph(ctx, tx); err != nil {
		return err
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM photo_change_receipts r
		LEFT JOIN photo_assets a ON a.asset_id=r.asset_id
 LEFT JOIN photo_sets ps ON ps.set_id=r.set_id
		WHERE (r.set_id IS NOT NULL AND ps.set_id IS NULL) OR (r.asset_id IS NOT NULL AND a.asset_id IS NULL)
		   OR (r.settings_key IS NOT NULL AND NOT EXISTS (SELECT 1 FROM photo_library_settings))`).Scan(&orphans); err != nil {
		return fmt.Errorf("checking photo receipt references: %w", err)
	}
	if orphans > 0 {
		return fmt.Errorf("%w: %d photo receipts reference missing assets or settings", ErrInvalidPhotoAsset, orphans)
	}
	if err := validatePhotoSetGraph(ctx, tx); err != nil {
		return err
	}
	for _, table := range photoMetadataTables {
		if err := table.validateRows(ctx, tx); err != nil {
			return fmt.Errorf("validating photo metadata: %w", err)
		}
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
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_set_members m LEFT JOIN photo_sets s ON s.set_id=m.set_id LEFT JOIN photo_assets a ON a.asset_id=m.asset_id WHERE s.set_id IS NULL OR a.asset_id IS NULL`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid > 0 {
		return errors.New("dangling photo set membership")
	}
	return nil
}
