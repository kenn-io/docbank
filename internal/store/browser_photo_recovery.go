package store

import (
	"context"
	"fmt"
	"strings"
	"uuid"
)

// BrowserPhotoImport describes recorded members of one browser selection.
type BrowserPhotoImport struct {
	SourceID  string            `json:"source_id"`
	Folder    string            `json:"folder"`
	StartedAt string            `json:"started_at"`
	Groups    int               `json:"groups"`
	Members   map[string]string `json:"-"`
}

// BrowserPhotoImport finds a browser source from its committed provenance.
func (s *Store) BrowserPhotoImport(ctx context.Context, sourceID string) (*BrowserPhotoImport, error) {
	imports, err := s.BrowserPhotoImports(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range imports {
		if item.SourceID == sourceID || sourceID == "" && item.Folder != "Choose files" {
			return item, nil
		}
	}
	return nil, nil
}

// BrowserPhotoImports returns committed browser sources, newest first.
func (s *Store) BrowserPhotoImports(ctx context.Context) ([]*BrowserPhotoImport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.started_at,i.source_desc,p.original_path,cv.blob_hash,live.blob_hash
		FROM ingests i JOIN provenance p ON p.ingest_id=i.id
		JOIN nodes n ON n.id=p.node_id AND n.trashed_at IS NULL
		JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
		JOIN content_versions cv ON cv.version_id=b.content_version_id
		JOIN content_versions live ON live.version_id=n.current_version_id
		WHERE i.source_kind=$1 AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
		ORDER BY i.started_at DESC,p.original_path`, storedString(s.postgresSchema != "", "photo-import"))
	if err != nil {
		return nil, fmt.Errorf("reading browser photo imports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var imports []*BrowserPhotoImport
	byID := map[string]*BrowserPhotoImport{}
	groups := map[string]map[string]bool{}
	for rows.Next() {
		var started, desc, original, hash, current string
		if err := rows.Scan(&started, &desc, &original, &hash, &current); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(original, "browser:") || !strings.HasPrefix(desc, "browser: ") {
			continue
		}
		id, relative, ok := strings.Cut(strings.TrimPrefix(original, "browser:"), "/")
		if !ok || relative == "" {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		result := byID[id]
		if result == nil {
			result = &BrowserPhotoImport{SourceID: id, Folder: strings.TrimPrefix(desc, "browser: "), StartedAt: started, Members: map[string]string{}}
			byID[id] = result
			groups[id] = map[string]bool{}
			imports = append(imports, result)
		}
		if old, exists := result.Members[relative]; hash != current || exists && old != hash {
			result.Members[relative] = ""
		} else if !exists {
			result.Members[relative] = hash
		}
		kind := ClassifyPhotoSource(relative).Kind
		groups[id][PhotoImportGroupKey(relative, kind)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, result := range imports {
		result.Groups = len(groups[result.SourceID])
	}
	return imports, nil
}
