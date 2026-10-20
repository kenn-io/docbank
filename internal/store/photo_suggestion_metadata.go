package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// metadataPhotoSuggestion records what the owner decided about a description,
// never the generated text. After restore the row awaits regeneration, which
// fills only the fields this record leaves open.
type metadataPhotoSuggestion struct {
	Type                string   `json:"type"`
	ID                  string   `json:"suggestion_id" db:"suggestion_id"`
	ContentVersionID    string   `json:"content_version_id" db:"content_version_id"`
	ProducerFingerprint string   `json:"producer_fingerprint" db:"producer_fingerprint"`
	Dismissed           bool     `json:"dismissed" db:"dismissed"`
	CaptionResolved     bool     `json:"caption_resolved" db:"caption_resolved"`
	ResolvedTags        []string `json:"resolved_tags" db:"resolved_tags_json,json"`
	CreatedAt           string   `json:"created_at" db:"created_at"`
}

var photoSuggestionMetadata = newMetadataTable(metadataTable[metadataPhotoSuggestion]{
	record: metadataPhotoSuggestion{Type: "photo_suggestion"}, table: "photo_suggestions",
	suffix:   "WHERE dismissed OR caption_resolved OR resolved_tags_json<>'[]' ORDER BY suggestion_id",
	validate: validatePhotoSuggestionMetadata, checkExport: true, insert: insertPhotoSuggestionMetadata,
})

func validatePhotoSuggestionMetadata(value metadataPhotoSuggestion) error {
	if value.Type != "photo_suggestion" || validateUUIDv4(value.ID) != nil || validateUUIDv4(value.ContentVersionID) != nil ||
		!value.Dismissed && !value.CaptionResolved && len(value.ResolvedTags) == 0 {
		return errors.New("invalid photo suggestion record")
	}
	for i, name := range value.ResolvedTags {
		if normalized, err := NormalizeTagName(name); err != nil || normalized != name || slices.Contains(value.ResolvedTags[:i], name) {
			return errors.Join(ErrInvalidTag, err)
		}
	}
	if err := validateCatalogSHA256(value.ProducerFingerprint, "suggestion producer"); err != nil {
		return err
	}
	return validateMetadataTime("photo suggestion created_at", value.CreatedAt)
}

func insertPhotoSuggestionMetadata(ctx context.Context, tx *sql.Tx, value metadataPhotoSuggestion) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO photo_suggestions(suggestion_id,content_version_id,producer_fingerprint,dismissed,caption_resolved,resolved_tags_json,awaiting,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		value.ID, value.ContentVersionID, value.ProducerFingerprint, value.Dismissed, value.CaptionResolved, mustCatalogJSON(value.ResolvedTags), !value.Dismissed, value.CreatedAt)
	return err
}
