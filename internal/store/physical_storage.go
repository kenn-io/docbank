package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

const (
	maxPackEligibleBytes int64 = 64 << 20
	looseEncodingRaw           = "raw"
	looseEncodingZstd          = "zstd"
)

// ErrPhysicalAuthorityMissing means logical blob membership exists but no
// indexed loose representation or pack mapping currently authorizes reads.
var ErrPhysicalAuthorityMissing = packstore.ErrPhysicalAuthorityMissing

// PhysicalContent describes the catalog-authorized representation of one
// logical blob without requiring a filesystem scan.
type PhysicalContent struct {
	Kind         string
	Encoding     string
	LogicalBytes int64
	StoredBytes  int64
	PackEligible bool
}

// LooseBacklog summarizes loose content eligible for an explicit pack pass.
type LooseBacklog struct {
	EligibleObjects     int64
	EligibleBytes       int64
	EligibleStoredBytes int64
	RawObjects          int64
	CompressedObjects   int64
}

// BlobPhysical is the loose representation published before a metadata
// transaction grants logical authority.
type BlobPhysical struct {
	Encoding     string
	StoredBytes  int64
	PackEligible bool
	// Created proves this write published a new, fully hashed canonical loose
	// representation rather than deduplicating an existing file by type and size.
	Created bool
}

func normalizeBlobPhysical(size int64, physical []BlobPhysical) (BlobPhysical, error) {
	if len(physical) > 1 {
		return BlobPhysical{}, errors.New("at most one physical blob receipt may be supplied")
	}
	if len(physical) == 0 {
		return BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: size, PackEligible: size <= maxPackEligibleBytes}, nil
	}
	result := physical[0]
	if result.Encoding != looseEncodingRaw && result.Encoding != looseEncodingZstd {
		return BlobPhysical{}, fmt.Errorf("invalid loose encoding %q", result.Encoding)
	}
	if result.StoredBytes < 0 {
		return BlobPhysical{}, errors.New("loose stored bytes must not be negative")
	}
	if result.Encoding == looseEncodingRaw && result.StoredBytes != size {
		return BlobPhysical{}, fmt.Errorf("raw loose content stores %d bytes, want logical size %d", result.StoredBytes, size)
	}
	return result, nil
}

const physicalContentSQL = `
	SELECT b.size, l.encoding, l.stored_size, l.pack_eligible,
	       i.stored_len, i.flags
	FROM blobs b
	LEFT JOIN blob_locations l
	  ON l.blob_hash = b.hash
	 AND l.store_id = (SELECT store_id FROM blob_stores WHERE role = 'primary')
	LEFT JOIN blob_pack_entries i
	  ON i.blob_hash = l.blob_hash AND i.store_id = l.store_id
	WHERE b.hash = ?`

const authorizedPhysicalContentSQL = `
	SELECT b.size, l.encoding, l.stored_size, l.pack_eligible,
	       i.stored_len, i.flags
	FROM blobs b
	JOIN blob_locations l ON l.blob_hash = b.hash
	LEFT JOIN blob_pack_entries i
	  ON i.blob_hash = l.blob_hash AND i.store_id = l.store_id
	WHERE b.hash = ?
	  AND (
	    (l.kind = 'loose' AND l.encoding IN ('raw', 'zstd'))
	    OR (l.kind = 'packed' AND i.blob_hash IS NOT NULL)
	  )
	ORDER BY CASE WHEN l.store_id = (
	  SELECT store_id FROM blob_stores WHERE role = 'primary'
	) THEN 0 ELSE 1 END, l.store_id
	LIMIT 1`

