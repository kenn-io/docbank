package store

import (
	"context"
	"errors"
)

// MissingProcessingTargetsAfter excludes published work at the selected
// profile, including after daemon restart. Failed jobs remain eligible.
func (s *Store) MissingProcessingTargetsAfter(ctx context.Context, profile, after string, bindings, imageBindings, limit int) ([]DocumentSummary, error) {
	if bindings < 1 || imageBindings < 0 || imageBindings > bindings || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid processing backfill bounds")
	}
	query := `WITH RECURSIVE live(id) AS (
 SELECT id FROM nodes WHERE parent_id IS NULL AND trashed_at IS NULL
 UNION ALL SELECT n.id FROM nodes n JOIN live l ON n.parent_id=l.id WHERE n.trashed_at IS NULL
 ) SELECT n.id,cv.version_id,n.name FROM nodes n JOIN live l ON l.id=n.id
 JOIN content_versions cv ON cv.version_id=n.current_version_id
 WHERE n.trashed_at IS NULL AND cv.version_id>$1 AND (
 (SELECT COUNT(DISTINCT h.binding_id) FROM embedding_heads h
 WHERE h.content_version_id=cv.version_id AND h.profile_fingerprint=$2 AND h.input_kind<>'image') < $3
 OR ($4>0 AND (SELECT COUNT(DISTINCT h.binding_id) FROM embedding_heads h
 WHERE h.content_version_id=cv.version_id AND h.profile_fingerprint=$2 AND h.input_kind='image') < $4
 AND ` + photoDescriptionEligibleSQL("cv.version_id") + `))
 ORDER BY cv.version_id LIMIT $5`
	rows, err := s.db.QueryContext(ctx, query, after, profile, bindings-imageBindings, imageBindings, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DocumentSummary
	for rows.Next() {
		var item DocumentSummary
		if err := rows.Scan(&item.NodeID, &item.ContentVersionID, &item.Name); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
