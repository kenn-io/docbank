package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
)

var emailInventorySuppressionProfile = func() string {
	v := sha256.Sum256([]byte("docbank-email-inventory-suppression/v1"))
	return hex.EncodeToString(v[:])
}()

func emailInventorySuppressed(ctx context.Context, q metadataQuerier, v ContentVersion) (bool, error) {
	var yes bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM derivative_purge_suppressions WHERE source_sha256=? AND profile_fingerprint=? AND active=1)`, v.BlobHash, derivativeAttachmentSuppressionScope(v.ID, emailInventorySuppressionProfile)).Scan(&yes)
	return yes, err
}

// purgeEmailCatalogTx shares the existing derivative purge transaction. Only
// All/version selectors address email inventories; rendition IDs keep their
// original selector meaning.
func purgeEmailCatalogTx(ctx context.Context, tx *sql.Tx, request PurgeRequest, asOf string, report *PurgeReport) ([]derivativePurgeSuppression, []string, error) {
	var suppressions []derivativePurgeSuppression
	if request.All || len(request.ContentVersionIDs) > 0 {
		versions, err := loadProcessingMetadataIDs(ctx, tx, "email purge version", `SELECT version_id FROM content_versions ORDER BY version_id`)
		if err != nil {
			return nil, nil, err
		}
		selected := stringSet(request.ContentVersionIDs)
		for _, id := range versions {
			if _, ok := selected[id]; !request.All && !ok {
				continue
			}
			var retained bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM email_document_publications WHERE parent_version_id=?)`, id).Scan(&retained); err != nil {
				return nil, nil, err
			}
			if retained {
				return nil, nil, ErrEmailDocumentConflict
			}
			v, err := emailVersion(ctx, tx, id)
			if err != nil {
				return nil, nil, err
			}
			generationIDs, err := stringColumnTx(ctx, tx, "email generation", `SELECT generation_id FROM email_attachments WHERE content_version_id=? ORDER BY generation_id`, id)
			if err != nil {
				return nil, nil, err
			}
			if len(generationIDs) == 0 && !emailMIME(v.MimeType) {
				continue
			}
			if len(generationIDs) == 0 {
				suppressed, err := emailInventorySuppressed(ctx, tx, v)
				if err != nil {
					return nil, nil, err
				}
				if suppressed {
					continue
				}
				sum := sha256.Sum256([]byte("docbank-email-pending-generation/v1\x00" + v.BlobHash + "\x00" + v.ID))
				generationIDs = append(generationIDs, hex.EncodeToString(sum[:]))
			}
			for _, generationID := range generationIDs {
				suppressions = append(suppressions, derivativePurgeSuppression{sourceSHA256: v.BlobHash, profileFingerprint: derivativeAttachmentSuppressionScope(v.ID, emailInventorySuppressionProfile), buildID: generationID, purgedAt: asOf, active: true})
			}
			for _, entry := range []struct {
				query string
				count *int
			}{
				{`DELETE FROM email_heads WHERE content_version_id=?`, &report.RemovedEmailHeads},
				{`DELETE FROM email_attachments WHERE content_version_id=?`, &report.RemovedEmailAttachments},
			} {
				result, err := tx.ExecContext(ctx, entry.query, id)
				if err != nil {
					return nil, nil, err
				}
				count, err := rowsAffectedInt(result)
				if err != nil {
					return nil, nil, err
				}
				*entry.count += count
			}
		}
	}
	// Generations have no independent source-blob root: an orphan is collectible.
	ids, err := stringColumnTx(ctx, tx, "orphan email generation", `SELECT generation_id FROM email_generations g WHERE NOT EXISTS(SELECT 1 FROM email_attachments a WHERE a.generation_id=g.generation_id) ORDER BY generation_id`)
	if err != nil {
		return nil, nil, err
	}
	var blobs []string
	for _, id := range ids {
		hashes, err := stringColumnTx(ctx, tx, "email artifact", `SELECT blob_hash FROM email_part_artifacts WHERE generation_id=? ORDER BY blob_hash`, id)
		if err != nil {
			return nil, nil, err
		}
		blobs = append(blobs, hashes...)
		result, err := tx.ExecContext(ctx, `DELETE FROM email_part_artifacts WHERE generation_id=?`, id)
		if err != nil {
			return nil, nil, err
		}
		count, err := rowsAffectedInt(result)
		if err != nil {
			return nil, nil, err
		}
		report.RemovedEmailPartArtifacts += count
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_generations WHERE generation_id=?`, id); err != nil {
			return nil, nil, err
		}
		report.RemovedEmailGenerations++
	}
	return suppressions, blobs, nil
}

func deleteEmailAuthorityForVersionsTx(ctx context.Context, tx *sql.Tx, versions []string) error {
	for _, id := range versions {
		var retained bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM email_document_publications WHERE parent_version_id=?) OR EXISTS(SELECT 1 FROM email_document_relations WHERE child_version_id=?)`, id, id).Scan(&retained); err != nil {
			return err
		}
		if retained {
			return ErrEmailDocumentConflict
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_attachments WHERE content_version_id=?`, id); err != nil {
			return err
		}
	}
	return nil
}
