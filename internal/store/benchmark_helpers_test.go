package store

import (
	"context"
	"database/sql"
)

// bulkBenchmarkFileVersionsTx copies a file into revision-1 nodes with shared
// content. New node IDs follow lastNodeID; version IDs match the sequence.
func bulkBenchmarkFileVersionsTx(ctx context.Context, tx *sql.Tx, source Node, lastNodeID int64, count int) error {
	_, err := tx.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<$1)
		INSERT INTO nodes (id,parent_id,name,kind,current_version_id,revision,created_at,modified_at)
		SELECT $2+i,parent_id,printf('seed-%d.txt',i),kind,
		printf('00000000-0000-4000-8000-%012x',i),1,created_at,modified_at
		FROM seq CROSS JOIN nodes WHERE id=$3`, count, lastNodeID, source.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<$1)
		INSERT INTO content_versions (version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,
		introduced_operation_id,transition_kind,source_version_id)
		SELECT printf('00000000-0000-4000-8000-%012x',i),$2+i,blob_hash,size,mime_type,recorded_at,1,
		printf('00000000-0000-4000-9000-%012x',i),'content_create',NULL
		FROM seq CROSS JOIN content_versions WHERE version_id=$3`, count, lastNodeID, source.CurrentVersionID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE nodes SET revision=revision+$1 WHERE id=$2`, count, source.ParentID)
	return err
}
