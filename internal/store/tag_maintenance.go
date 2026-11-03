package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Tag edits change revisions of photo members; they do not change the
// occupancy of unowned sibling sidecars. SQLite triggers own its queue.
func (s *Store) refreshTagPhotoWorkTx(ctx context.Context, tx *sql.Tx, nodeFilter string, args ...any) error {
	if s.postgresSchema == "" {
		return nil
	}
	var assets []string
	err := queryRows(ctx, tx, `SELECT DISTINCT asset_id FROM photo_files WHERE asset_id IS NOT NULL AND `+nodeFilter, args, func(rows *sql.Rows) error {
		var asset string
		if err := rows.Scan(&asset); err != nil {
			return err
		}
		assets = append(assets, asset)
		return nil
	})
	if err != nil {
		return fmt.Errorf("finding photo work after tag edits: %w", err)
	}
	for _, asset := range assets {
		if err := s.refreshPhotoAssetWorkTx(ctx, tx, asset); err != nil {
			return err
		}
	}
	return nil
}
