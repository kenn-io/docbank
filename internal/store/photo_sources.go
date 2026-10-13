package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"path"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

type photoSource struct {
	identity, path, asset, target string
	trashed                       bool
}

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

func photoSourcesTx(ctx context.Context, tx *sql.Tx, assets string, checkout bool) (map[string]photoSource, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(f.asset_id,CAST(f.node_id AS TEXT)),f.file_id,COALESCE(p.identity,''),COALESCE(p.ingest_id,''),COALESCE(p.original_path,n.name),
 COALESCE((SELECT MIN(b.observed_at) FROM provenance_version_bindings b WHERE b.provenance_identity=p.identity),i.started_at,'' ) AS observed,
 (SELECT COUNT(*) FROM photo_files member WHERE member.asset_id=f.asset_id OR member.node_id=f.node_id),COALESCE(f.source_provenance_identity,''),COALESCE(f.sidecar_of_file_id,''),n.trashed_at IS NOT NULL
 FROM photo_files f JOIN nodes n ON n.id=f.node_id LEFT JOIN provenance p ON p.node_id=f.node_id
 AND EXISTS(SELECT 1 FROM ingests origin WHERE origin.id=p.ingest_id AND origin.source_kind NOT LIKE 'embedded:%')
 AND (p.identity=f.source_provenance_identity OR NOT EXISTS(SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity))
 LEFT JOIN ingests i ON i.id=p.ingest_id WHERE (?='' OR f.asset_id IN (SELECT value FROM json_each(?))) AND (? OR EXISTS(SELECT 1 FROM photo_files missing WHERE missing.source_provenance_identity IS NULL AND (missing.asset_id=f.asset_id OR missing.node_id=f.node_id)))
 ORDER BY observed,i.started_at,p.ingest_id,p.original_path,p.identity`, assets, assets, checkout)
	if err != nil {
		return nil, fmt.Errorf("loading photo sources: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type group struct {
		asset, key, observed string
		members, agreement   int
		refs                 map[string]photoSource
	}
	groups := map[string]*group{}
	sources := map[string]photoSource{}
	for rows.Next() {
		var source photoSource
		var file, ingest, observed, fixed string
		var members int
		if err := rows.Scan(&source.asset, &file, &source.identity, &ingest, &source.path, &observed, &members, &fixed, &source.target, &source.trashed); err != nil {
			return nil, err
		}
		source.path = normalizePhotoSourcePath(source.path)
		if _, exists := sources[file]; !exists || source.identity == fixed {
			sources[file] = source
		}
		if source.identity == "" {
			continue
		}
		folder, stem := photoImportSourceKey(source.path)
		key := source.asset + "\x00" + ingest + "\x00" + folder + "\x00" + stem
		g := groups[key]
		if g == nil {
			g = &group{asset: source.asset, key: key, members: members, refs: map[string]photoSource{}}
			groups[key] = g
		}
		if _, exists := g.refs[file]; !exists {
			g.refs[file] = source
			if source.identity == fixed {
				g.agreement++
			}
			if observed > g.observed {
				g.observed = observed
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		return a.agreement > b.agreement || a.agreement == b.agreement && (a.observed < b.observed || a.observed == b.observed && a.key < b.key)
	})
	complete := map[string]bool{}
	byFile := map[string][]*group{}
	for _, g := range ordered {
		for file := range g.refs {
			byFile[file] = append(byFile[file], g)
		}
		if len(g.refs) == g.members && !complete[g.asset] {
			complete[g.asset] = true
			maps.Copy(sources, g.refs)
		}
	}
	if checkout {
		companions := map[string][]string{}
		for file, source := range sources {
			if source.target != "" && !source.trashed {
				companions[source.target] = append(companions[source.target], file)
			}
		}
		for target, files := range companions {
			sort.Strings(files)
			candidates := []photoSource{sources[target]}
			for _, g := range byFile[target] {
				candidates = append(candidates, g.refs[target])
			}
			resolved := false
			for _, candidate := range candidates {
				choices := map[string]photoSource{}
				for _, file := range files {
					source := sources[file]
					if !photoSourcePairMatches(source.path, candidate.path) {
						for _, g := range byFile[file] {
							observed := g.refs[target]
							if observed.identity != "" && strings.EqualFold(norm.NFC.String(path.Base(observed.path)), norm.NFC.String(path.Base(candidate.path))) && photoSourcePairMatches(g.refs[file].path, candidate.path) {
								source = g.refs[file]
								break
							}
						}
					}
					if photoSourcePairMatches(source.path, candidate.path) {
						choices[file] = source
					}
				}
				if len(choices) == len(files) {
					sources[target] = candidate
					maps.Copy(sources, choices)
					resolved = true
					break
				}
			}
			if !resolved {
				return nil, fmt.Errorf("%w: incompatible source names for file %s and target %s in asset %s", ErrInvalidPhotoCheckout, files[0], target, sources[target].asset)
			}
		}
	}
	return sources, nil
}

func photoSourcePairMatches(sidecar, target string) bool {
	name := norm.NFC.String(strings.TrimSuffix(path.Base(sidecar), path.Ext(sidecar)))
	base := norm.NFC.String(path.Base(target))
	return strings.EqualFold(name, base) || strings.EqualFold(name, strings.TrimSuffix(base, path.Ext(base)))
}

func normalizePhotoSourcePath(path string) string {
	windowsDrive := len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && (path[2] == '\\' || path[2] == '/')
	if windowsDrive || strings.HasPrefix(path, `\\`) {
		return strings.ReplaceAll(path, "\\", "/")
	}
	return path
}
