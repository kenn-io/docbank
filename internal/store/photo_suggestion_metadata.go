package store

import (
	"context"
	"errors"

	"go.kenn.io/docbank/document"
)

type metadataPhotoSuggestion struct {
	AcceptedTags        []PhotoSuggestionAcceptedTag `json:"accepted_tags" db:"accepted_tags_json,json"`
	Type                string                       `json:"type"`
	ID                  string                       `json:"suggestion_id" db:"suggestion_id"`
	ContentVersionID    string                       `json:"content_version_id" db:"content_version_id"`
	ProducerFingerprint string                       `json:"producer_fingerprint" db:"producer_fingerprint"`
	Kind                string                       `json:"kind" db:"kind"`
	Value               document.PhotoDescription    `json:"value" db:"value_json,json"`
	Response            string                       `json:"response" db:"response"`
	Revision            int64                        `json:"revision" db:"revision"`
	ReceiptID           *string                      `json:"receipt_id" db:"receipt_id"`
	CreatedAt           string                       `json:"created_at" db:"created_at"`
}

func (value metadataPhotoSuggestion) suggestion() PhotoSuggestion {
	return PhotoSuggestion{AcceptedTags: value.AcceptedTags, ID: value.ID, ContentVersionID: value.ContentVersionID, ProducerFingerprint: value.ProducerFingerprint, Kind: value.Kind, Value: value.Value, Response: value.Response, Revision: value.Revision, ReceiptID: value.ReceiptID, CreatedAt: value.CreatedAt}
}

var photoSuggestionMetadata = newMetadataTable(metadataTable[metadataPhotoSuggestion]{record: metadataPhotoSuggestion{Type: "photo_suggestion"}, table: "photo_suggestions", suffix: "WHERE response<>'pending' ORDER BY suggestion_id", validate: func(value metadataPhotoSuggestion) error {
	if value.Type != "photo_suggestion" || value.Response == photoSuggestionPending {
		return errors.New("pending suggestions are rebuildable")
	}
	return validatePhotoSuggestion(value.suggestion())
}, checkExport: true})

func validatePhotoSuggestionReferences(ctx context.Context, q metadataQuerier) error {
	var invalid int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM photo_suggestions s
 JOIN content_versions v ON v.version_id=s.content_version_id
 LEFT JOIN photo_files f ON f.node_id=v.node_id WHERE f.file_id IS NULL`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return errors.New("photo suggestion content version does not belong to a photo")
	}
	return nil
}
