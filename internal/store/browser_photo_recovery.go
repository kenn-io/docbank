package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"
)

// BrowserPhotoImport describes recorded members of one browser selection.
type BrowserPhotoImport struct {
	SourceID        string            `json:"source_id"`
	Folder          string            `json:"folder"`
	Groups          int               `json:"groups"`
	FolderSelection bool              `json:"folder_selection"`
	Members         map[string]string `json:"-"`
}

// BrowserPhotoImportMember describes one current browser provenance path.
type BrowserPhotoImportMember struct {
	SourceID        string
	Folder          string
	FolderSelection bool
	RelativePath    string
	BlobHash        string
}

// BrowserPhotoImport finds a browser source from its committed provenance.
func (s *Store) BrowserPhotoImport(ctx context.Context, sourceID string) (BrowserPhotoImport, bool, error) {
	pathMatch, wildcard := "LIKE", "%"
	if s.postgresSchema == "" {
		pathMatch, wildcard = "GLOB", "*"
	}
	pattern := "browser:" + wildcard + "/" + wildcard
	if sourceID != "" {
		if _, err := uuid.Parse(sourceID); err != nil {
			return BrowserPhotoImport{}, false, nil
		}
		pattern = "browser:" + sourceID + "/" + wildcard
	}
	var original, desc string
	err := s.db.QueryRowContext(ctx, `SELECT p.original_path,i.source_desc
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id JOIN nodes n ON n.id=p.node_id
		WHERE i.source_kind=$1 AND i.source_desc LIKE 'browser: %' AND p.original_path `+pathMatch+` $2
		AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
		ORDER BY i.started_at DESC,p.original_path DESC LIMIT 1`, storedString(s.postgresSchema != "", "photo-import"), pattern).Scan(&original, &desc)
	if errors.Is(err, sql.ErrNoRows) {
		return BrowserPhotoImport{}, false, nil
	}
	if err != nil {
		return BrowserPhotoImport{}, false, fmt.Errorf("finding browser photo import: %w", err)
	}
	id, _, ok := strings.Cut(strings.TrimPrefix(original, "browser:"), "/")
	if _, err := uuid.Parse(id); !ok || err != nil {
		return BrowserPhotoImport{}, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.original_path,cv.blob_hash
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id JOIN nodes n ON n.id=p.node_id
		JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
		JOIN content_versions cv ON cv.version_id=b.content_version_id
		WHERE i.source_kind=$1 AND p.original_path `+pathMatch+` $2
		AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)`, storedString(s.postgresSchema != "", "photo-import"), "browser:"+id+"/"+wildcard)
	if err != nil {
		return BrowserPhotoImport{}, false, fmt.Errorf("reading browser photo import: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := BrowserPhotoImport{SourceID: id, Folder: strings.TrimPrefix(desc, "browser: "), Members: map[string]string{}}
	groups := map[string]bool{}
	for rows.Next() {
		var path, hash string
		if err := rows.Scan(&path, &hash); err != nil {
			return BrowserPhotoImport{}, false, err
		}
		relative := strings.TrimPrefix(path, "browser:"+id+"/")
		if relative == "" {
			continue
		}
		if old, exists := result.Members[relative]; exists && old != hash {
			result.Members[relative] = ""
		} else if !exists {
			result.Members[relative] = hash
		}
		result.FolderSelection = result.FolderSelection || strings.Contains(relative, "/")
		groups[PhotoImportGroupKey(relative, ClassifyPhotoSource(relative).Kind)] = true
	}
	if err := rows.Err(); err != nil {
		return BrowserPhotoImport{}, false, err
	}
	result.Groups = len(groups)
	return result, result.Groups > 0, nil
}

// BrowserPhotoImportPaths finds recorded members by their path beneath a
// selected folder. The returned hashes let callers confirm the source before
// continuing it.
func (s *Store) BrowserPhotoImportPaths(
	ctx context.Context, relativePaths []string,
) ([]BrowserPhotoImportMember, error) {
	patterns := make([]string, 0, len(relativePaths))
	seen := make(map[string]bool, len(relativePaths))
	for _, relativePath := range relativePaths {
		if relativePath == "" {
			continue
		}
		pattern := "browser:%/" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(relativePath)
		if s.postgresSchema == "" {
			pattern = "browser:*/" + strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]").Replace(relativePath)
		}
		if !seen[pattern] {
			patterns = append(patterns, pattern)
			seen[pattern] = true
		}
	}
	if len(patterns) == 0 {
		return nil, nil
	}
	slices.Sort(patterns)
	encoded, err := json.Marshal(patterns)
	if err != nil {
		return nil, err
	}
	pathMatch := "p.original_path LIKE convert_to(paths.value,'UTF8') ESCAPE decode('5c','hex')"
	if s.postgresSchema == "" {
		pathMatch = "p.original_path GLOB paths.value"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.original_path,i.source_desc,cv.blob_hash
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id JOIN nodes n ON n.id=p.node_id
		JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
		JOIN content_versions cv ON cv.version_id=b.content_version_id
		WHERE i.source_kind='photo-import' AND i.source_desc LIKE 'browser: %'
		AND EXISTS (SELECT 1 FROM (`+s.jsonArrayRowsSQL()+`) paths WHERE `+pathMatch+`)
		AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)`, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("finding browser photo import paths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var members []BrowserPhotoImportMember
	for rows.Next() {
		var original, description, hash string
		if err := rows.Scan(&original, &description, &hash); err != nil {
			return nil, err
		}
		id, relativePath, ok := strings.Cut(strings.TrimPrefix(original, "browser:"), "/")
		if _, err := uuid.Parse(id); !ok || err != nil {
			continue
		}
		folder := strings.TrimPrefix(description, "browser: ")
		members = append(members, BrowserPhotoImportMember{
			SourceID: id, Folder: folder, FolderSelection: folder != "Choose files",
			RelativePath: relativePath, BlobHash: hash,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return members, nil
}
