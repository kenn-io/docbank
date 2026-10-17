package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
)

const photoXMPPurgeReceipts = `SELECT receipt_id FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_node WHERE operation='xmp_sync' AND json_extract(after_json,'$.node_id') IN (SELECT value FROM json_each(?1)) UNION SELECT receipt_id FROM photo_change_receipts INDEXED BY photo_change_receipts_xmp_sidecar WHERE operation='xmp_sync' AND json_extract(after_json,'$.sync.sidecar.node_id') IN (SELECT value FROM json_each(?1))`

// adjustPhotosForPurgedNodesTx removes memberships before a permanent node
// deletion. Assets remain as empty, revisioned graph rows so receipts and later
// restore/import validation retain their identity.
func adjustPhotosForPurgedNodesTx(ctx context.Context, tx *sql.Tx, nodeIDs []int64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	assetIDs := make(map[string]struct{})
	var targetFileIDs []string
	var photoNodeIDs []int64
	for _, nodeID := range nodeIDs {
		rows, err := tx.QueryContext(ctx, `SELECT COALESCE(asset_id,''), file_id, role FROM photo_files WHERE node_id=?`, nodeID)
		if err != nil {
			return fmt.Errorf("finding photo memberships for purged node %d: %w", nodeID, err)
		}
		for rows.Next() {
			var assetID, fileID, role string
			if err := rows.Scan(&assetID, &fileID, &role); err != nil {
				_ = rows.Close() //nolint:sqlclosecheck // close before returning the scan error.
				return err
			}
			photoNodeIDs = append(photoNodeIDs, nodeID)
			if assetID == "" {
				continue
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
	before := make(map[string]PhotoAsset, len(assetIDs))
	for assetID := range assetIDs {
		asset, err := photoAssetByIDQuery(ctx, tx, assetID)
		if err != nil {
			return err
		}
		before[assetID] = asset
	}
	for _, targetFileID := range targetFileIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET asset_id=NULL,sidecar_of_file_id=NULL WHERE sidecar_of_file_id=?`, targetFileID); err != nil {
			return fmt.Errorf("removing sidecars for purged photo source %s: %w", targetFileID, err)
		}
	}
	if len(photoNodeIDs) > 0 {
		encoded, err := json.Marshal(photoNodeIDs)
		if err != nil {
			return fmt.Errorf("encoding purged photo nodes: %w", err)
		}
		// An XMP receipt naming a purged original or sidecar goes with it.
		if _, err := tx.ExecContext(ctx, `WITH RECURSIVE nodes_to_purge(id) AS (SELECT value FROM json_each(?)), doomed(receipt_id) AS (`+photoXMPPurgeReceipts+`
 UNION
 SELECT r.receipt_id FROM photo_change_receipts r
 WHERE r.operation IN ('authored','authored_undo','authored_sidecar') AND
 (EXISTS (SELECT 1 FROM json_each(r.after_json,'$.after') entry WHERE json_extract(entry.value,'$.node_id') IN (SELECT id FROM nodes_to_purge))
 OR json_extract(r.after_json,'$.sidecar.node_id') IN (SELECT id FROM nodes_to_purge))
 UNION
 SELECT r.receipt_id FROM photo_change_receipts r JOIN doomed d ON json_extract(r.after_json,'$.undo_of')=d.receipt_id
 WHERE r.operation='authored_undo'
 ) DELETE FROM photo_change_receipts WHERE receipt_id IN (SELECT receipt_id FROM doomed)`, string(encoded)); err != nil {
			return fmt.Errorf("removing authored receipts for purged nodes: %w", err)
		}
	}
	for _, nodeID := range nodeIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM photo_files WHERE node_id=?`, nodeID); err != nil {
			return fmt.Errorf("removing photo membership for purged node %d: %w", nodeID, err)
		}
	}
	for _, old := range before {
		if _, err := commitPhotoAssetTx(ctx, tx, old, old, "purge"); err != nil {
			return fmt.Errorf("repairing photo asset %s after purge: %w", old.ID, err)
		}
		if err := validatePhotoAssetGraph(ctx, tx, old.ID); err != nil {
			return err
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
