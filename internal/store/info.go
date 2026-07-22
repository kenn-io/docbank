package store

import (
	"context"
	"database/sql"
	"fmt"
)

// VaultInfo summarizes the logical authority held by a vault. Physical loose
// and packed storage is reported separately by the blob store.
type VaultInfo struct {
	VaultID             string
	LiveFiles           int64
	LiveDirectories     int64
	TrashedNodes        int64
	ContentVersions     int64
	LogicalVersionBytes int64
	TrackedBlobs        int64
	TrackedBlobBytes    int64
}

// Info returns a point-in-time logical inventory without changing the vault.
func (s *Store) Info(ctx context.Context) (VaultInfo, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return VaultInfo{}, fmt.Errorf("starting vault inventory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	info := VaultInfo{VaultID: s.vaultID}
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN parent_id IS NOT NULL AND trashed_at IS NULL AND kind = 'file'
				THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN parent_id IS NOT NULL AND trashed_at IS NULL AND kind = 'dir'
				THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN trashed_at IS NOT NULL THEN 1 ELSE 0 END), 0)
		FROM nodes`).Scan(&info.LiveFiles, &info.LiveDirectories, &info.TrashedNodes); err != nil {
		return VaultInfo{}, fmt.Errorf("counting vault nodes: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0) FROM content_versions`).Scan(
		&info.ContentVersions, &info.LogicalVersionBytes,
	); err != nil {
		return VaultInfo{}, fmt.Errorf("counting content versions: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs`).Scan(
		&info.TrackedBlobs, &info.TrackedBlobBytes,
	); err != nil {
		return VaultInfo{}, fmt.Errorf("counting tracked blobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return VaultInfo{}, fmt.Errorf("finishing vault inventory: %w", err)
	}
	return info, nil
}
