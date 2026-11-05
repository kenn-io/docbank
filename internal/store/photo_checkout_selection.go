package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
)

// Checkout has a fixed selector, not a search expression. Keep each selector's
// query separate so a small ID or album selection can seek its membership.
func (s *Store) checkoutSelectionIDsTx(ctx context.Context, tx *sql.Tx, selection PhotoCheckoutSelection) ([]string, error) {
	var selectors []compiledQueryFragment
	if selection.All {
		selectors = append(selectors, compiledQueryFragment{sql: "TRUE"})
	} else {
		if len(selection.AssetIDs) > 0 {
			args := make([]any, len(selection.AssetIDs))
			for i, id := range selection.AssetIDs {
				args[i] = id
			}
			selectors = append(selectors, compiledQueryFragment{sql: `a.asset_id IN (` + numberedPlaceholders(len(selection.AssetIDs), 0) + `)`, args: args})
		}
		if len(selection.AlbumIDs) > 0 {
			args := make([]any, len(selection.AlbumIDs))
			for i, id := range selection.AlbumIDs {
				args[i] = id
			}
			selectors = append(selectors, compiledQueryFragment{sql: `a.asset_id IN (SELECT asset_id FROM photo_set_members WHERE set_id IN (` + numberedPlaceholders(len(selection.AlbumIDs), 0) + `))`, args: args})
		}
		if years := selection.Years; years != nil {
			filter := compiledQueryFragment{sql: photoCaptureDatePredicate("v", ">=", "$1"), args: []any{fmt.Sprintf("%04d-01-01", years.From)}}
			if years.To < 9999 {
				filter.sql += ` AND ` + photoCaptureDatePredicate("v", "<", "$2")
				filter.args = append(filter.args, fmt.Sprintf("%04d-01-01", years.To+1))
			}
			selectors = append(selectors, filter)
		}
	}
	var ids []string
	for _, selector := range selectors {
		rows, err := tx.QueryContext(ctx, `SELECT a.asset_id FROM photo_assets a
 JOIN photo_files f ON f.file_id=a.display_file_id JOIN nodes n ON n.id=f.node_id
 JOIN content_versions v ON v.version_id=n.current_version_id
 WHERE `+selector.sql+` AND `+photoBrowseLiveDisplay+` AND `+photoVisibilityPredicate(false), selector.args...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	sort.Strings(ids)
	return slices.Compact(ids), nil
}
