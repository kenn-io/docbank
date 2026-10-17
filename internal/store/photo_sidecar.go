package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// photoSidecarMemberRevisionsSQL snapshots each member's node revision and
// audit coverage. Enrolling a scope changes coverage without a revision, and an
// audited vault only accepts decisions for covered originals.
const photoSidecarMemberRevisionsSQL = `(SELECT group_concat(member_revision, ',') FROM
 (SELECT n.id || ':' || n.revision ||
 CASE WHEN EXISTS (SELECT 1 FROM audit_memberships m WHERE m.node_id=n.id) THEN ':audited' ELSE '' END AS member_revision
 FROM photo_files f JOIN nodes n ON n.id=f.node_id
 WHERE f.asset_id=a.asset_id ORDER BY n.id))`

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

// auditRefusesAttachmentChangeTx reports whether an audited vault refuses
// attachment changes to the node because no audit scope covers it.
func auditRefusesAttachmentChangeTx(ctx context.Context, tx *sql.Tx, nodeID int64) (bool, error) {
	var refused bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_authority WHERE singleton=1)
 AND NOT EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=?)`, nodeID).Scan(&refused)
	if err != nil {
		return false, fmt.Errorf("checking audit coverage for photo node %d: %w", nodeID, err)
	}
	return refused, nil
}
