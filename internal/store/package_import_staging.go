package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
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
func discardUnreceiptedPackageFilesTx(ctx context.Context, tx *sql.Tx, pkg Package) error {
	rows, err := tx.QueryContext(ctx, `WITH retained(version_id) AS (
		SELECT content_version_id FROM package_import_receipts
		UNION
		SELECT json_extract(rep.value,'$.content_version_id')
		FROM package_import_receipts receipt,
			json_each(CAST(receipt.receipt_json AS TEXT),'$.member.representations') rep
	)
	SELECT version.node_id,version.version_id FROM content_versions version
	WHERE EXISTS (SELECT 1 FROM provenance p WHERE p.node_id=version.node_id AND p.ingest_id=?)
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
	if err := deleteRenditionAuthorityForVersionsTx(ctx, tx, versionIDs); err != nil {
		return err
	}
	encoded, err := json.Marshal(nodeIDs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tags SET revision=revision+1 WHERE id IN (
		SELECT tag_id FROM node_tags WHERE node_id IN (SELECT value FROM json_each(?)))`, string(encoded)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM nodes WHERE id IN (SELECT value FROM json_each(?))`, string(encoded))
	return err
}
