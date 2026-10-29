package store

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *Store) insertAuditMembershipTx(ctx context.Context, tx *sql.Tx, scopeID string, nodeID uint64, baseline string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_memberships(scope_id,node_id,baseline_digest) VALUES($1,$2,$3)`, scopeID, nodeID, baseline); err != nil {
		return err
	}
	return s.queueAuditPhotosTx(ctx, tx, &nodeID)
}

// A nil node queues all eligible assets when authority first becomes active.
// Membership changes queue the whole asset, including when only a sidecar is
// enrolled. Both writes belong to the caller's storage transaction.
func (s *Store) queueAuditPhotosTx(ctx context.Context, tx *sql.Tx, nodeID *uint64) error {
	if s.postgresSchema == "" {
		// Released SQLite layouts own this effect in triggers. Their replacement
		// requires the supported layout cutover; do not run both queue owners.
		return nil
	}
	query := `INSERT INTO photo_xmp_unsettled(asset_id)
  SELECT DISTINCT asset_id FROM photo_xmp_inputs WHERE eligible`
	var args []any
	if nodeID != nil {
		query += ` AND asset_id IN (SELECT asset_id FROM photo_files WHERE node_id=$1)`
		args = []any{*nodeID}
	}
	query += ` ON CONFLICT(asset_id) DO NOTHING`
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("queueing audit photo re-export: %w", err)
	}
	return nil
}
