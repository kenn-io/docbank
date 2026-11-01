package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/internal/query"
)

// Released SQLite triggers own these effects. PostgreSQL writers perform them
// in the same transaction as the authoritative mutation, never after commit.
func (s *Store) promoteContentBlobTx(ctx context.Context, tx *sql.Tx, hash string) error {
	if s.postgresSchema == "" {
		return nil
	}
	for _, statement := range []string{
		`DELETE FROM rendition_blob_staging WHERE blob_hash=$1`,
		`DELETE FROM derivative_blob_purge_pending WHERE blob_hash=$1`,
	} {
		if _, err := tx.ExecContext(ctx, statement, hash); err != nil {
			return err
		}
	}
	return nil
}

// refreshNodePhotoWorkTx includes unowned sidecars paired by name, and both
// parents of a moved sidecar. A node's own asset also changes when its revision
// or current version changes. Collect IDs before writing on the connection.
func (s *Store) refreshNodePhotoWorkTx(ctx context.Context, tx *sql.Tx, node Node, prior *Node) error {
	if s.postgresSchema == "" {
		return nil
	}
	candidates := []Node{node}
	if prior != nil {
		candidates = append(candidates, *prior)
	}
	assets := map[string]bool{}
	for _, candidate := range candidates {
		sibling := ""
		if stem, ok := strings.CutSuffix(candidate.Name, ".xmp"); ok {
			sibling = stem
		}
		if err := queryRows(ctx, tx, `SELECT DISTINCT f.asset_id FROM photo_files f JOIN nodes n ON n.id=f.node_id
   WHERE f.asset_id IS NOT NULL AND (n.id=$1 OR ($2<>'' AND n.parent_id=$3 AND n.name=$2))`,
			[]any{candidate.ID, sibling, candidate.ParentID}, func(rows *sql.Rows) error {
				var asset string
				if err := rows.Scan(&asset); err != nil {
					return err
				}
				assets[asset] = true
				return nil
			}); err != nil {
			return err
		}
	}
	for asset := range assets {
		if err := s.refreshPhotoAssetWorkTx(ctx, tx, asset); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) refreshPhotoAssetWorkTx(ctx context.Context, tx *sql.Tx, asset string) error {
	if s.postgresSchema == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM photo_xmp_unsettled WHERE asset_id=$1`, asset); err != nil {
		return fmt.Errorf("clearing photo export work: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO photo_xmp_unsettled(asset_id)
   SELECT $1 WHERE EXISTS(SELECT 1 FROM photo_xmp_inputs WHERE asset_id=$1 AND eligible AND (pending OR conflicts<>0))`, asset); err != nil {
		return fmt.Errorf("refreshing photo export work: %w", err)
	}
	return nil
}

// refreshPhotoTimelineTx applies the released timeline projection to current
// bytes, including claims bound to another retained version of the same bytes.
// Folder eligibility uses the same Go function registered with SQLite.
func (s *Store) refreshPhotoTimelineTx(ctx context.Context, tx *sql.Tx, nodeID int64) error {
	if s.postgresSchema == "" {
		return nil
	}
	var fileID string
	err := tx.QueryRowContext(ctx, `SELECT file_id FROM photo_files WHERE node_id=$1`, nodeID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var path, mtime, folder *string
	err = queryRows(ctx, tx, `WITH RECURSIVE chain(observed_at,identity) AS (
   SELECT b.observed_at,b.provenance_identity FROM nodes n
   JOIN content_versions v ON v.version_id=n.current_version_id
   JOIN content_versions cv ON cv.node_id=n.id AND cv.blob_hash=v.blob_hash
   JOIN provenance_version_bindings b ON b.content_version_id=cv.version_id WHERE n.id=$1
   UNION SELECT chain.observed_at,p.identity FROM chain JOIN provenance p ON p.supersedes=chain.identity
  ) SELECT c.original_path,c.original_mtime,i.source_kind,i.source_desc
  FROM chain JOIN provenance c ON c.identity=chain.identity JOIN ingests i ON i.id=c.ingest_id
  WHERE NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=c.identity)
  ORDER BY chain.observed_at,c.identity`, []any{nodeID}, func(rows *sql.Rows) error {
		var claimPath, kind, desc string
		var claimTime *string
		if err := rows.Scan(&claimPath, &claimTime, &kind, &desc); err != nil {
			return err
		}
		if folder == nil {
			if key, ok := query.PhotoFolderKey(kind, desc, claimPath); ok {
				path, folder = new(claimPath), new(key)
			}
		}
		if mtime == nil && claimTime != nil && !strings.HasPrefix(*claimTime, "0000") {
			mtime = claimTime
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO photo_source_claims(file_id,original_path,original_mtime,folder_key) VALUES($1,$2,$3,$4)
  ON CONFLICT(file_id) DO UPDATE SET original_path=excluded.original_path,original_mtime=excluded.original_mtime,folder_key=excluded.folder_key`, fileID, path, mtime, folder); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM photo_timeline WHERE file_id=$1`, fileID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO photo_timeline(file_id,sort_key,day,modified_time,folder_key)
  SELECT f.file_id,
   COALESCE(NULLIF(t.capture_sort_key,''),substr(c.original_mtime,1,19)||'.'||substr(
    CASE WHEN substr(c.original_mtime,20,1)='.' THEN substr(c.original_mtime,21,length(c.original_mtime)-21) ELSE '' END||'000000000',1,9)),
   COALESCE(NULLIF(t.capture_date,''),substr(c.original_mtime,1,10)),
   CASE WHEN NULLIF(t.capture_sort_key,'') IS NULL THEN c.original_mtime END,
   CASE WHEN NULLIF(t.capture_sort_key,'') IS NOT NULL AND t.capture_time_precision IN ('second','fraction')
    THEN CASE WHEN t.capture_time_timezone IN ('utc','offset') THEN 'z' ELSE 'f' END||c.folder_key END
  FROM photo_files f JOIN nodes n ON n.id=f.node_id
  JOIN content_versions v ON v.version_id=n.current_version_id
  JOIN photo_source_claims c ON c.file_id=f.file_id
  LEFT JOIN source_metadata_heads h ON h.source_sha256=v.blob_hash
  LEFT JOIN photo_technical_metadata t ON t.generation_id=h.generation_id
  WHERE f.file_id=$1 AND f.role<>'sidecar' AND (NULLIF(t.capture_sort_key,'') IS NOT NULL OR c.original_mtime IS NOT NULL)`, fileID)
	return err
}

func (s *Store) writeLooseDocumentLocationTx(ctx context.Context, tx *sql.Tx, hash string, physical BlobPhysical) error {
	packs, err := s.packSummaryKeysTx(ctx, tx, []string{hash})
	if err != nil {
		return err
	}
	if err := writeLooseLocationTx(ctx, tx, s.primaryStoreID, hash, physical); err != nil {
		return err
	}
	return s.refreshPackSummariesTx(ctx, tx, packs)
}
