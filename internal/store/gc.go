package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BlobInfo identifies a recorded blob.
type BlobInfo struct {
	Hash string
	Size int64
}

// BlobInfo returns logical catalog membership independently of whether a
// loose or packed representation currently has physical read authority.
func (s *Store) BlobInfo(ctx context.Context, hash string) (BlobInfo, error) {
	var info BlobInfo
	err := s.db.QueryRowContext(ctx,
		`SELECT hash, size FROM blobs WHERE hash = ?`, hash,
	).Scan(&info.Hash, &info.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return BlobInfo{}, ErrNotFound
	}
	if err != nil {
		return BlobInfo{}, fmt.Errorf("reading blob membership %s: %w", hash, err)
	}
	return info, nil
}

// HasBlob reports whether the metadata catalog grants authority to hash.
func (s *Store) HasBlob(ctx context.Context, hash string) (bool, error) {
	var recorded bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)`, hash,
	).Scan(&recorded); err != nil {
		return false, fmt.Errorf("checking blob authority for %s: %w", hash, err)
	}
	return recorded, nil
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

// UnreachableBlobs lists blobs referenced by no content version. Every current
// file head is itself a content version, as are retained prior versions. These
// are the gc candidates. Callers that go
// on to delete blob files must serialize against concurrent writers (the
// daemon's maintenance gate does this): with writers running, a concurrent
// ingest can dedup against a candidate's file between this query and the
// deletion, leaving a live node pointing at a removed blob.
func (s *Store) UnreachableBlobs(ctx context.Context) ([]BlobInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.hash, b.size FROM blobs b
		WHERE NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = b.hash)
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
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, h := range hashes {
			if _, err := tx.Exec(`DELETE FROM blob_pack_index WHERE blob_hash = ?`, h); err != nil {
				return fmt.Errorf("deleting packed mapping of %s: %w", h, err)
			}
			if _, err := tx.Exec(`DELETE FROM text_extraction_queue WHERE blob_hash = ?`, h); err != nil {
				return fmt.Errorf("deleting extraction queue row of %s: %w", h, err)
			}
			if _, err := tx.Exec(`DELETE FROM content_fts WHERE rowid IN (
				SELECT rowid FROM extracted_text WHERE blob_hash = ?
			)`, h); err != nil {
				return fmt.Errorf("deleting content search rows of %s: %w", h, err)
			}
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

// AllBlobHashes lists every recorded blob identity without reading ancillary
// metadata. Integrity verification uses this after separately validating the
// metadata stream, so one malformed scalar does not suppress the useful report.
func (s *Store) AllBlobHashes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash FROM blobs ORDER BY hash`)
	if err != nil {
		return nil, fmt.Errorf("listing blob hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("scanning blob hash: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing blob hashes: %w", err)
	}
	return hashes, nil
}

// PackedBlobStoredBytes returns the physical stored length of every cataloged
// packed blob. GC uses it to distinguish bytes unlinked immediately from dead
// immutable-pack space that requires a later repack.
func (s *Store) PackedBlobStoredBytes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blob_hash, stored_len FROM blob_pack_index`)
	if err != nil {
		return nil, fmt.Errorf("listing packed blob sizes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]int64)
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, fmt.Errorf("scanning packed blob size: %w", err)
		}
		result[hash] = size
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing packed blob sizes: %w", err)
	}
	return result, nil
}
