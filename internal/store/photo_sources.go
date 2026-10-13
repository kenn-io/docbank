package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Backfill chooses one complete source group before removable bindings can affect names.
func backfillPhotoSourcesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(f.asset_id,CAST(f.node_id AS TEXT)),f.file_id,p.identity,p.ingest_id,p.original_path,
 COALESCE((SELECT MIN(b.observed_at) FROM provenance_version_bindings b WHERE b.provenance_identity=p.identity),i.started_at),
 (SELECT COUNT(*) FROM photo_files member WHERE member.asset_id=f.asset_id OR member.node_id=f.node_id),f.source_provenance_identity
 FROM photo_files f JOIN provenance p ON p.node_id=f.node_id JOIN ingests i ON i.id=p.ingest_id
 WHERE i.source_kind NOT LIKE 'embedded:%' AND NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 AND EXISTS(SELECT 1 FROM photo_files missing WHERE missing.source_provenance_identity IS NULL AND (missing.asset_id=f.asset_id OR missing.node_id=f.node_id))
 ORDER BY p.identity`)
	if err != nil {
		return fmt.Errorf("loading legacy photo sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type group struct {
		asset, key, observed string
		members              int
		refs                 map[string]string
	}
	groups := map[string]*group{}
	for rows.Next() {
		var asset, file, identity, ingest, path, observed string
		var members int
		var fixed *string
		if err := rows.Scan(&asset, &file, &identity, &ingest, &path, &observed, &members, &fixed); err != nil {
			return fmt.Errorf("reading legacy photo source: %w", err)
		}
		if fixed != nil && *fixed != identity {
			continue
		}
		folder, stem := photoImportSourceKey(normalizePhotoSourcePath(path))
		key := ingest + "\x00" + folder + "\x00" + stem
		id := asset + "\x00" + key
		g := groups[id]
		if g == nil {
			g = &group{asset: asset, key: key, members: members, refs: map[string]string{}}
			groups[id] = g
		}
		if _, exists := g.refs[file]; !exists {
			g.refs[file] = identity
			if observed > g.observed {
				g.observed = observed
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading legacy photo sources: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	chosen := map[string]*group{}
	for _, g := range groups {
		if len(g.refs) != g.members {
			continue
		}
		prior := chosen[g.asset]
		if prior == nil || g.observed < prior.observed || g.observed == prior.observed && g.key < prior.key {
			chosen[g.asset] = g
		}
	}
	for _, g := range chosen {
		for file, identity := range g.refs {
			if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET source_provenance_identity=? WHERE file_id=? AND source_provenance_identity IS NULL`, identity, file); err != nil {
				return fmt.Errorf("backfilling photo source: %w", err)
			}
		}
	}
	return nil
}

func normalizePhotoSourcePath(path string) string {
	windowsDrive := len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
	if windowsDrive || strings.HasPrefix(path, `\\`) {
		return strings.ReplaceAll(path, "\\", "/")
	}
	return path
}
