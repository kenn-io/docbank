package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"go.kenn.io/kit/packstore"
)

// PackCatalog adapts docbank's blobs membership table and packed metadata to
// Kit's application-neutral physical storage engine. A blobs row grants read
// authority; nodes, versions, and future external pins decide only whether GC
// may remove that row.
type PackCatalog struct{ store *Store }

// NewPackCatalog constructs a packed-storage catalog over s.
func NewPackCatalog(s *Store) *PackCatalog { return &PackCatalog{store: s} }

var _ packstore.Catalog = (*PackCatalog)(nil)

func (c *PackCatalog) Resolve(ctx context.Context, hash packstore.Hash) (packstore.Location, error) {
	row := c.store.db.QueryRowContext(ctx, `
		SELECT i.pack_id, i.pack_offset, i.stored_len, i.raw_len, i.flags, i.crc32c
		FROM blobs b
		LEFT JOIN blob_pack_index i ON i.blob_hash = b.hash
		WHERE b.hash = ?`, hash.String())
	var packID sql.NullString
	var offset, stored, raw, flags, crc sql.NullInt64
	if err := row.Scan(&packID, &offset, &stored, &raw, &flags, &crc); err != nil {
		if err == sql.ErrNoRows {
			return packstore.Location{}, nil
		}
		return packstore.Location{}, fmt.Errorf("resolving packed blob %s: %w", hash, err)
	}
	location := packstore.Location{Member: true}
	if !packID.Valid {
		return location, nil
	}
	if !offset.Valid || !stored.Valid || !raw.Valid || !flags.Valid || !crc.Valid ||
		flags.Int64 < 0 || flags.Int64 > math.MaxUint8 || crc.Int64 < 0 || crc.Int64 > math.MaxUint32 {
		return packstore.Location{}, fmt.Errorf("packed blob %s has invalid footer metadata", hash)
	}
	entry := packstore.IndexEntry{Hash: hash, PackID: packID.String,
		Offset: offset.Int64, StoredLen: stored.Int64, RawLen: raw.Int64,
		Flags: uint8(flags.Int64), CRC32C: uint32(crc.Int64)}
	if err := entry.Validate(); err != nil {
		return packstore.Location{}, fmt.Errorf("validating packed blob %s: %w", hash, err)
	}
	location.Pack = &entry
	return location, nil
}

