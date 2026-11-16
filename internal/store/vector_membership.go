package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
)

// SQLite's released triggers own its revisions. PostgreSQL writers advance
// only affected spaces, in the transaction that changes serving membership.
func invalidateVectorMembershipTx(ctx context.Context, tx *sql.Tx, postgres bool, spaces string, args ...any) error {
	if !postgres {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO vector_membership_epochs(vector_space_id,revision)
 SELECT DISTINCT vector_space_id,1 FROM (`+spaces+`) changed
 ORDER BY vector_space_id
 ON CONFLICT(vector_space_id) DO UPDATE SET revision=vector_membership_epochs.revision+1`, args...)
	if err != nil {
		return fmt.Errorf("invalidating vector membership: %w", err)
	}
	return nil
}

func invalidateNodeVectorMembershipTx(ctx context.Context, tx *sql.Tx, postgres bool, nodes []int64, imagesOnly bool) error {
	if !postgres || len(nodes) == 0 {
		return nil
	}
	encoded, err := json.Marshal(nodes)
	if err != nil {
		return err
	}
	return invalidateVectorMembershipTx(ctx, tx, true, `SELECT h.vector_space_id
 FROM embedding_heads h JOIN nodes n ON n.current_version_id=h.content_version_id
 WHERE n.id IN (SELECT value::bigint FROM jsonb_array_elements_text($1::jsonb))
 AND (NOT $2 OR h.input_kind='image')`, string(encoded), imagesOnly)
}

func invalidatePhotoVectorMembershipTx(ctx context.Context, tx *sql.Tx, postgres bool, before, after PhotoAsset) error {
	if !postgres {
		return nil
	}
	var oldNode, newNode int64
	if before.DisplayFileID != nil {
		if file, ok := photoFileByID(before.Files, *before.DisplayFileID); ok {
			oldNode = file.NodeID
		}
	}
	if after.DisplayFileID != nil {
		if file, ok := photoFileByID(after.Files, *after.DisplayFileID); ok {
			newNode = file.NodeID
		}
	}
	if oldNode == newNode && before.Kind == after.Kind && equalPhotoOptional(before.HiddenAt, after.HiddenAt) && equalPhotoOptional(before.ExcludedAt, after.ExcludedAt) {
		return nil
	}
	return invalidateNodeVectorMembershipTx(ctx, tx, true, []int64{oldNode, newNode}, true)
}

// Capture the affected spaces before deleting their last head. Cascades cannot
// recover those identities afterwards. Keep SQLite's statement and trigger owner.
func deleteEmbeddingHeadsTx(ctx context.Context, tx *sql.Tx, postgres bool, where string, args ...any) (sql.Result, error) {
	if err := invalidateVectorMembershipTx(ctx, tx, postgres, `SELECT vector_space_id FROM embedding_heads WHERE `+where, args...); err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, `DELETE FROM embedding_heads WHERE `+where, args...)
}

func deleteRenditionHeadsTx(ctx context.Context, tx *sql.Tx, postgres bool, where string, args ...any) (sql.Result, error) {
	if err := invalidateVectorMembershipTx(ctx, tx, postgres, `SELECT h.vector_space_id FROM embedding_heads h
 JOIN embedding_sets es ON es.embedding_set_id=h.embedding_set_id
 JOIN embedding_input_generations g ON g.generation_id=es.input_generation_id
 WHERE es.input_kind<>'image' AND g.attachment_id IN (SELECT attachment_id FROM rendition_heads WHERE `+where+`)`, args...); err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, `DELETE FROM rendition_heads WHERE `+where, args...)
}
