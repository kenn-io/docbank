package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// deleteRenditionAuthorityForVersionsTx revokes version-specific serving
// authority before its owning content versions are permanently deleted.
// Immutable builds remain cataloged until ordinary derivative GC proves that
// no attachment or other root still needs them.
func deleteRenditionAuthorityForVersionsTx(
	ctx context.Context, tx *sql.Tx, versionIDs []string,
) (retErr error) {
	if len(versionIDs) == 0 {
		return nil
	}
	deleteHeads, err := tx.PrepareContext(ctx,
		`DELETE FROM rendition_heads WHERE content_version_id=?`)
	if err != nil {
		return fmt.Errorf("preparing rendition head deletion: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, deleteHeads.Close()) }()
	deleteAttachments, err := tx.PrepareContext(ctx,
		`DELETE FROM rendition_attachments WHERE content_version_id=?`)
	if err != nil {
		return fmt.Errorf("preparing rendition attachment deletion: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, deleteAttachments.Close()) }()
	for _, versionID := range versionIDs {
		if _, err := deleteHeads.ExecContext(ctx, versionID); err != nil {
			return fmt.Errorf("deleting rendition heads for content version %s: %w", versionID, err)
		}
		if _, err := deleteAttachments.ExecContext(ctx, versionID); err != nil {
			return fmt.Errorf("deleting rendition attachments for content version %s: %w", versionID, err)
		}
	}
	return nil
}
