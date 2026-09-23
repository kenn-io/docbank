package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

type metadataPhotoAsset struct {
	Type                  string  `json:"type"`
	AssetID               string  `json:"asset_id"`
	Kind                  string  `json:"kind"`
	Revision              int64   `json:"revision"`
	ExcludedAt            *string `json:"excluded_at"`
	DisplayFileID         *string `json:"display_file_id"`
	DisplayOverrideFileID *string `json:"display_override_file_id"`
	CreatedAt             string  `json:"created_at"`
	UpdatedAt             string  `json:"updated_at"`
}

type metadataPhotoFile struct {
	Type        string  `json:"type"`
	FileID      string  `json:"file_id"`
	AssetID     string  `json:"asset_id"`
	NodeID      int64   `json:"node_id"`
	Role        string  `json:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id"`
	CreatedAt   string  `json:"created_at"`
}

type metadataPhotoSettings struct {
	Type       string  `json:"type"`
	Preference *string `json:"preference"`
	Revision   int64   `json:"revision"`
	UpdatedAt  string  `json:"updated_at"`
}

type metadataPhotoReceipt struct {
	Type           string  `json:"type"`
	ReceiptID      string  `json:"receipt_id"`
	Operation      string  `json:"operation"`
	AssetID        *string `json:"asset_id"`
	SettingsKey    *string `json:"settings_key"`
	BeforeRevision int64   `json:"before_revision"`
	AfterRevision  int64   `json:"after_revision"`
	BeforeJSON     string  `json:"before_json"`
	AfterJSON      string  `json:"after_json"`
	CreatedAt      string  `json:"created_at"`
}

func exportPhotoMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT asset_id, kind, revision, excluded_at, display_file_id,
		       display_override_file_id, created_at, updated_at
		FROM photo_assets ORDER BY asset_id`)
	if err != nil {
		return fmt.Errorf("exporting photo assets: %w", err)
	}
	for rows.Next() {
		var record metadataPhotoAsset
		if err := rows.Scan(&record.AssetID, &record.Kind, &record.Revision, &record.ExcludedAt, &record.DisplayFileID, &record.DisplayOverrideFileID, &record.CreatedAt, &record.UpdatedAt); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return err
		}
		record.Type = metadataPhotoAssetType
		if err := validatePhotoAssetMetadataRecord(record); err != nil {
			_ = rows.Close()
			return err
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT file_id, asset_id, node_id, role, sidecar_of_file_id, created_at
		FROM photo_files ORDER BY file_id`)
	if err != nil {
		return fmt.Errorf("exporting photo files: %w", err)
	}
	for rows.Next() {
		var record metadataPhotoFile
		if err := rows.Scan(&record.FileID, &record.AssetID, &record.NodeID, &record.Role, &record.SidecarOfID, &record.CreatedAt); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return err
		}
		record.Type = metadataPhotoFileType
		if err := validatePhotoFileMetadataRecord(record); err != nil {
			_ = rows.Close()
			return err
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var settings metadataPhotoSettings
	var preference sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT preference, revision, updated_at FROM photo_library_settings WHERE singleton=1`).Scan(&preference, &settings.Revision, &settings.UpdatedAt)
	if err == nil {
		settings.Type = metadataPhotoSettingsType
		if preference.Valid {
			settings.Preference = new(preference.String)
		}
		if err := validatePhotoSettingsMetadataRecord(settings); err != nil {
			return err
		}
		if err := write(settings); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("exporting photo settings: %w", err)
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT receipt_id, operation, asset_id, settings_key, before_revision,
		       after_revision, before_json, after_json, created_at
		FROM photo_change_receipts ORDER BY receipt_id`)
	if err != nil {
		return fmt.Errorf("exporting photo receipts: %w", err)
	}
	for rows.Next() {
		var record metadataPhotoReceipt
		var assetID, settingsKey sql.NullString
		if err := rows.Scan(&record.ReceiptID, &record.Operation, &assetID, &settingsKey, &record.BeforeRevision, &record.AfterRevision, &record.BeforeJSON, &record.AfterJSON, &record.CreatedAt); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
			return err
		}
		if assetID.Valid {
			record.AssetID = new(assetID.String)
		}
		if settingsKey.Valid {
			record.SettingsKey = new(settingsKey.String)
		}
		record.Type = metadataPhotoReceiptType
		if err := validatePhotoReceiptMetadataRecord(record); err != nil {
			_ = rows.Close()
			return err
		}
		if err := write(record); err != nil {
			_ = rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
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
	case "create", "promote", "attach", "detach", "exclude", "display", "purge", "settings_recompute":
		if v.AssetID == nil || v.SettingsKey != nil {
			return errors.New("invalid photo receipt asset/settings identity")
		}
	case "settings":
		if v.AssetID != nil || v.SettingsKey == nil || *v.SettingsKey != "library" {
			return errors.New("invalid photo settings receipt identity")
		}
	default:
		return fmt.Errorf("invalid photo receipt operation %q", v.Operation)
	}
	if v.AfterRevision != v.BeforeRevision+1 || v.Operation == "create" && v.BeforeRevision != 0 ||
		v.Operation != "create" && v.Operation != "promote" && v.BeforeRevision < 1 {
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

func importPhotoMetadataRecord(ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value) error {
	switch kind {
	case metadataPhotoAssetType:
		var v metadataPhotoAsset
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoAssetMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_assets(asset_id,kind,revision,excluded_at,display_file_id,display_override_file_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, v.AssetID, v.Kind, v.Revision, v.ExcludedAt, v.DisplayFileID, v.DisplayOverrideFileID, v.CreatedAt, v.UpdatedAt)
		return err
	case metadataPhotoFileType:
		var v metadataPhotoFile
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoFileMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_files(file_id,asset_id,node_id,role,sidecar_of_file_id,created_at) VALUES(?,?,?,?,?,?)`, v.FileID, v.AssetID, v.NodeID, v.Role, v.SidecarOfID, v.CreatedAt)
		return err
	case metadataPhotoSettingsType:
		var v metadataPhotoSettings
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoSettingsMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_library_settings(singleton,preference,revision,updated_at) VALUES(1,?,?,?)`, v.Preference, v.Revision, v.UpdatedAt)
		return err
	case metadataPhotoReceiptType:
		var v metadataPhotoReceipt
		if err := decodeMetadataRecord(raw, &v); err != nil {
			return err
		}
		if err := validatePhotoReceiptMetadataRecord(v); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO photo_change_receipts(receipt_id,operation,asset_id,settings_key,before_revision,after_revision,before_json,after_json,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, v.ReceiptID, v.Operation, v.AssetID, v.SettingsKey, v.BeforeRevision, v.AfterRevision, v.BeforeJSON, v.AfterJSON, v.CreatedAt)
		return err
	default:
		return fmt.Errorf("unknown photo metadata record %q", kind)
	}
}

func validatePhotoMetadataState(ctx context.Context, tx metadataQuerier) error {
	if err := validatePhotoGraph(ctx, tx); err != nil {
		return err
	}
	var orphans int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM photo_change_receipts r
		LEFT JOIN photo_assets a ON a.asset_id=r.asset_id
		WHERE r.asset_id IS NOT NULL AND a.asset_id IS NULL`).Scan(&orphans); err != nil {
		return fmt.Errorf("checking photo receipt references: %w", err)
	}
	if orphans > 0 {
		return fmt.Errorf("%w: %d photo receipts reference missing assets", ErrInvalidPhotoAsset, orphans)
	}
	if err := exportPhotoMetadata(ctx, tx, func(any) error { return nil }); err != nil {
		return fmt.Errorf("validating photo metadata: %w", err)
	}
	return nil
}
