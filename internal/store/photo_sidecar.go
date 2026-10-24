package store

import (
	"context"
	"database/sql"
	"errors"
)

// photoSidecarIntendedTarget names the original that a sidecar's source path
// and SidecarForExtension claim pair it with, or "" when that is ambiguous. A
// sidecar Docbank generated keeps its target unless its packet names an
// extension the attached original does not have; a sidecar without outside provenance pairs by vault names.
// Checkout observations use their saved pairing because their paths are relative.
func photoSidecarIntendedTarget(ctx context.Context, q metadataQuerier, sidecar PhotoFile, node Node, extension string) (string, error) {
	if sidecar.Role != PhotoRoleSidecar || sidecar.SidecarOfID == nil {
		return "", ErrStaleRevision
	}
	path := node.Name
	var kind, source string
	err := q.QueryRowContext(ctx, `SELECT p.original_path,i.source_kind,i.source_desc FROM provenance p JOIN ingests i ON i.id=p.ingest_id JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
 WHERE p.node_id=? AND b.content_version_id=? AND (i.source_kind NOT LIKE 'embedded:%' OR i.source_kind='embedded:photo-xmp') AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 ORDER BY b.observed_at DESC,p.identity DESC LIMIT 1`, node.ID, node.CurrentVersionID).Scan(&path, &kind, &source)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if kind == "photo-checkout" {
		var target, name string
		err := q.QueryRowContext(ctx, `SELECT e.sidecar_of_file_id,frozen.relative_path FROM photo_checkout_entries e
 JOIN photo_files f ON f.file_id=e.file_id JOIN photo_files original ON original.file_id=e.sidecar_of_file_id
 JOIN photo_checkout_entries frozen ON frozen.checkout_id=e.checkout_id AND frozen.file_id=e.sidecar_of_file_id
 WHERE e.checkout_id=? AND f.node_id=? AND e.sidecar_of_file_id=? AND f.sidecar_of_file_id=e.sidecar_of_file_id AND f.asset_id=? AND original.asset_id=f.asset_id`, source, node.ID, *sidecar.SidecarOfID, sidecar.AssetID).Scan(&target, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err == nil && !photoSidecarExtensionAgrees(name, extension) {
			return "", nil
		}
		return target, err
	}
	if kind == "embedded:photo-xmp" {
		if extension == "" {
			return *sidecar.SidecarOfID, nil
		}
		// The attachment is stable across renames, so the claim is checked against its current name.
		var name string
		err := q.QueryRowContext(ctx, `SELECT n.name FROM photo_files f JOIN nodes n ON n.id=f.node_id WHERE f.file_id=?`, *sidecar.SidecarOfID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !photoSidecarExtensionAgrees(name, extension) {
			return "", nil
		}
		return *sidecar.SidecarOfID, err
	}
	intended, bound, err := photoSidecarSourceTx(ctx, q, sidecar.AssetID, path, extension, errors.Is(err, sql.ErrNoRows))
	if err != nil {
		return "", err
	}
	if !bound {
		return *sidecar.SidecarOfID, nil
	}
	return intended, nil
}
