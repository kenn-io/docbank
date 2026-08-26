package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/packstore"
)

// DerivativePackPurgeTarget is one crash-durable physical pack retirement.
type DerivativePackPurgeTarget struct {
	StoreID string
	PackID  string
}

// RecordDerivativePackPurgeTargets freezes every packed location for the
// exact logical purge receipt before blob membership can be deleted.
func (s *Store) RecordDerivativePackPurgeTargets(ctx context.Context, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	args := make([]any, len(hashes))
	for index, hash := range hashes {
		if _, err := packstore.ParseHash(hash); err != nil {
			return fmt.Errorf("parsing derivative pack purge hash %q: %w", hash, err)
		}
		args[index] = hash
	}
	query := `INSERT INTO derivative_pack_purge_pending(blob_hash,store_id,pack_id)
		SELECT i.blob_hash,i.store_id,i.pack_id
		FROM blob_pack_entries i
		JOIN derivative_blob_purge_pending p ON p.blob_hash=i.blob_hash
		WHERE i.blob_hash IN (` + placeholders(len(hashes)) + `)
		ON CONFLICT(blob_hash,store_id,pack_id) DO NOTHING`
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("recording derivative pack purge targets: %w", err)
	}
	return nil
}

// PendingDerivativePackPurgeTargets returns the exact distinct packs still
// carrying bytes named by a logical derivative purge receipt.
func (s *Store) PendingDerivativePackPurgeTargets(
	ctx context.Context,
) ([]DerivativePackPurgeTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT store_id,pack_id FROM derivative_pack_purge_pending
		ORDER BY store_id,pack_id`)
	if err != nil {
		return nil, fmt.Errorf("listing derivative pack purge targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var targets []DerivativePackPurgeTarget
	for rows.Next() {
		var target DerivativePackPurgeTarget
		if err := rows.Scan(&target.StoreID, &target.PackID); err != nil {
			return nil, fmt.Errorf("scanning derivative pack purge target: %w", err)
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// LiveDerivativePackEntries returns only still-authoritative neighbors that
// must survive retirement of a targeted immutable pack.
func (s *Store) LiveDerivativePackEntries(
	ctx context.Context, target DerivativePackPurgeTarget,
) ([]packstore.IndexEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.blob_hash,i.pack_id,i.pack_offset,i.stored_len,i.raw_len,i.flags,i.crc32c
		FROM blob_pack_entries i JOIN blobs b ON b.hash=i.blob_hash
		WHERE i.store_id=? AND i.pack_id=?
		  AND EXISTS (SELECT 1 FROM derivative_pack_purge_pending p
		              WHERE p.store_id=i.store_id AND p.pack_id=i.pack_id)
		ORDER BY i.blob_hash`, target.StoreID, target.PackID)
	if err != nil {
		return nil, fmt.Errorf("listing live derivative pack entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var entries []packstore.IndexEntry
	for rows.Next() {
		entry, err := scanPackEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ReplaceDerivativePackEntryWithLoose atomically swaps one verified live
// neighbor from the targeted pack to loose authority in the same store.
func (s *Store) ReplaceDerivativePackEntryWithLoose(
	ctx context.Context, target DerivativePackPurgeTarget,
	entry packstore.IndexEntry, receipt packstore.LooseReceipt,
) error {
	if receipt.StoreID != packstore.StoreID(target.StoreID) || receipt.Hash != entry.Hash ||
		receipt.Location.LogicalSize != entry.RawLen {
		return errors.New("derivative pack replacement receipt has the wrong identity")
	}
	if err := (packstore.ReadLocation{
		StoreID: receipt.StoreID, Generation: receipt.Generation, Loose: &receipt.Location,
	}).Validate(); err != nil {
		return fmt.Errorf("validating derivative pack replacement: %w", err)
	}
	encoding, err := looseEncodingName(receipt.Location.Encoding)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM derivative_pack_purge_pending WHERE store_id=? AND pack_id=?
		)`, target.StoreID, target.PackID).Scan(&present); err != nil {
			return err
		}
		if !present {
			return errors.New("derivative pack purge target is no longer active")
		}
		result, err := tx.ExecContext(ctx, `UPDATE blob_locations
			SET generation=?,kind='loose',encoding=?,stored_size=?,pack_eligible=?
			WHERE blob_hash=? AND store_id=? AND kind='packed'
			  AND EXISTS (SELECT 1 FROM blob_pack_entries i
			              WHERE i.blob_hash=? AND i.store_id=? AND i.pack_id=?)`,
			receipt.Generation, encoding, receipt.Location.StoredSize,
			entry.RawLen <= maxPackEligibleBytes, entry.Hash.String(), target.StoreID,
			entry.Hash.String(), target.StoreID, target.PackID)
		if err != nil {
			return fmt.Errorf("replacing derivative pack entry %s: %w", entry.Hash, err)
		}
		if err := requireOneRow(result, "replacing derivative pack entry "+entry.Hash.String()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM blob_pack_entries
			WHERE blob_hash=? AND store_id=? AND pack_id=?`,
			entry.Hash.String(), target.StoreID, target.PackID); err != nil {
			return fmt.Errorf("retiring derivative pack mapping %s: %w", entry.Hash, err)
		}
		return nil
	})
}

// CompleteDerivativePackPurge removes catalog authority and the durable target
// only after the immutable pack has been physically retired.
func (s *Store) CompleteDerivativePackPurge(
	ctx context.Context, target DerivativePackPurgeTarget,
) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM blob_pack_entries i JOIN blobs b ON b.hash=i.blob_hash
			WHERE i.store_id=? AND i.pack_id=?
		)`, target.StoreID, target.PackID).Scan(&live); err != nil {
			return err
		}
		if live {
			return errors.New("derivative pack still has live mappings")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM blob_packs
			WHERE store_id=? AND pack_id=?`, target.StoreID, target.PackID); err != nil {
			return fmt.Errorf("deleting retired derivative pack: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM derivative_pack_purge_pending
			WHERE store_id=? AND pack_id=?`, target.StoreID, target.PackID); err != nil {
			return fmt.Errorf("completing derivative pack purge target: %w", err)
		}
		return nil
	})
}
