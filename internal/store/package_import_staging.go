package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"slices"
)

// IngestPackageFileWithReceipt fences staging in the transaction that creates
// the document, so cancellation cannot be followed by another staged file.
func (s *Store) IngestPackageFileWithReceipt(
	ctx context.Context, job PackageImportJob, run IngestRun, parentID int64,
	name, blobHash string, size int64, mimeType, originalPath string, physical BlobPhysical,
) (ContentWriteReceipt, error) {
	var receipt ContentWriteReceipt
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		held, err := packageImportLeaseHeld(ctx, tx, job.ID, job.Epoch, job.Token)
		if err != nil {
			return err
		}
		pkg, err := loadPackageTx(ctx, tx, held.PackageID)
		if err != nil {
			return err
		}
		if pkg.PackageID != job.PackageID || pkg.IngestID != run.ID() || pkg.State != "importing" {
			return ErrPackageConflict
		}
		receipt, _, _, err = s.ingestFileTx(ctx, tx, run, parentID, name, blobHash,
			size, mimeType, originalPath, "", ingestFileOptions{exact: true, completeReceipt: true}, physical)
		return err
	})
	return receipt, err
}

// discardUnreceiptedPackageFilesTx removes document authority, leaving shared
// bytes to ordinary garbage collection. A receipt retains every representation,
// including supplied text and pages that are not its primary content version.
func (s *Store) discardUnreceiptedPackageFilesTx(ctx context.Context, tx *sql.Tx, pkg Package) error {
	representations := `SELECT json_extract(rep.value,'$.content_version_id')
		FROM package_import_receipts receipt,
			json_each(CAST(receipt.receipt_json AS TEXT),'$.member.representations') rep`
	if s.postgresSchema != "" {
		array := `convert_from(receipt.receipt_json,'UTF8')::jsonb #> '{member,representations}'`
		representations = `SELECT rep.value ->> 'content_version_id' FROM package_import_receipts receipt,
			jsonb_array_elements(CASE WHEN jsonb_typeof(` + array + `)='array' THEN ` + array + ` ELSE '[]'::jsonb END) rep(value)`
	}
	rows, err := tx.QueryContext(ctx, `WITH retained(version_id) AS (
		SELECT content_version_id FROM package_import_receipts
		UNION
		SELECT e.version_id FROM photo_checkout_entries e JOIN photo_checkouts c ON c.checkout_id=e.checkout_id WHERE (`+photoCheckoutHoldPredicate+`)
		UNION
		`+representations+`
	)
	SELECT version.node_id,version.version_id FROM content_versions version
	WHERE EXISTS (SELECT 1 FROM provenance p WHERE p.node_id=version.node_id AND p.ingest_id=$1)
	  AND NOT EXISTS (SELECT 1 FROM content_versions kept JOIN retained ON retained.version_id=kept.version_id
		WHERE kept.node_id=version.node_id)
	ORDER BY version.node_id,version.version_id`, pkg.IngestID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var nodeIDs []int64
	var versionIDs []string
	for rows.Next() {
		var nodeID int64
		var versionID string
		if err := rows.Scan(&nodeID, &versionID); err != nil {
			return err
		}
		nodeIDs = append(nodeIDs, nodeID)
		versionIDs = append(versionIDs, versionID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	if err := s.deleteRenditionAuthorityForVersionsTx(ctx, tx, versionIDs); err != nil {
		return err
	}
	nodeIDs = slices.Compact(nodeIDs)
	if err := recordVaultChangesTx(ctx, tx, "removed", nodeIDs...); err != nil {
		return err
	}
	if err := s.adjustPhotosForPurgedNodesTx(ctx, tx, nodeIDs); err != nil {
		return err
	}
	encoded, err := json.Marshal(nodeIDs)
	if err != nil {
		return err
	}
	selection := `SELECT CAST(value AS BIGINT) FROM ` + (metadataJSONDialect{postgres: s.postgresSchema != ""}).arrayValues("$1")
	if _, err := tx.ExecContext(ctx, `WITH doomed(id) AS (`+selection+`)
		UPDATE tags SET revision=revision+1, assignment_count=assignment_count-gone.n
		FROM (SELECT nt.tag_id, COUNT(*) AS n FROM doomed d JOIN node_tags nt ON nt.node_id=d.id
		GROUP BY nt.tag_id) gone WHERE tags.id=gone.tag_id`, string(encoded)); err != nil {
		return err
	}
	var pairedAssets []string
	if s.postgresSchema != "" {
		if err := queryRows(ctx, tx, `SELECT DISTINCT f.asset_id FROM nodes sidecar
			JOIN nodes original ON original.parent_id=sidecar.parent_id AND original.name=left(sidecar.name,-4)
			JOIN photo_files f ON f.node_id=original.id
			WHERE sidecar.id IN (`+selection+`) AND right(sidecar.name,4)='.xmp' AND f.asset_id IS NOT NULL`,
			[]any{string(encoded)}, func(rows *sql.Rows) error {
				var asset string
				if err := rows.Scan(&asset); err != nil {
					return err
				}
				pairedAssets = append(pairedAssets, asset)
				return nil
			}); err != nil {
			return err
		}
	}
	if err := s.beforeContentVersionDeletionTx(ctx, tx, versionIDs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id IN (`+selection+`)`, string(encoded)); err != nil {
		return err
	}
	for _, asset := range pairedAssets {
		if err := s.refreshPhotoAssetWorkTx(ctx, tx, asset); err != nil {
			return err
		}
	}
	return nil
}
