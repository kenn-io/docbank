package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Node is a row of the virtual tree. IDs are canonical; paths are display.
type Node struct {
	ID         int64
	ParentID   *int64
	Name       string
	Kind       string // "dir" | "file"
	BlobHash   string
	Size       int64
	MimeType   string
	Revision   int64
	CreatedAt  string
	ModifiedAt string
	TrashedAt  *string
}

// IsDir reports whether the node is a directory.
func (n Node) IsDir() bool { return n.Kind == "dir" }

const nodeCols = `id, parent_id, name, kind,
	COALESCE(blob_hash, ''), COALESCE(size, 0), COALESCE(mime_type, ''),
	revision, created_at, modified_at, trashed_at`

func scanNode(row interface{ Scan(args ...any) error }) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.ParentID, &n.Name, &n.Kind,
		&n.BlobHash, &n.Size, &n.MimeType,
		&n.Revision, &n.CreatedAt, &n.ModifiedAt, &n.TrashedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("scanning node: %w", err)
	}
	return n, nil
}

// NodeByID returns the node with the given id, live or trashed.
func (s *Store) NodeByID(ctx context.Context, id int64) (Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id)
	n, err := scanNode(row)
	if err != nil {
		return Node{}, fmt.Errorf("node %d: %w", id, err)
	}
	return n, nil
}

// splitPath turns "/a/b/" into ["a","b"]. "" and "/" yield nil (the root).
func splitPath(path string) []string {
	var segs []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs
}

// NodeByPath walks live nodes from the root along the given /-separated path.
func (s *Store) NodeByPath(ctx context.Context, path string) (Node, error) {
	n, err := s.NodeByID(ctx, s.rootID)
	if err != nil {
		return Node{}, err
	}
	for _, seg := range splitPath(path) {
		seg, err := NormalizeName(seg)
		if err != nil {
			return Node{}, fmt.Errorf("path %q: %w", path, err)
		}
		row := s.db.QueryRowContext(ctx,
			`SELECT `+nodeCols+` FROM nodes
			 WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`, n.ID, seg)
		n, err = scanNode(row)
		if err != nil {
			return Node{}, fmt.Errorf("path %q: %w", path, err)
		}
	}
	return n, nil
}

// Children lists the live children of a directory, dirs first, name-sorted.
func (s *Store) Children(ctx context.Context, dirID int64) ([]Node, error) {
	dir, err := s.NodeByID(ctx, dirID)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("node %d: %w", dirID, ErrNotDir)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeCols+` FROM nodes
		 WHERE parent_id = ? AND trashed_at IS NULL
		 ORDER BY kind = 'file', name`, dirID)
	if err != nil {
		return nil, fmt.Errorf("listing children of %d: %w", dirID, err)
	}
	defer func() { _ = rows.Close() }()

	var kids []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		kids = append(kids, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing children of %d: %w", dirID, err)
	}
	return kids, nil
}

// Path returns the display path of a node ("/" for the root).
func (s *Store) Path(ctx context.Context, id int64) (string, error) {
	if _, err := s.NodeByID(ctx, id); err != nil {
		return "", fmt.Errorf("computing path of node %d: %w", id, err)
	}
	var path string
	err := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE ancestry(id, parent_id, name, depth) AS (
			SELECT id, parent_id, name, 0 FROM nodes WHERE id = ?
			UNION ALL
			SELECT n.id, n.parent_id, n.name, a.depth + 1
			FROM nodes n JOIN ancestry a ON n.id = a.parent_id
		)
		SELECT COALESCE('/' || GROUP_CONCAT(name, '/'), '/')
		FROM (SELECT name FROM ancestry WHERE parent_id IS NOT NULL ORDER BY depth DESC)`,
		id).Scan(&path)
	if err != nil {
		return "", fmt.Errorf("computing path of node %d: %w", id, err)
	}
	if path == "/" || path == "" {
		return "/", nil
	}
	return path, nil
}
