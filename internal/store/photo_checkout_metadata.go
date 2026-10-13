package store

import (
	"context"
	"errors"
)

const photoCheckoutsStorageSchemaVersion = 34

type metadataPhotoCheckout struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"checkout_id" db:"checkout_id"`
	Selection PhotoCheckoutSelection `json:"selection" db:"selection_json,json"`
	ByteLimit int64                  `json:"byte_limit" db:"byte_limit"`
	CreatedAt string                 `json:"created_at" db:"created_at"`
}

type metadataPhotoCheckoutEntry struct {
	Type       string `json:"type"`
	CheckoutID string `json:"checkout_id" db:"checkout_id"`
	FileID     string `json:"file_id" db:"file_id"`
	AssetID    string `json:"asset_id" db:"asset_id"`
	VersionID  string `json:"version_id" db:"version_id"`
	Hash       string `json:"blob_hash" db:"blob_hash"`
	Size       int64  `json:"size" db:"size"`
	Path       string `json:"relative_path" db:"relative_path"`
}

var photoCheckoutMetadataTables = []metadataRecordCodec{
	newMetadataTable(metadataTable[metadataPhotoCheckout]{record: metadataPhotoCheckout{Type: "photo_checkout"}, table: "photo_checkouts", suffix: "ORDER BY checkout_id", checkExport: true, validate: func(v metadataPhotoCheckout) error {
		if v.Type != "photo_checkout" || validateUUIDv4(v.ID) != nil || v.ByteLimit < 0 || v.Selection.All && v.ByteLimit == 0 {
			return ErrInvalidPhotoCheckout
		}
		if err := ValidatePhotoCheckoutSelection(v.Selection); err != nil {
			return err
		}
		return validateMetadataTime("photo checkout created_at", v.CreatedAt)
	}}),
	newMetadataTable(metadataTable[metadataPhotoCheckoutEntry]{record: metadataPhotoCheckoutEntry{Type: "photo_checkout_entry"}, table: "photo_checkout_entries", suffix: "ORDER BY checkout_id,file_id", checkExport: true, validate: func(v metadataPhotoCheckoutEntry) error {
		if v.Type != "photo_checkout_entry" || validateUUIDv4(v.CheckoutID) != nil || validateUUIDv4(v.FileID) != nil || validateUUIDv4(v.AssetID) != nil || validateUUIDv4(v.VersionID) != nil || v.Size < 0 || validateCheckoutPath(v.Path) != nil {
			return ErrInvalidPhotoCheckout
		}
		return validateCatalogSHA256(v.Hash, "photo checkout blob hash")
	}}),
}

func validatePhotoCheckoutReferences(ctx context.Context, q metadataQuerier) error {
	var invalid int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_checkout_entries e LEFT JOIN content_versions v ON v.version_id=e.version_id LEFT JOIN photo_files f ON f.file_id=e.file_id LEFT JOIN photo_assets a ON a.asset_id=e.asset_id LEFT JOIN photo_checkouts c ON c.checkout_id=e.checkout_id WHERE v.version_id IS NULL OR f.file_id IS NULL OR a.asset_id IS NULL OR c.checkout_id IS NULL OR v.node_id<>f.node_id OR v.blob_hash<>e.blob_hash OR v.size<>e.size`).Scan(&invalid); err != nil {
		return err
	}
	if invalid > 0 {
		return errors.New("photo checkout frozen bases do not match content versions")
	}
	return nil
}
