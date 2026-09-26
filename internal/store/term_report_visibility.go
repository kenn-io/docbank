package store

import (
	"context"
	"encoding/json/v2"

	"go.kenn.io/docbank/report"
)

// CheckTermReportVisibility verifies that every exact captured version is still
// retained under a visible file node. A newer head is allowed: it does not
// change the old observation or authorize a recount from current content.
func (s *Store) CheckTermReportVisibility(ctx context.Context, frame report.Frame) error {
	identities, err := report.VisibilityIdentities(frame)
	if err != nil {
		return err
	}
	if len(identities) == 0 {
		return nil
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return err
	}
	var visible int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM json_each(?) selected
		JOIN nodes n ON n.id=json_extract(selected.value,'$.node_id')
			AND n.kind='file' AND n.trashed_at IS NULL
		JOIN content_versions cv ON cv.node_id=n.id
			AND cv.version_id=json_extract(selected.value,'$.version_id')
			AND cv.blob_hash=json_extract(selected.value,'$.sha256')`, string(encoded)).Scan(&visible)
	if err != nil {
		return err
	}
	if visible != len(identities) {
		return report.ErrVisibilityChanged
	}
	return nil
}