func scanPhysicalContent(row scanner, hash string) (PhysicalContent, error) {
	var (
		logical      int64
		encoding     sql.NullString
		looseStored  sql.NullInt64
		packEligible sql.NullBool
		packedStored sql.NullInt64
		packedFlags  sql.NullInt64
	)
	err := row.Scan(&logical, &encoding, &looseStored, &packEligible, &packedStored, &packedFlags)
	if errors.Is(err, sql.ErrNoRows) {
		return PhysicalContent{}, ErrNotFound
	}
	if err != nil {
		return PhysicalContent{}, fmt.Errorf("reading physical content %s: %w", hash, err)
	}
	physical := PhysicalContent{
		LogicalBytes: logical,
		PackEligible: packEligible.Valid && packEligible.Bool,
	}
	if packedStored.Valid {
		if !packedFlags.Valid || packedFlags.Int64 < 0 || packedFlags.Int64 > math.MaxUint8 {
			return PhysicalContent{}, fmt.Errorf("blob %s has invalid packed encoding flags", hash)
		}
		physical.Kind = "packed"
		physical.Encoding = looseEncodingRaw
		if pack.BlobFlags(packedFlags.Int64)&pack.BlobCompressed != 0 {
			physical.Encoding = looseEncodingZstd
		}
		physical.StoredBytes = packedStored.Int64
		return physical, nil
	}
	if !encoding.Valid || !looseStored.Valid {
		return PhysicalContent{}, fmt.Errorf("blob %s: %w", hash, ErrPhysicalAuthorityMissing)
	}
	physical.Kind = "loose"
	physical.Encoding = encoding.String
	physical.StoredBytes = looseStored.Int64
	return physical, nil
}

func physicalContentTx(tx *sql.Tx, hash string) (PhysicalContent, error) {
	return scanPhysicalContent(tx.QueryRow(physicalContentSQL, hash), hash)
}

// authorizedPhysicalContentTx returns the deterministic preferred catalog
// representation for a mutation receipt. Unlike physicalContentTx, which is
// intentionally primary-specific for local packing and repair, this accepts a
// secondary-only blob after EnsureBlobTx has validated its authority.
func authorizedPhysicalContentTx(tx *sql.Tx, hash string) (PhysicalContent, error) {
	if _, err := requirePhysicalAuthorityTx(tx, hash); err != nil {
		return PhysicalContent{}, err
	}
	physical, err := scanPhysicalContent(tx.QueryRow(authorizedPhysicalContentSQL, hash), hash)
	if errors.Is(err, ErrNotFound) {
		return PhysicalContent{}, fmt.Errorf("blob %s: %w", hash, ErrPhysicalAuthorityMissing)
	}
	return physical, err
}

// requirePhysicalAuthorityTx returns the logical size only when the catalog
// authorizes either loose or packed bytes for hash. Logical membership alone
// is insufficient for reads or for creating another current reference.
func requirePhysicalAuthorityTx(tx *sql.Tx, hash string) (int64, error) {
	var (
		size     int64
		location bool
	)
	err := tx.QueryRow(`
		SELECT b.size, EXISTS(
			SELECT 1
			FROM blob_locations l
			LEFT JOIN blob_pack_entries e
			  ON e.blob_hash=l.blob_hash AND e.store_id=l.store_id
			WHERE l.blob_hash=b.hash
			  AND (
			    (l.kind='loose' AND l.encoding IN ('raw','zstd'))
			    OR (l.kind='packed' AND e.blob_hash IS NOT NULL)
			  )
		)
		FROM blobs b WHERE b.hash=?`,
		hash,
	).Scan(&size, &location)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("checking physical authority for %s: %w", hash, err)
	}
	if !location {
		return 0, fmt.Errorf("blob %s: %w", hash, ErrPhysicalAuthorityMissing)
	}
	return size, nil
}

// PhysicalContent returns the indexed representation with current catalog
// authority for hash.
func (s *Store) PhysicalContent(ctx context.Context, hash string) (PhysicalContent, error) {
	return scanPhysicalContent(s.db.QueryRowContext(ctx, physicalContentSQL, hash), hash)
}

// LooseBacklog returns indexed packing work without walking blob directories.
func (s *Store) LooseBacklog(ctx context.Context) (LooseBacklog, error) {
	var backlog LooseBacklog
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(b.size), 0), COALESCE(SUM(l.stored_size), 0),
		       COALESCE(SUM(CASE WHEN l.encoding = 'raw' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN l.encoding = 'zstd' THEN 1 ELSE 0 END), 0)
		FROM blob_locations l JOIN blobs b ON b.hash = l.blob_hash
		WHERE l.store_id = ? AND l.kind = ? AND l.pack_eligible = 1`,
		s.primaryStoreID, blobLocationKindLoose,
	).Scan(&backlog.EligibleObjects, &backlog.EligibleBytes, &backlog.EligibleStoredBytes,
		&backlog.RawObjects, &backlog.CompressedObjects)
	if err != nil {
		return LooseBacklog{}, fmt.Errorf("reading loose backlog: %w", err)
	}
	return backlog, nil
}
