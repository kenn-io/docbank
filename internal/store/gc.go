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

// GCCandidate is one unreachable catalog row and its indexed loose authority.
type GCCandidate struct {
	Hash            string
	Loose           bool
	LooseStoredSize int64
}

// GCCandidateScanPage reports bounded raw catalog progress independently of
// how many rows qualify as unreachable work.
type GCCandidateScanPage struct {
	Items     []GCCandidate
	Examined  int
	HighWater string
	More      bool
}

// StringScanPage reports bounded raw key progress for a filtered string
// inventory such as unreferenced packed mappings.
type StringScanPage struct {
	Items     []string
	Examined  int
	HighWater string
	More      bool
}

// RepackCandidate binds one sparse pack to the lowest canonical live blob hash
// that provides its stable maintenance key.
type RepackCandidate struct {
	Hash     string
	Usage    packstore.PackUsage
	Eligible bool
}

type RepackScanPage struct {
	Items []RepackCandidate
	More  bool
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

// UnreachableBlobsPageFrom distinguishes the beginning of an ordering from an
// arbitrary stored key, including the empty string. It examines a bounded raw
// key window before filtering for unreachable work.
func (s *Store) UnreachableBlobsPageFrom(
	ctx context.Context, after *string, limit int,
) (GCCandidateScanPage, error) {
	if limit <= 0 {
		return GCCandidateScanPage{}, errors.New("blob page limit must be positive")
	}
	query, args := unreachableBlobScanQuery(after, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return GCCandidateScanPage{}, fmt.Errorf("finding unreachable blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type scanRow struct {
		candidate GCCandidate
		eligible  bool
	}
	raw := make([]scanRow, 0, limit+1)
	for rows.Next() {
		var item scanRow
		var looseSize sql.NullInt64
		if err := rows.Scan(&item.candidate.Hash, &looseSize, &item.eligible); err != nil {
			return GCCandidateScanPage{},
				fmt.Errorf("finding unreachable blobs: scanning blob row: %w", err)
		}
		item.candidate.Loose = looseSize.Valid
		item.candidate.LooseStoredSize = looseSize.Int64
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return GCCandidateScanPage{}, fmt.Errorf("finding unreachable blobs: %w", err)
	}
	more := len(raw) > limit
	if more {
		raw = raw[:limit]
	}
	page := GCCandidateScanPage{Examined: len(raw), More: more}
	if len(raw) > 0 {
		page.HighWater = raw[len(raw)-1].candidate.Hash
	}
	for _, item := range raw {
		if item.eligible {
			page.Items = append(page.Items, item.candidate)
		}
	}
	return page, nil
}

const unreachableBlobsStartPageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT hash, loose_stored_size FROM blobs
		ORDER BY hash LIMIT ?
	)
	SELECT p.hash, p.loose_stored_size,
	       NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = p.hash)
	FROM raw_page p ORDER BY p.hash`

const unreachableBlobsResumePageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT hash, loose_stored_size FROM blobs
		WHERE hash > ? ORDER BY hash LIMIT ?
	)
	SELECT p.hash, p.loose_stored_size,
	       NOT EXISTS (SELECT 1 FROM content_versions v WHERE v.blob_hash = p.hash)
	FROM raw_page p ORDER BY p.hash`

func unreachableBlobScanQuery(after *string, limit int) (string, []any) {
	if after == nil {
		return unreachableBlobsStartPageSQL, []any{limit + 1}
	}
	return unreachableBlobsResumePageSQL, []any{*after, limit + 1}
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
	return s.BlobHashesPageFrom(ctx, &after, limit)
}

