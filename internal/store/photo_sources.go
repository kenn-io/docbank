package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type photoSource struct {
	identity, path, asset, target string
	eligible                      bool
}

const photoSourceObservedSQL = `COALESCE((SELECT MIN(b.observed_at) FROM provenance_version_bindings b WHERE b.provenance_identity=p.identity),i.started_at,'')`

func backfillPhotoSourcesTx(ctx context.Context, tx *sql.Tx) error {
	sources, err := photoSourcesTx(ctx, tx, "", false)
	if err != nil {
		return err
	}
	for file, source := range sources {
		if source.identity != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET source_provenance_identity=? WHERE file_id=? AND source_provenance_identity IS NULL`, source.identity, file); err != nil {
				return fmt.Errorf("backfilling photo source: %w", err)
			}
		}
	}
	return nil
}

func photoSourcesTx(ctx context.Context, tx metadataQuerier, assets string, checkout bool) (map[string]photoSource, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(f.asset_id,CAST(f.node_id AS TEXT)),f.file_id,COALESCE(p.identity,''),COALESCE(p.original_path,n.name),
 COALESCE(f.sidecar_of_file_id,''),EXISTS(SELECT 1 FROM photo_assets a WHERE a.asset_id=f.asset_id AND a.hidden_at IS NULL AND a.excluded_at IS NULL) AND n.trashed_at IS NULL
 FROM photo_files f JOIN nodes n ON n.id=f.node_id LEFT JOIN provenance p ON p.node_id=f.node_id
 AND ((? AND p.identity=f.source_provenance_identity) OR (NOT ?
 AND EXISTS(SELECT 1 FROM ingests origin WHERE origin.id=p.ingest_id AND origin.source_kind NOT LIKE 'embedded:%')
 AND NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)))
 LEFT JOIN ingests i ON i.id=p.ingest_id WHERE (?='' OR f.asset_id IN (SELECT value FROM json_each(?))) AND (? OR EXISTS(SELECT 1 FROM photo_files missing WHERE missing.source_provenance_identity IS NULL AND (missing.asset_id=f.asset_id OR missing.node_id=f.node_id)))
 ORDER BY `+photoSourceObservedSQL+`,i.started_at,p.ingest_id,p.original_path,p.identity`, checkout, checkout, assets, assets, checkout)
	if err != nil {
		return nil, fmt.Errorf("loading photo sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sources := map[string]photoSource{}
	for rows.Next() {
		var source photoSource
		var file string
		if err := rows.Scan(&source.asset, &file, &source.identity, &source.path, &source.target, &source.eligible); err != nil {
			return nil, err
		}
		if source.identity != "" {
			source.path = normalizePhotoSourcePath(source.path)
		}
		if _, exists := sources[file]; !exists {
			sources[file] = source
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return sources, nil
}

func normalizePhotoSourcePath(path string) string {
	windowsDrive := len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
	if windowsDrive || strings.HasPrefix(path, `\\`) {
		return normalizePhotoWindowsSourcePath(path, windowsDrive)
	}
	return path
}

func normalizePhotoWindowsSourcePath(path string, drive bool) string {
	if drive {
		path = strings.ToUpper(path[:1]) + path[1:]
	}
	return strings.ReplaceAll(path, "\\", "/")
}
