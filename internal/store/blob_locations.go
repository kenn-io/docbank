package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/kit/packstore"
)

const (
	blobLocationKindLoose  = "loose"
	blobLocationKindPacked = "packed"
)

// PackedLocation is the store-scoped immutable pack entry for one blob.
type PackedLocation struct {
	PackID string
	Offset int64
	Stored int64
	Raw    int64
	Flags  uint8
	CRC32C uint32
}

// BlobLocation is one catalog-authorized physical representation.
type BlobLocation struct {
	Hash         string
	StoreID      string
	Generation   string
	Kind         string
	Encoding     string
	LogicalSize  int64
	StoredSize   int64
	PackEligible bool
	Pack         *PackedLocation
}

// ResolveBlobLocations returns the application-authorized read candidates for
// hash. Task 5 exposes only the built-in primary; later placement work adds
// secondaries and policy ordering without changing Kit's read path.
func (s *Store) ResolveBlobLocations(
	ctx context.Context,
	hash packstore.Hash,
) (packstore.Resolution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.size, l.store_id, l.generation, l.kind, l.encoding,
		       l.stored_size, l.pack_eligible,
		       e.pack_id, e.pack_offset, e.stored_len, e.raw_len, e.flags, e.crc32c
		FROM blobs b
		LEFT JOIN blob_locations l ON l.blob_hash = b.hash
		LEFT JOIN blob_pack_entries e
		  ON e.blob_hash = l.blob_hash AND e.store_id = l.store_id
		WHERE b.hash = ?
		ORDER BY CASE WHEN l.store_id = ? THEN 0 ELSE 1 END, l.store_id`,
		hash.String(), s.primaryStoreID,
	)
	if err != nil {
		return packstore.Resolution{}, fmt.Errorf("resolving blob locations %s: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	resolution := packstore.Resolution{}
	for rows.Next() {
		resolution.Member = true
		location, present, err := scanBlobReadLocation(rows, hash)
		if err != nil {
			return packstore.Resolution{}, err
		}
		if present {
			resolution.Candidates = append(resolution.Candidates, location)
		}
	}
	if err := rows.Err(); err != nil {
		return packstore.Resolution{}, fmt.Errorf("resolving blob locations %s: %w", hash, err)
	}
	return resolution, nil
}

func scanBlobReadLocation(
	row scanner,
	hash packstore.Hash,
) (packstore.ReadLocation, bool, error) {
	var (
		logical      int64
		storeID      sql.NullString
		generation   sql.NullString
		kind         sql.NullString
		encoding     sql.NullString
		storedSize   sql.NullInt64
		packEligible sql.NullBool
		packID       sql.NullString
		offset       sql.NullInt64
		packedStored sql.NullInt64
		raw          sql.NullInt64
		flags        sql.NullInt64
		crc32c       sql.NullInt64
	)
	if err := row.Scan(
		&logical, &storeID, &generation, &kind, &encoding, &storedSize, &packEligible,
		&packID, &offset, &packedStored, &raw, &flags, &crc32c,
	); err != nil {
		return packstore.ReadLocation{}, false, fmt.Errorf(
			"scanning blob location %s: %w", hash, err,
		)
	}
	if !storeID.Valid {
		return packstore.ReadLocation{}, false, nil
	}
	location := packstore.ReadLocation{
		StoreID:    packstore.StoreID(storeID.String),
		Generation: packstore.LocationGeneration(generation.String),
	}
	switch kind.String {
	case blobLocationKindLoose:
		if !encoding.Valid || !storedSize.Valid {
			return packstore.ReadLocation{}, false, fmt.Errorf(
				"blob %s has incomplete loose authority", hash,
			)
		}
		var looseEncoding packstore.LooseEncoding
		switch encoding.String {
		case looseEncodingRaw:
			looseEncoding = packstore.LooseEncodingRaw
		case looseEncodingZstd:
			looseEncoding = packstore.LooseEncodingZstd
		default:
			return packstore.ReadLocation{}, false, fmt.Errorf(
				"blob %s has unknown loose encoding %q", hash, encoding.String,
			)
		}
		location.Loose = &packstore.LooseLocation{
			Encoding:    looseEncoding,
			LogicalSize: logical,
			StoredSize:  storedSize.Int64,
		}
	case blobLocationKindPacked:
		if !packID.Valid || !offset.Valid || !packedStored.Valid || !raw.Valid ||
			!flags.Valid || flags.Int64 < 0 || flags.Int64 > math.MaxUint8 ||
			!crc32c.Valid || crc32c.Int64 < 0 || crc32c.Int64 > math.MaxUint32 {
			return packstore.ReadLocation{}, false, fmt.Errorf(
				"blob %s has incomplete packed authority", hash,
			)
		}
		entry := packstore.IndexEntry{
			Hash: hash, PackID: packID.String, Offset: offset.Int64,
			StoredLen: packedStored.Int64, RawLen: raw.Int64,
			Flags: uint8(flags.Int64), CRC32C: uint32(crc32c.Int64),
		}
		location.Pack = &entry
	default:
		return packstore.ReadLocation{}, false, fmt.Errorf(
			"blob %s has unknown location kind %q", hash, kind.String,
		)
	}
	if err := location.Validate(); err != nil {
		return packstore.ReadLocation{}, false, fmt.Errorf(
			"validating blob location %s: %w", hash, err,
		)
	}
	return location, true, nil
}

func blobLocationGeneration() (string, error) {
	id, err := newUUIDv4()
	if err != nil {
		return "", fmt.Errorf("creating blob-location generation: %w", err)
	}
	return id, nil
}

func writeLooseLocationTx(
	ctx context.Context,
	tx *sql.Tx,
	storeID string,
	hash string,
	physical BlobPhysical,
) error {
	generation, err := blobLocationGeneration()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO blob_locations(
			blob_hash, store_id, generation, kind, encoding, stored_size, pack_eligible
		) VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(blob_hash, store_id) DO UPDATE SET
			generation=excluded.generation,
			kind=excluded.kind,
			encoding=excluded.encoding,
			stored_size=excluded.stored_size,
			pack_eligible=excluded.pack_eligible`,
		hash, storeID, generation, blobLocationKindLoose, physical.Encoding,
		physical.StoredBytes, physical.PackEligible,
	)
	if err != nil {
		return fmt.Errorf("recording loose blob location %s: %w", hash, err)
	}
	return nil
}