// BlobHashesPageFrom distinguishes the beginning of an ordering from an
// arbitrary stored key, including the empty string.
func (s *Store) BlobHashesPageFrom(
	ctx context.Context, after *string, limit int,
) ([]string, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("blob hash page limit must be positive")
	}
	query, args := blobHashesPageQuery(after, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
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

const blobHashesStartPageSQL = `SELECT hash FROM blobs ORDER BY hash LIMIT ?`
const blobHashesResumePageSQL = `SELECT hash FROM blobs WHERE hash > ? ORDER BY hash LIMIT ?`

func blobHashesPageQuery(after *string, limit int) (string, []any) {
	if after == nil {
		return blobHashesStartPageSQL, []any{limit + 1}
	}
	return blobHashesResumePageSQL, []any{*after, limit + 1}
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
	page, err := s.SparseRepackScanPage(ctx, after, "\xff", limit, now, minAge, minDeadBytes)
	if err != nil {
		return nil, false, err
	}
	result := make([]RepackCandidate, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Eligible {
			result = append(result, item)
		}
	}
	return result, page.More, nil
}

const sparseRepackScanPageSQL = `
	SELECT scan_hash, pack_id, entry_count, stored_bytes, created_at,
	       live_entries, live_stored_bytes, live_raw_bytes,
	       max_live_stored_len, max_live_raw_len
	FROM blob_packs INDEXED BY blob_packs_live_scan
	WHERE live_entries > 0
	  AND (scan_hash > ? OR (scan_hash = ? AND pack_id > ?))
	ORDER BY scan_hash, pack_id LIMIT ?`

// SparseRepackScanPage examines at most limit persisted pack summaries. Packs
// that do not satisfy the caller's thresholds still consume the finite scan
// budget, so selection work is independent of total catalog cardinality.
func (s *Store) SparseRepackScanPage(
	ctx context.Context,
	afterHash string,
	afterPackID string,
	limit int,
	now time.Time,
	minAge time.Duration,
	minDeadBytes int64,
) (RepackScanPage, error) {
	if limit <= 0 {
		return RepackScanPage{}, errors.New("repack page limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, sparseRepackScanPageSQL,
		afterHash, afterHash, afterPackID, limit+1)
	if err != nil {
		return RepackScanPage{}, fmt.Errorf("scanning sparse repack candidates: %w", err)
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
			return RepackScanPage{}, fmt.Errorf("scanning sparse repack candidate: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return RepackScanPage{}, fmt.Errorf("parsing blob pack %s creation time: %w",
				candidate.Usage.PackID, err)
		}
		candidate.Usage.CreatedAt = createdAt
		candidate.Eligible = candidate.Usage.LiveEntries <= (candidate.Usage.EntryCount-1)/2 &&
			!candidate.Usage.CreatedAt.After(now.UTC().Add(-minAge)) &&
			candidate.Usage.StoredBytes-candidate.Usage.LiveStoredBytes >= minDeadBytes
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return RepackScanPage{}, fmt.Errorf("scanning sparse repack candidates: %w", err)
	}
	more := len(result) > limit
	if more {
		result = result[:limit]
	}
	return RepackScanPage{Items: result, More: more}, nil
}

// UnreferencedPackMappingsPage returns one canonical-hash keyset page of pack
// mappings whose blob authority has been revoked.
func (s *Store) UnreferencedPackMappingsPage(
	ctx context.Context, after *string, limit int,
) (StringScanPage, error) {
	if limit <= 0 {
		return StringScanPage{}, errors.New("pack mapping page limit must be positive")
	}
	query, args := unreferencedMappingScanQuery(after, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return StringScanPage{}, fmt.Errorf("listing unreferenced pack mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type scanRow struct {
		hash     string
		eligible bool
	}
	raw := make([]scanRow, 0, limit+1)
	for rows.Next() {
		var item scanRow
		if err := rows.Scan(&item.hash, &item.eligible); err != nil {
			return StringScanPage{}, fmt.Errorf("scanning unreferenced pack mapping: %w", err)
		}
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return StringScanPage{}, fmt.Errorf("listing unreferenced pack mappings: %w", err)
	}
	more := len(raw) > limit
	if more {
		raw = raw[:limit]
	}
	page := StringScanPage{Examined: len(raw), More: more}
	if len(raw) > 0 {
		page.HighWater = raw[len(raw)-1].hash
	}
	for _, item := range raw {
		if item.eligible {
			page.Items = append(page.Items, item.hash)
		}
	}
	return page, nil
}

const unreferencedMappingsStartPageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT blob_hash FROM blob_pack_index
		ORDER BY blob_hash LIMIT ?
	)
	SELECT p.blob_hash,
	       NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = p.blob_hash)
	FROM raw_page p ORDER BY p.blob_hash`

const unreferencedMappingsResumePageSQL = `
	WITH raw_page AS MATERIALIZED (
		SELECT blob_hash FROM blob_pack_index
		WHERE blob_hash > ? ORDER BY blob_hash LIMIT ?
	)
	SELECT p.blob_hash,
	       NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = p.blob_hash)
	FROM raw_page p ORDER BY p.blob_hash`

func unreferencedMappingScanQuery(after *string, limit int) (string, []any) {
	if after == nil {
		return unreferencedMappingsStartPageSQL, []any{limit + 1}
	}
	return unreferencedMappingsResumePageSQL, []any{*after, limit + 1}
}

// DeleteUnreferencedPackMappings conditionally removes the named stale
// mappings. A blob authority restored after selection protects its mapping.
func (s *Store) DeleteUnreferencedPackMappings(ctx context.Context, hashes []string) (int64, error) {
	var removed int64
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		for _, hash := range hashes {
			result, err := tx.ExecContext(ctx, `
				DELETE FROM blob_pack_index
				WHERE blob_hash = ?
				  AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = ?)`, hash, hash)
			if err != nil {
				return fmt.Errorf("deleting unreferenced pack mapping %s: %w", hash, err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("counting deleted pack mapping %s: %w", hash, err)
			}
			removed += count
		}
		return nil
	})
	return removed, err
}

// DeadPackUsagePage returns a bounded set of packs with no live mappings.
// Successful repack retirement deletes each returned candidate, so callers can
// resume this phase without an identity cursor.
const deadPackUsagePageSQL = `
	SELECT pack_id, entry_count, stored_bytes, created_at,
	       live_entries, live_stored_bytes, live_raw_bytes,
	       max_live_stored_len, max_live_raw_len
	FROM blob_packs INDEXED BY blob_packs_dead_scan
	WHERE live_entries = 0
	ORDER BY scan_hash, pack_id LIMIT ?`

func (s *Store) DeadPackUsagePage(
	ctx context.Context, limit int,
) ([]packstore.PackUsage, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("dead-pack page limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, deadPackUsagePageSQL, limit+1)
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
