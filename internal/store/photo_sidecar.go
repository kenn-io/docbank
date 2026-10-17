package store

import (
	"context"
	"database/sql"
	"errors"
)

// photoSidecarIntendedTarget names the original that a sidecar's source path
// and SidecarForExtension claim pair it with, or "" when that is ambiguous. A
// sidecar Docbank generated keeps its target unless its packet names an
// extension; a sidecar without outside provenance pairs by vault names.
func photoSidecarIntendedTarget(ctx context.Context, q metadataQuerier, sidecar PhotoFile, node Node, extension string) (string, error) {
	if sidecar.Role != PhotoRoleSidecar || sidecar.SidecarOfID == nil {
		return "", ErrStaleRevision
	}
	path := node.Name
	var generated bool
	err := q.QueryRowContext(ctx, `SELECT p.original_path,i.source_kind='embedded:photo-xmp' FROM provenance p JOIN ingests i ON i.id=p.ingest_id
 WHERE p.node_id=? AND (i.source_kind NOT LIKE 'embedded:%' OR i.source_kind='embedded:photo-xmp') AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 ORDER BY i.started_at DESC,p.identity DESC LIMIT 1`, node.ID).Scan(&path, &generated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if generated && extension == "" {
		return *sidecar.SidecarOfID, nil
	}
	intended, bound, err := photoSidecarSourceTx(ctx, q, sidecar.AssetID, path, extension, generated || errors.Is(err, sql.ErrNoRows))
	if err != nil {
		return "", err
	}
	if !bound {
		return *sidecar.SidecarOfID, nil
	}
	return intended, nil
}