// AddRestoredBlobLocation grants authority to bytes independently verified
// during an explicit restore remap. The source primary remains authoritative
// until the complete mapped copy set has committed.
func (s *Store) AddRestoredBlobLocation(
	ctx context.Context,
	hash string,
	destination packstore.ReadLocation,
) error {
	if _, err := packstore.ParseHash(hash); err != nil {
		return fmt.Errorf("parsing restored blob hash: %w", err)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var size int64
		if err := tx.QueryRowContext(
			ctx, `SELECT size FROM blobs WHERE hash=?`, hash,
		).Scan(&size); err != nil {
			return fmt.Errorf("reading restored blob %s: %w", hash, err)
		}
		var lifecycle string
		if err := tx.QueryRowContext(ctx, `
			SELECT lifecycle FROM blob_stores WHERE store_id=?`,
			destination.StoreID,
		).Scan(&lifecycle); err != nil {
			return fmt.Errorf("reading restored destination %s: %w", destination.StoreID, err)
		}
		if lifecycle != blobStoreLifecycleActive {
			return fmt.Errorf(
				"restored destination %s is %s: %w",
				destination.StoreID, lifecycle, ErrBlobStoreState,
			)
		}
		if destination.Loose == nil {
			return errors.New("restored placement currently requires loose destination authority")
		}
		if err := destination.Validate(); err != nil {
			return fmt.Errorf("validating restored destination: %w", err)
		}
		if destination.Loose.LogicalSize != size {
			return fmt.Errorf(
				"restored destination size %d does not match logical size %d",
				destination.Loose.LogicalSize, size,
			)
		}
		if destination.Loose.StoredSize < 0 {
			return errors.New("restored loose destination has negative stored size")
		}
		encoding, err := looseEncodingName(destination.Loose.Encoding)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO blob_locations(
				blob_hash,store_id,generation,kind,encoding,stored_size,pack_eligible
			) VALUES(?,?,?,'loose',?,?,?)
			ON CONFLICT(blob_hash,store_id) DO UPDATE SET
				generation=excluded.generation,
				kind=excluded.kind,
				encoding=excluded.encoding,
				stored_size=excluded.stored_size,
				pack_eligible=excluded.pack_eligible`,
			hash, destination.StoreID, destination.Generation, encoding,
			destination.Loose.StoredSize, size <= maxPackEligibleBytes,
		)
		if err != nil {
			return fmt.Errorf("recording restored location for %s: %w", hash, err)
		}
		return nil
	})
}

// RetireRestoredPrimary revokes the temporary local restore copy only after a
// verified secondary is authoritative. Audited bytes remain primary-pinned
// unless the restore mapping explicitly acknowledges remote-only retention.
func (s *Store) RetireRestoredPrimary(
	ctx context.Context, hash string, allowAuditedRemoteOnly bool,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var secondaryCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM blob_locations
			WHERE blob_hash=? AND store_id<>?`,
			hash, s.primaryStoreID,
		).Scan(&secondaryCount); err != nil {
			return fmt.Errorf("checking restored secondary authority for %s: %w", hash, err)
		}
		if secondaryCount == 0 {
			return fmt.Errorf("blob %s has no restored secondary authority", hash)
		}
		var primaryKind string
		if err := tx.QueryRowContext(ctx, `
			SELECT kind FROM blob_locations WHERE blob_hash=? AND store_id=?`,
			hash, s.primaryStoreID,
		).Scan(&primaryKind); err != nil {
			return fmt.Errorf("reading restored primary authority for %s: %w", hash, err)
		}
		if primaryKind != blobLocationKindLoose {
			return fmt.Errorf("restored primary authority for %s is not loose", hash)
		}
		if !allowAuditedRemoteOnly {
			var audited bool
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS(
					SELECT 1
					FROM content_versions version
					JOIN audit_memberships membership
					  ON membership.node_id=version.node_id
					WHERE version.blob_hash=?
				)`, hash).Scan(&audited); err != nil {
				return fmt.Errorf("checking restored audit retention for %s: %w", hash, err)
			}
			if audited {
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM blob_locations
			WHERE blob_hash=? AND store_id=? AND kind='loose'`,
			hash, s.primaryStoreID,
		); err != nil {
			return fmt.Errorf("retiring restored primary location for %s: %w", hash, err)
		}
		return nil
	})
}