func (c *PackCatalog) ListReferences(ctx context.Context) (packstore.ReferenceInventory, error) {
	rows, err := c.store.db.QueryContext(ctx, `SELECT hash FROM blobs ORDER BY hash`)
	if err != nil {
		return packstore.ReferenceInventory{}, fmt.Errorf("listing blob references: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []packstore.Reference
	complete := true
	for rows.Next() {
		var original string
		if err := rows.Scan(&original); err != nil {
			return packstore.ReferenceInventory{}, fmt.Errorf("scanning blob reference: %w", err)
		}
		hash, err := packstore.ParseHash(original)
		if err != nil {
			complete = false
			slog.Error("malformed blob hash; suppressing destructive packed-storage repair",
				"hash", original, "error", err)
			continue
		}
		refs = append(refs, packstore.Reference{Hash: hash, OriginalHashes: []string{original}})
	}
	if err := rows.Err(); err != nil {
		return packstore.ReferenceInventory{}, fmt.Errorf("listing blob references: %w", err)
	}
	return packstore.ReferenceInventory{References: refs, Complete: complete}, nil
}

func (c *PackCatalog) ListUnpacked(ctx context.Context) ([]packstore.Candidate, error) {
	rows, err := c.store.db.QueryContext(ctx, `
		SELECT b.hash, b.size FROM blobs b
		WHERE NOT EXISTS (SELECT 1 FROM blob_pack_index i WHERE i.blob_hash = b.hash)
		ORDER BY b.hash`)
	if err != nil {
		return nil, fmt.Errorf("listing unpacked blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []packstore.Candidate
	for rows.Next() {
		var original string
		var size int64
		if err := rows.Scan(&original, &size); err != nil {
			return nil, fmt.Errorf("scanning unpacked blob: %w", err)
		}
		hash, err := packstore.ParseHash(original)
		if err != nil {
			slog.Error("malformed unpacked blob hash; preserving loose content",
				"hash", original, "error", err)
			continue
		}
		result = append(result, packstore.Candidate{Hash: hash,
			OriginalHashes: []string{original}, Paths: []string{hash.String()[:2] + "/" + hash.String()}, Size: size})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing unpacked blobs: %w", err)
	}
	return result, nil
}

func (c *PackCatalog) ListIndexed(ctx context.Context) ([]packstore.IndexEntry, error) {
	return c.listEntries(ctx, `
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM blob_pack_index ORDER BY blob_hash`)
}

func (c *PackCatalog) ListPackRecords(ctx context.Context) ([]packstore.PackRecord, error) {
	rows, err := c.store.db.QueryContext(ctx, `
		SELECT pack_id, entry_count, stored_bytes, created_at FROM blob_packs ORDER BY created_at, pack_id`)
	if err != nil {
		return nil, fmt.Errorf("listing blob packs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []packstore.PackRecord
	for rows.Next() {
		record, err := scanPackRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing blob packs: %w", err)
	}
	return result, nil
}

func (c *PackCatalog) ListPackEntries(ctx context.Context, packID string) ([]packstore.IndexEntry, error) {
	return c.listEntries(ctx, `
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM blob_pack_index WHERE pack_id = ? ORDER BY blob_hash`, packID)
}

func (c *PackCatalog) HasPackRecord(ctx context.Context, packID string) (bool, error) {
	var exists bool
	if err := c.store.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM blob_packs WHERE pack_id = ?)`, packID).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking blob pack %s: %w", packID, err)
	}
	return exists, nil
}

func (c *PackCatalog) PruneUnreferenced(ctx context.Context) (int64, error) {
	result, err := c.store.db.ExecContext(ctx, `
		DELETE FROM blob_pack_index
		WHERE NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash = blob_pack_index.blob_hash)`)
	if err != nil {
		return 0, fmt.Errorf("pruning unreferenced pack mappings: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned pack mappings: %w", err)
	}
	return n, nil
}

func (c *PackCatalog) RecordPack(ctx context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	return c.writePack(ctx, record, adoptions, false)
}

func (c *PackCatalog) AdoptPack(ctx context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	return c.writePack(ctx, record, adoptions, true)
}

func (c *PackCatalog) writePack(ctx context.Context, record packstore.PackRecord,
	adoptions []packstore.Adoption, replace bool) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("validating blob pack %s: %w", record.PackID, err)
	}
	return c.store.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
			VALUES (?, ?, ?, ?)`, record.PackID, record.EntryCount, record.StoredBytes,
			record.CreatedAt.UTC().Format(timestampLayout)); err != nil {
			return fmt.Errorf("recording blob pack %s: %w", record.PackID, err)
		}
		for _, adoption := range adoptions {
			if err := adoption.Entry.Validate(); err != nil {
				return fmt.Errorf("validating packed blob %s: %w", adoption.Entry.Hash, err)
			}
			if adoption.Entry.PackID != record.PackID {
				return fmt.Errorf("pack entry %s names %s, expected %s",
					adoption.Entry.Hash, adoption.Entry.PackID, record.PackID)
			}
			if err := writeAdoption(ctx, tx, adoption.Entry, replace); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeAdoption(ctx context.Context, tx *sql.Tx, entry packstore.IndexEntry, replace bool) error {
	if replace {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO blob_pack_index
				(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			SELECT ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM blobs WHERE hash = ?)
			ON CONFLICT(blob_hash) DO UPDATE SET
				pack_id=excluded.pack_id, pack_offset=excluded.pack_offset,
				stored_len=excluded.stored_len, raw_len=excluded.raw_len,
				flags=excluded.flags, crc32c=excluded.crc32c`,
			entry.Hash.String(), entry.PackID, entry.Offset, entry.StoredLen, entry.RawLen,
			entry.Flags, entry.CRC32C, entry.Hash.String())
		if err != nil {
			return fmt.Errorf("adopting packed blob %s: %w", entry.Hash, err)
		}
		return requireOneRow(result, "adopting packed blob "+entry.Hash.String())
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO blob_pack_index
			(blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
		SELECT ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM blobs WHERE hash = ?)`,
		entry.Hash.String(), entry.PackID, entry.Offset, entry.StoredLen, entry.RawLen,
		entry.Flags, entry.CRC32C, entry.Hash.String())
	if err != nil {
		return fmt.Errorf("recording packed blob %s: %w", entry.Hash, err)
	}
	return requireOneRow(result, "recording packed blob "+entry.Hash.String())
}

func (c *PackCatalog) DeletePackRecord(ctx context.Context, packID string) error {
	if _, err := c.store.db.ExecContext(ctx, `DELETE FROM blob_packs WHERE pack_id = ?`, packID); err != nil {
		return fmt.Errorf("deleting blob pack %s: %w", packID, err)
	}
	return nil
}

func (c *PackCatalog) DeleteIndexEntry(ctx context.Context, hash packstore.Hash) error {
	if _, err := c.store.db.ExecContext(ctx,
		`DELETE FROM blob_pack_index WHERE blob_hash = ?`, hash.String()); err != nil {
		return fmt.Errorf("deleting packed blob mapping %s: %w", hash, err)
	}
	return nil
}

func (c *PackCatalog) ListPackUsage(ctx context.Context) ([]packstore.PackUsage, error) {
	rows, err := c.store.db.QueryContext(ctx, `
		SELECT p.pack_id, p.entry_count, p.stored_bytes, p.created_at,
		       COUNT(b.hash),
		       COALESCE(SUM(CASE WHEN b.hash IS NOT NULL THEN i.stored_len ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN b.hash IS NOT NULL THEN i.raw_len ELSE 0 END), 0),
		       COALESCE(MAX(CASE WHEN b.hash IS NOT NULL THEN i.stored_len ELSE 0 END), 0),
		       COALESCE(MAX(CASE WHEN b.hash IS NOT NULL THEN i.raw_len ELSE 0 END), 0)
		FROM blob_packs p
		LEFT JOIN blob_pack_index i ON i.pack_id = p.pack_id
		LEFT JOIN blobs b ON b.hash = i.blob_hash
		GROUP BY p.pack_id, p.entry_count, p.stored_bytes, p.created_at
		ORDER BY p.created_at, p.pack_id`)
	if err != nil {
		return nil, fmt.Errorf("listing blob pack usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []packstore.PackUsage
	for rows.Next() {
		var packID, created string
		var usage packstore.PackUsage
		if err := rows.Scan(&packID, &usage.EntryCount, &usage.StoredBytes, &created,
			&usage.LiveEntries, &usage.LiveStoredBytes, &usage.LiveRawBytes,
			&usage.MaxLiveStoredLen, &usage.MaxLiveRawLen); err != nil {
			return nil, fmt.Errorf("scanning blob pack usage: %w", err)
		}
		createdAt, err := time.Parse(timestampLayout, created)
		if err != nil {
			return nil, fmt.Errorf("parsing blob pack %s creation time: %w", packID, err)
		}
		usage.PackID, usage.CreatedAt = packID, createdAt
		result = append(result, usage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing blob pack usage: %w", err)
	}
	return result, nil
}

func (c *PackCatalog) ListLivePackEntries(ctx context.Context, packID string) ([]packstore.IndexEntry, error) {
	return c.listEntries(ctx, `
		SELECT i.blob_hash, i.pack_id, i.pack_offset, i.stored_len, i.raw_len, i.flags, i.crc32c
		FROM blob_pack_index i JOIN blobs b ON b.hash = i.blob_hash
		WHERE i.pack_id = ? ORDER BY i.blob_hash`, packID)
}

func (c *PackCatalog) CommitRepack(ctx context.Context, sourceIDs []string,
	records []packstore.PackRecord, moves []packstore.RepackMove) error {
	sources := make(map[string]struct{}, len(sourceIDs))
	for _, id := range sourceIDs {
		if _, duplicate := sources[id]; duplicate {
			return fmt.Errorf("duplicate repack source %s", id)
		}
		sources[id] = struct{}{}
	}
	newPacks := make(map[string]struct{}, len(records))
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("validating replacement pack %s: %w", record.PackID, err)
		}
		if _, duplicate := newPacks[record.PackID]; duplicate {
			return fmt.Errorf("duplicate replacement pack %s", record.PackID)
		}
		newPacks[record.PackID] = struct{}{}
	}
	expected := make(map[packstore.Hash]string, len(moves))
	for _, move := range moves {
		if err := move.NewEntry.Validate(); err != nil {
			return fmt.Errorf("validating replacement blob %s: %w", move.NewEntry.Hash, err)
		}
		if _, ok := sources[move.OldPackID]; !ok {
			return fmt.Errorf("repack move for %s names unknown source %s", move.NewEntry.Hash, move.OldPackID)
		}
		if _, ok := newPacks[move.NewEntry.PackID]; !ok {
			return fmt.Errorf("repack move for %s names unknown replacement %s", move.NewEntry.Hash, move.NewEntry.PackID)
		}
		if _, duplicate := expected[move.NewEntry.Hash]; duplicate {
			return fmt.Errorf("duplicate repack move for %s", move.NewEntry.Hash)
		}
		expected[move.NewEntry.Hash] = move.OldPackID
	}
	return c.store.withTx(ctx, func(tx *sql.Tx) error {
		actual, err := liveMappingsForPacks(ctx, tx, sourceIDs)
		if err != nil {
			return err
		}
		if len(actual) != len(expected) {
			return fmt.Errorf("repack live mapping set changed: expected %d, found %d", len(expected), len(actual))
		}
		for hash, oldID := range expected {
			if actual[hash] != oldID {
				return fmt.Errorf("repack mapping changed for %s", hash)
			}
		}
		for _, record := range records {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO blob_packs (pack_id, entry_count, stored_bytes, created_at)
				VALUES (?, ?, ?, ?)`, record.PackID, record.EntryCount, record.StoredBytes,
				record.CreatedAt.UTC().Format(timestampLayout)); err != nil {
				return fmt.Errorf("recording replacement pack %s: %w", record.PackID, err)
			}
		}
		for _, move := range moves {
			e := move.NewEntry
			result, err := tx.ExecContext(ctx, `
				UPDATE blob_pack_index
				SET pack_id = ?, pack_offset = ?, stored_len = ?, raw_len = ?, flags = ?, crc32c = ?
				WHERE blob_hash = ? AND pack_id = ?
				  AND EXISTS (SELECT 1 FROM blobs WHERE hash = ?)`,
				e.PackID, e.Offset, e.StoredLen, e.RawLen, e.Flags, e.CRC32C,
				e.Hash.String(), move.OldPackID, e.Hash.String())
			if err != nil {
				return fmt.Errorf("moving packed blob %s: %w", e.Hash, err)
			}
			if err := requireOneRow(result, "moving packed blob "+e.Hash.String()); err != nil {
				return err
			}
		}
		return nil
	})
}

func liveMappingsForPacks(ctx context.Context, tx *sql.Tx, packIDs []string) (map[packstore.Hash]string, error) {
	if len(packIDs) == 0 {
		return map[packstore.Hash]string{}, nil
	}
	args := make([]any, len(packIDs))
	for i, id := range packIDs {
		args[i] = id
	}
	query := `SELECT i.blob_hash, i.pack_id FROM blob_pack_index i
		JOIN blobs b ON b.hash = i.blob_hash WHERE i.pack_id IN (` + placeholders(len(packIDs)) + `)`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("checking repack mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[packstore.Hash]string)
	for rows.Next() {
		var raw, packID string
		if err := rows.Scan(&raw, &packID); err != nil {
			return nil, fmt.Errorf("scanning repack mapping: %w", err)
		}
		hash, err := packstore.ParseHash(raw)
		if err != nil {
			return nil, fmt.Errorf("parsing repack mapping hash %q: %w", raw, err)
		}
		result[hash] = packID
	}
	return result, rows.Err()
}

func (c *PackCatalog) DeleteEmptyPackRecord(ctx context.Context, packID string) (bool, error) {
	result, err := c.store.db.ExecContext(ctx, `
		DELETE FROM blob_packs WHERE pack_id = ?
		  AND NOT EXISTS (SELECT 1 FROM blob_pack_index WHERE pack_id = ?)`, packID, packID)
	if err != nil {
		return false, fmt.Errorf("deleting empty blob pack %s: %w", packID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting deleted blob pack %s: %w", packID, err)
	}
	return n == 1, nil
}

func (c *PackCatalog) ClearPackMetadata(ctx context.Context) error {
	return c.store.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM blob_pack_index`); err != nil {
			return fmt.Errorf("clearing packed blob mappings: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM blob_packs`); err != nil {
			return fmt.Errorf("clearing blob pack records: %w", err)
		}
		return nil
	})
}

func (c *PackCatalog) listEntries(ctx context.Context, query string, args ...any) ([]packstore.IndexEntry, error) {
	rows, err := c.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing packed blob mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []packstore.IndexEntry
	for rows.Next() {
		entry, err := scanPackEntry(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing packed blob mappings: %w", err)
	}
	return result, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanPackEntry(row scanner) (packstore.IndexEntry, error) {
	var raw string
	var entry packstore.IndexEntry
	var flags, crc int64
	if err := row.Scan(&raw, &entry.PackID, &entry.Offset, &entry.StoredLen, &entry.RawLen, &flags, &crc); err != nil {
		return packstore.IndexEntry{}, fmt.Errorf("scanning packed blob mapping: %w", err)
	}
	if flags < 0 || flags > math.MaxUint8 || crc < 0 || crc > math.MaxUint32 {
		return packstore.IndexEntry{}, fmt.Errorf("packed blob %s has out-of-range footer metadata", raw)
	}
	hash, err := packstore.ParseHash(raw)
	if err != nil {
		return packstore.IndexEntry{}, fmt.Errorf("parsing packed blob hash %q: %w", raw, err)
	}
	entry.Hash, entry.Flags, entry.CRC32C = hash, uint8(flags), uint32(crc)
	if err := entry.Validate(); err != nil {
		return packstore.IndexEntry{}, fmt.Errorf("validating packed blob %s: %w", hash, err)
	}
	return entry, nil
}

func scanPackRecord(row scanner) (packstore.PackRecord, error) {
	var record packstore.PackRecord
	var created string
	if err := row.Scan(&record.PackID, &record.EntryCount, &record.StoredBytes, &created); err != nil {
		return packstore.PackRecord{}, fmt.Errorf("scanning blob pack: %w", err)
	}
	createdAt, err := time.Parse(timestampLayout, created)
	if err != nil {
		return packstore.PackRecord{}, fmt.Errorf("parsing blob pack %s creation time: %w", record.PackID, err)
	}
	record.CreatedAt = createdAt
	if err := record.Validate(); err != nil {
		return packstore.PackRecord{}, fmt.Errorf("validating blob pack %s: %w", record.PackID, err)
	}
	return record, nil
}

func requireOneRow(result sql.Result, operation string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if n != 1 {
		return fmt.Errorf("%s: expected one affected row, got %d", operation, n)
	}
	return nil
}

func placeholders(n int) string {
	values := make([]string, n)
	for i := range values {
		values[i] = "?"
	}
	return strings.Join(values, ",")
}
