package store

import (
	"context"
	"errors"
	"fmt"
)

// VectorDocument is one unique content object currently selected by at least
// one live, text-searchable file. ExtractorVersion and Text together determine
// derived embedding freshness.
type VectorDocument struct {
	BlobHash         string
	ExtractorVersion int
	Text             string
}

// VectorDocuments returns a stable node-ID page for the rebuildable vector
// mirror. It exposes no historical or trashed content and relies only on the
// current extractor's successful FTS projection.
func (s *Store) VectorDocuments(
	ctx context.Context, afterBlobHash string, limit int,
) ([]VectorDocument, error) {
	if limit < 1 || limit > 5000 {
		return nil, errors.New("invalid vector document page")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT v.blob_hash, e.extractor_version, e.text
		FROM nodes n
		JOIN content_versions v ON v.version_id=n.current_version_id
		JOIN text_searchable_versions sv ON sv.version_id=v.version_id
		JOIN extracted_text e ON e.blob_hash=v.blob_hash
		WHERE v.blob_hash > ? AND n.trashed_at IS NULL
		  AND n.kind='file' AND e.extractor='plain-text' AND e.status='ok'
		  AND e.text IS NOT NULL AND e.text<>''
		ORDER BY v.blob_hash
		LIMIT ?`, afterBlobHash, limit)
	if err != nil {
		return nil, fmt.Errorf("listing vector documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]VectorDocument, 0)
	for rows.Next() {
		var item VectorDocument
		if err := rows.Scan(&item.BlobHash, &item.ExtractorVersion, &item.Text); err != nil {
			return nil, fmt.Errorf("listing vector documents: scanning row: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing vector documents: %w", err)
	}
	return items, nil
}
