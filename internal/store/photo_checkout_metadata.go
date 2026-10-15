package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
)

const photoCheckoutsStorageSchemaVersion = 36

type metadataPhotoCheckout struct {
	RetiredAt         *string                `json:"retired_at,omitempty" db:"retired_at"`
	RetentionReleased bool                   `json:"retention_released" db:"retention_released"`
	Type              string                 `json:"type"`
	ID                string                 `json:"checkout_id" db:"checkout_id"`
	Selection         PhotoCheckoutSelection `json:"selection" db:"selection_json,json"`
	ByteLimit         int64                  `json:"byte_limit" db:"byte_limit"`
	CreatedAt         string                 `json:"created_at" db:"created_at"`
}

type metadataPhotoCheckoutEntry struct {
	BaseHeld    *bool   `json:"base_held,omitempty"`
	SidecarOfID *string `json:"sidecar_of_file_id" db:"sidecar_of_file_id"`
	Type        string  `json:"type"`
	CheckoutID  string  `json:"checkout_id" db:"checkout_id"`
	FileID      string  `json:"file_id" db:"file_id"`
	AssetID     string  `json:"asset_id" db:"asset_id"`
	VersionID   string  `json:"version_id" db:"version_id"`
	Hash        string  `json:"blob_hash" db:"blob_hash"`
	Size        int64   `json:"size" db:"size"`
	Path        string  `json:"relative_path" db:"relative_path"`
}

var photoCheckoutMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataPhotoCheckout]{record: metadataPhotoCheckout{Type: "photo_checkout"}, table: "photo_checkouts", suffix: "ORDER BY checkout_id", checkExport: true, exportAll: exportPhotoCheckoutMetadata, validate: validatePhotoCheckoutMetadata}),
	newMetadataTable(metadataTable[metadataPhotoCheckoutEntry]{record: metadataPhotoCheckoutEntry{Type: "photo_checkout_entry"}, table: "photo_checkout_entries", suffix: "ORDER BY checkout_id,file_id", checkExport: true, exportAll: exportPhotoCheckoutEntryMetadata, insert: insertPhotoCheckoutEntryMetadata, validate: validatePhotoCheckoutEntryMetadata}),
}

func validatePhotoCheckoutEntryMetadata(v metadataPhotoCheckoutEntry) error {
	if v.Type != "photo_checkout_entry" || validateUUIDv4(v.CheckoutID) != nil || validateUUIDv4(v.FileID) != nil || validateUUIDv4(v.AssetID) != nil || validateUUIDv4(v.VersionID) != nil || v.SidecarOfID != nil && validateUUIDv4(*v.SidecarOfID) != nil || v.Size < 0 || validateCheckoutPath(v.Path) != nil {
		return ErrInvalidPhotoCheckout
	}
	return validateCatalogSHA256(v.Hash, "photo checkout blob hash")
}

func validatePhotoCheckoutMetadata(v metadataPhotoCheckout) error {
	if v.Type != "photo_checkout" || validateUUIDv4(v.ID) != nil || v.ByteLimit < 0 || v.Selection.All && v.ByteLimit == 0 {
		return ErrInvalidPhotoCheckout
	}
	if err := ValidatePhotoCheckoutSelection(v.Selection); err != nil {
		return err
	}
	if v.RetiredAt != nil {
		if !v.RetentionReleased {
			return ErrInvalidPhotoCheckout
		}
		if err := validateMetadataTime("photo checkout retired_at", *v.RetiredAt); err != nil {
			return err
		}
	}
	return validateMetadataTime("photo checkout created_at", v.CreatedAt)
}

func validatePhotoCheckoutReferences(ctx context.Context, q metadataQuerier) error {
	var invalid int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_checkout_entries e LEFT JOIN content_versions v ON v.version_id=e.version_id LEFT JOIN photo_files f ON f.file_id=e.file_id LEFT JOIN photo_assets a ON a.asset_id=e.asset_id LEFT JOIN photo_checkouts c ON c.checkout_id=e.checkout_id WHERE c.checkout_id IS NULL OR (e.sidecar_of_file_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM photo_checkout_entries target WHERE target.checkout_id=e.checkout_id AND target.file_id=e.sidecar_of_file_id AND target.asset_id=e.asset_id AND target.sidecar_of_file_id IS NULL AND target.file_id<>e.file_id)) OR ((`+photoCheckoutHoldPredicate+`) AND (v.version_id IS NULL OR f.file_id IS NULL OR a.asset_id IS NULL OR v.node_id<>f.node_id OR v.blob_hash<>e.blob_hash OR v.size<>e.size))`).Scan(&invalid); err != nil {
		return err
	}
	if invalid > 0 {
		return errors.New("photo checkout frozen bases do not match content versions")
	}
	return nil
}

func exportPhotoCheckoutMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT c.checkout_id,c.selection_json,c.byte_limit,c.created_at,c.retention_released OR COALESCE(o.state,c.state) IN ('queued','running','paused'),c.retired_at FROM photo_checkouts c LEFT JOIN storage_operations o ON o.operation_id=c.operation_id ORDER BY c.checkout_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataPhotoCheckout{Type: "photo_checkout"}
		var selection string
		if err := rows.Scan(&record.ID, &selection, &record.ByteLimit, &record.CreatedAt, &record.RetentionReleased, &record.RetiredAt); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(selection), &record.Selection); err != nil {
			return err
		}
		if err := validatePhotoCheckoutMetadata(record); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func insertPhotoCheckoutEntryMetadata(ctx context.Context, tx *sql.Tx, v metadataPhotoCheckoutEntry) error {
	held := v.BaseHeld != nil && *v.BaseHeld
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_checkout_entries(checkout_id,file_id,asset_id,version_id,blob_hash,size,relative_path,sidecar_of_file_id,base_held) VALUES(?,?,?,?,?,?,?,?,?)`, v.CheckoutID, v.FileID, v.AssetID, v.VersionID, v.Hash, v.Size, v.Path, v.SidecarOfID, held)
	return err
}

func exportPhotoCheckoutEntryMetadata(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT e.checkout_id,e.file_id,e.asset_id,e.version_id,e.blob_hash,e.size,e.relative_path,e.sidecar_of_file_id,c.retired_at IS NULL AND (e.base_held=1 OR e.published=1 OR e.file_id=COALESCE(o.cursor,'')) FROM photo_checkout_entries e JOIN photo_checkouts c ON c.checkout_id=e.checkout_id LEFT JOIN storage_operations o ON o.operation_id=c.operation_id ORDER BY e.checkout_id,e.file_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataPhotoCheckoutEntry{Type: "photo_checkout_entry"}
		var held bool
		if err := rows.Scan(&record.CheckoutID, &record.FileID, &record.AssetID, &record.VersionID, &record.Hash, &record.Size, &record.Path, &record.SidecarOfID, &held); err != nil {
			return err
		}
		if held {
			record.BaseHeld = new(true)
		}
		if err := validatePhotoCheckoutEntryMetadata(record); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rows.Err()
}
