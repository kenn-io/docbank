package store

import (
	"context"
	"database/sql"
	"fmt"
)

// BlobInfo identifies a recorded blob.
type BlobInfo struct {
	Hash string
	Size int64
}

func scanBlobInfos(rows *sql.Rows, op string) ([]BlobInfo, error) {
	defer func() { _ = rows.Close() }()
	var out []BlobInfo
	for rows.Next() {
		var b BlobInfo
		if err := rows.Scan(&b.Hash, &b.Size); err != nil {
			return nil, fmt.Errorf("%s: scanning blob row: %w", op, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// UnreachableBlobs lists blobs referenced by no node (live or trashed) and
// no recorded prior version. These are the gc candidates. Callers that go
// on to delete blob files must hold the exclusive vault lock
// (home.Layout.AcquireLock): with writers running, a concurrent ingest can
// dedup against a candidate's file between this query and the deletion,
// leaving a live node pointing at a removed blob.
func (s *Store) UnreachableBlobs(ctx context.Context) ([]BlobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.hash, b.size FROM blobs b
		WHERE NOT EXISTS (SELECT 1 FROM nodes n WHERE n.blob_hash = b.hash)
		  AND NOT EXISTS (SELECT 1 FROM node_versions v WHERE v.blob_hash = b.hash)
		ORDER BY b.hash`)
	if err != nil {
		return nil, fmt.Errorf("finding unreachable blobs: %w", err)
	}
	return scanBlobInfos(rows, "finding unreachable blobs")
}

// DeleteBlobRows removes the metadata rows for reclaimed blobs. Callers
// must hold the exclusive vault lock (see UnreachableBlobs) and delete the
// blob files first; a crash in between leaves rows without files, which a
// gc re-run reconciles.
func (s *Store) DeleteBlobRows(ctx context.Context, hashes []string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, h := range hashes {
			if _, err := tx.Exec(`DELETE FROM extracted_text WHERE blob_hash = ?`, h); err != nil {
				return fmt.Errorf("deleting extracted text of %s: %w", h, err)
			}
			if _, err := tx.Exec(`DELETE FROM blobs WHERE hash = ?`, h); err != nil {
				return fmt.Errorf("deleting blob row %s: %w", h, err)
			}
		}
		return nil
	})
}

// AllBlobs lists every recorded blob, hash-ordered.
func (s *Store) AllBlobs(ctx context.Context) ([]BlobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash, size FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("listing blobs: %w", err)
	}
	return scanBlobInfos(rows, "listing blobs")
}
