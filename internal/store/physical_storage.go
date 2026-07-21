package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/kit/pack"
)

const (
	maxPackEligibleBytes int64 = 64 << 20
	looseEncodingRaw           = "raw"
	looseEncodingZstd          = "zstd"
)

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
	EligibleObjects   int64
	EligibleBytes     int64
	RawObjects        int64
	CompressedObjects int64
}

// BlobPhysical is the loose representation published before a metadata
// transaction grants logical authority.
type BlobPhysical struct {
	Encoding     string
	StoredBytes  int64
	PackEligible bool
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

// PhysicalContent returns the indexed representation with current catalog
// authority for hash.
func (s *Store) PhysicalContent(ctx context.Context, hash string) (PhysicalContent, error) {
	var (
		logical      int64
		encoding     sql.NullString
		looseStored  sql.NullInt64
		packEligible bool
		packedStored sql.NullInt64
		packedFlags  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT b.size, b.loose_encoding, b.loose_stored_size, b.pack_eligible,
		       i.stored_len, i.flags
		FROM blobs b LEFT JOIN blob_pack_index i ON i.blob_hash = b.hash
		WHERE b.hash = ?`, hash,
	).Scan(&logical, &encoding, &looseStored, &packEligible, &packedStored, &packedFlags)
	if errors.Is(err, sql.ErrNoRows) {
		return PhysicalContent{}, ErrNotFound
	}
	if err != nil {
		return PhysicalContent{}, fmt.Errorf("reading physical content %s: %w", hash, err)
	}
	physical := PhysicalContent{LogicalBytes: logical, PackEligible: packEligible}
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
		return PhysicalContent{}, fmt.Errorf("blob %s has no indexed physical authority", hash)
	}
	physical.Kind = "loose"
	physical.Encoding = encoding.String
	physical.StoredBytes = looseStored.Int64
	return physical, nil
}

// LooseBacklog returns indexed packing work without walking blob directories.
func (s *Store) LooseBacklog(ctx context.Context) (LooseBacklog, error) {
	var backlog LooseBacklog
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0),
		       COALESCE(SUM(CASE WHEN loose_encoding = 'raw' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN loose_encoding = 'zstd' THEN 1 ELSE 0 END), 0)
		FROM blobs
		WHERE pack_eligible = 1 AND loose_encoding IS NOT NULL`,
	).Scan(&backlog.EligibleObjects, &backlog.EligibleBytes,
		&backlog.RawObjects, &backlog.CompressedObjects)
	if err != nil {
		return LooseBacklog{}, fmt.Errorf("reading loose backlog: %w", err)
	}
	return backlog, nil
}
