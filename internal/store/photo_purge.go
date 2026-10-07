package store

import (
	"context"
	"database/sql"
	"fmt"
)

// adjustPhotosForPurgedNodesTx removes memberships before a permanent node
// deletion. Assets remain as empty, revisioned graph rows so receipts and later
// restore/import validation retain their identity.
func adjustPhotosForPurgedNodesTx(ctx context.Context, tx *sql.Tx, nodeIDs []int64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	assetIDs := make(map[string]struct{})
	var targetFileIDs []string
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
			if photoSidecarTargetRole(role) {
				targetFileIDs = append(targetFileIDs, fileID)
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
	for _, targetFileID := range targetFileIDs {
		rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM photo_files WHERE sidecar_of_file_id=?`, targetFileID)
		if err != nil {
			return fmt.Errorf("finding sidecars for purged photo source %s: %w", targetFileID, err)
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
		asset, err := photoAssetByIDQuery(ctx, tx, assetID)
		if err != nil {
			return err
		}
		before[assetID] = asset
	}
	for _, targetFileID := range targetFileIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE sidecar_of_file_id=?`, targetFileID); err != nil {
			return fmt.Errorf("removing sidecars for purged photo source %s: %w", targetFileID, err)
		}
	}
	for _, nodeID := range nodeIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE node_id=?`, nodeID); err != nil {
			return fmt.Errorf("removing photo membership for purged node %d: %w", nodeID, err)
		}
	}
	retiredBySet := make(map[string][]string)
	for _, old := range before {
		asset, err := commitPhotoAssetTx(ctx, tx, old, old, "purge")
		if err != nil {
			return fmt.Errorf("repairing photo asset %s after purge: %w", old.ID, err)
		}
		if err := validatePhotoAssetGraph(ctx, tx, old.ID); err != nil {
			return err
		}
		if len(asset.Files) != 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT set_id FROM photo_set_members WHERE asset_id=?`, asset.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var setID string
			if err := rows.Scan(&setID); err != nil {
				_ = rows.Close()
				return err
			}
			retiredBySet[setID] = append(retiredBySet[setID], asset.ID)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for setID, ids := range retiredBySet {
		set, err := photoSetByID(ctx, tx, setID)
		if err != nil {
			return err
		}
		if _, err := changePhotoSetMembersTx(ctx, tx, set, false, ids); err != nil {
			return fmt.Errorf("removing purged photo members from album %s: %w", setID, err)
		}
	}
	return nil
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
