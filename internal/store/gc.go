package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/kit/packstore"
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

// RepackCandidate binds one sparse pack to the lowest canonical live blob hash
// that provides its stable maintenance key.
type RepackCandidate struct {
	Hash  string
	Usage packstore.PackUsage
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

// UnreachableBlobsPage returns one hash-keyset page of blobs referenced by no
// content version. The extra-row probe keeps memory and row scanning bounded
// while reporting whether another page currently exists.
func (s *Store) UnreachableBlobsPage(
	ctx context.Context, after string, limit int,
) ([]BlobInfo, bool, error) {
	return s.blobPage(ctx, `
		SELECT b.hash, b.size FROM blobs b
		WHERE b.hash > ?
		  AND NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = b.hash)
		ORDER BY b.hash LIMIT ?`, after, limit, "finding unreachable blobs")
}

// BlobsPage returns one bounded hash-keyset page of recorded blob identities.
func (s *Store) BlobsPage(ctx context.Context, after string, limit int) ([]BlobInfo, bool, error) {
	return s.blobPage(ctx, `
		SELECT hash, size FROM blobs WHERE hash > ? ORDER BY hash LIMIT ?`,
		after, limit, "listing blobs")
}

// BlobHashesPage is the scalar-tolerant verification inventory. It does not
// scan ancillary blob metadata, so a malformed size remains reportable by
// ValidateMetadata without suppressing content verification.
func (s *Store) BlobHashesPage(
	ctx context.Context, after string, limit int,
) ([]string, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("blob hash page limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT hash FROM blobs WHERE hash > ? ORDER BY hash LIMIT ?`, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("listing blob hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0, limit+1)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, false, fmt.Errorf("scanning blob hash: %w", err)
		}
		result = append(result, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("listing blob hashes: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}

func (s *Store) blobPage(
	ctx context.Context, query, after string, limit int, operation string,
) ([]BlobInfo, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("blob page limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, query, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", operation, err)
	}
	items, err := scanBlobInfos(rows, operation)
	if err != nil {
		return nil, false, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, nil
}

// SparseRepackPage returns eligible non-empty sparse packs ordered by their
// unique lowest live canonical blob hash.
func (s *Store) SparseRepackPage(
	ctx context.Context,
	after string,
	limit int,
	now time.Time,
	minAge time.Duration,
	minDeadBytes int64,
) ([]RepackCandidate, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("repack page limit must be positive")
	}
	cutoff := now.UTC().Add(-minAge).Format(timestampLayout)
	rows, err := s.db.QueryContext(ctx, `
		SELECT MIN(b.hash), p.pack_id, p.entry_count, p.stored_bytes, p.created_at,
		       COUNT(b.hash),
		       COALESCE(SUM(CASE WHEN b.hash IS NOT NULL THEN i.stored_len ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN b.hash IS NOT NULL THEN i.raw_len ELSE 0 END), 0),
		       COALESCE(MAX(CASE WHEN b.hash IS NOT NULL THEN i.stored_len ELSE 0 END), 0),
		       COALESCE(MAX(CASE WHEN b.hash IS NOT NULL THEN i.raw_len ELSE 0 END), 0)
		FROM blob_packs p
		LEFT JOIN blob_pack_index i ON i.pack_id = p.pack_id
		LEFT JOIN blobs b ON b.hash = i.blob_hash
		GROUP BY p.pack_id, p.entry_count, p.stored_bytes, p.created_at
		HAVING COUNT(b.hash) > 0
		   AND COUNT(b.hash) <= (p.entry_count - 1) / 2
		   AND p.created_at <= ?
		   AND p.stored_bytes - COALESCE(SUM(
		       CASE WHEN b.hash IS NOT NULL THEN i.stored_len ELSE 0 END), 0) >= ?
		   AND MIN(b.hash) > ?
		ORDER BY MIN(b.hash) LIMIT ?`, cutoff, minDeadBytes, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("listing sparse repack candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]RepackCandidate, 0, limit+1)
	for rows.Next() {
		var candidate RepackCandidate
		var created string
		if err := rows.Scan(&candidate.Hash, &candidate.Usage.PackID,
			&candidate.Usage.EntryCount, &candidate.Usage.StoredBytes, &created,
			&candidate.Usage.LiveEntries, &candidate.Usage.LiveStoredBytes,
			&candidate.Usage.LiveRawBytes, &candidate.Usage.MaxLiveStoredLen,
			&candidate.Usage.MaxLiveRawLen); err != nil {
			return nil, false, fmt.Errorf("scanning sparse repack candidate: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return nil, false, fmt.Errorf("parsing blob pack %s creation time: %w",
				candidate.Usage.PackID, err)
		}
		candidate.Usage.CreatedAt = createdAt
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("listing sparse repack candidates: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
}

// DeadPackUsagePage returns a bounded set of packs with no live mappings.
// Successful repack retirement deletes each returned candidate, so callers can
// resume this phase without an identity cursor.
func (s *Store) DeadPackUsagePage(
	ctx context.Context, limit int,
) ([]packstore.PackUsage, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("dead-pack page limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.pack_id, p.entry_count, p.stored_bytes, p.created_at,
		       COUNT(b.hash), 0, 0, 0, 0
		FROM blob_packs p
		LEFT JOIN blob_pack_index i ON i.pack_id = p.pack_id
		LEFT JOIN blobs b ON b.hash = i.blob_hash
		GROUP BY p.pack_id, p.entry_count, p.stored_bytes, p.created_at
		HAVING COUNT(b.hash) = 0
		ORDER BY p.pack_id LIMIT ?`, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("listing dead packs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]packstore.PackUsage, 0, limit+1)
	for rows.Next() {
		var usage packstore.PackUsage
		var created string
		if err := rows.Scan(&usage.PackID, &usage.EntryCount, &usage.StoredBytes, &created,
			&usage.LiveEntries, &usage.LiveStoredBytes, &usage.LiveRawBytes,
			&usage.MaxLiveStoredLen, &usage.MaxLiveRawLen); err != nil {
			return nil, false, fmt.Errorf("scanning dead pack: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return nil, false, fmt.Errorf("parsing blob pack %s creation time: %w", usage.PackID, err)
		}
		usage.CreatedAt = createdAt
		result = append(result, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("listing dead packs: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return result, more, nil
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

// PackedBlobStoredByte reports one blob's immutable-pack payload length.
func (s *Store) PackedBlobStoredByte(ctx context.Context, hash string) (int64, bool, error) {
	var size int64
	err := s.db.QueryRowContext(ctx,
		`SELECT stored_len FROM blob_pack_index WHERE blob_hash = ?`, hash).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading packed blob size for %s: %w", hash, err)
	}
	return size, true, nil
}
