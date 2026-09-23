package store

import (
	"context"
	"database/sql"
	"fmt"
)

// adjustPhotosForPurgedNodesTx removes memberships before trash-empty deletes
// nodes. Assets remain as empty, revisioned graph rows so receipts and later
// restore/import validation retain their identity.
func (s *Store) adjustPhotosForPurgedNodesTx(ctx context.Context, tx *sql.Tx, nodeIDs []int64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	assetIDs := make(map[string]struct{})
	var rawFileIDs []string
	for _, nodeID := range nodeIDs {
		rows, err := tx.QueryContext(ctx, `SELECT asset_id, file_id, role FROM photo_files WHERE node_id=?`, nodeID)
		if err != nil {
			return fmt.Errorf("finding photo memberships for purged node %d: %w", nodeID, err)
		}
		for rows.Next() {
			var assetID, fileID, role string
			if err := rows.Scan(&assetID, &fileID, &role); err != nil {
				_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
				return err
			}
			assetIDs[assetID] = struct{}{}
			if role == PhotoRoleRAW {
				rawFileIDs = append(rawFileIDs, fileID)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for _, rawFileID := range rawFileIDs {
		rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM photo_files WHERE sidecar_of_file_id=?`, rawFileID)
		if err != nil {
			return fmt.Errorf("finding sidecars for purged RAW %s: %w", rawFileID, err)
		}
		for rows.Next() {
			var assetID string
			if err := rows.Scan(&assetID); err != nil {
				_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
				return err
			}
			assetIDs[assetID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if len(assetIDs) == 0 {
		return nil
	}
	before := make(map[string]PhotoAsset, len(assetIDs))
	for assetID := range assetIDs {
		asset, err := loadPhotoAssetTx(ctx, tx, assetID)
		if err != nil {
			return err
		}
		before[assetID] = asset
	}
	for _, rawFileID := range rawFileIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE sidecar_of_file_id=?`, rawFileID); err != nil {
			return fmt.Errorf("removing sidecars for purged RAW %s: %w", rawFileID, err)
		}
	}
	for _, nodeID := range nodeIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE node_id=?`, nodeID); err != nil {
			return fmt.Errorf("removing photo membership for purged node %d: %w", nodeID, err)
		}
	}
	settings, err := photoSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	for assetID, old := range before {
		asset := old
		files, err := loadPhotoFilesTx(ctx, tx, assetID)
		if err != nil {
			return err
		}
		asset.Files = files
		if asset.DisplayOverrideFileID != nil && !photoFileIDPresent(files, *asset.DisplayOverrideFileID) {
			asset.DisplayOverrideFileID = nil
		}
		choice := selectPhotoDisplay(files, settings.Preference, asset.DisplayOverrideFileID)
		asset.DisplayFileID = choice.FileID
		asset.DisplaySource = choice.Source
		asset.Revision++
		if _, err := tx.ExecContext(ctx, `
			UPDATE photo_assets SET display_file_id=?, display_override_file_id=?,
				revision=?, updated_at=? WHERE asset_id=?`,
			nullablePhotoString(asset.DisplayFileID), nullablePhotoString(asset.DisplayOverrideFileID),
			asset.Revision, nowRFC3339(), assetID); err != nil {
			return fmt.Errorf("repairing photo asset %s after purge: %w", assetID, err)
		}
		changes := photoAssetMemberChanges(old, asset)
		if err := writePhotoReceiptTx(ctx, tx, "purge", assetID, "", old.Revision, asset.Revision, photoAssetState(old, changes), photoAssetState(asset, changes)); err != nil {
			return err
		}
	}
	return validatePhotoGraphTx(ctx, tx)
}

func photoFileIDPresent(files []PhotoFile, id string) bool {
	for _, file := range files {
		if file.ID == id {
			return true
		}
	}
	return false
}

func doomedNodeIDsTx(ctx context.Context, tx *sql.Tx, selection string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE roots(id) AS (`+selection+`),
		doomed(id) AS (
		  SELECT id FROM roots
		  UNION ALL
		  SELECT n.id FROM nodes n JOIN doomed d ON n.parent_id=d.id
		)
		SELECT id FROM doomed ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
