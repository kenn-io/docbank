package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const nodeKindDir = "dir"

// Node is a row of the virtual tree. IDs are canonical; paths are display.
type Node struct {
	ID               int64
	ParentID         *int64
	Name             string
	Kind             string // "dir" | "file"
	CurrentVersionID string
	BlobHash         string
	Size             int64
	MimeType         string
	Revision         int64
	CreatedAt        string
	ModifiedAt       string
	TrashedAt        *string
}

// IsDir reports whether the node is a directory.
func (n Node) IsDir() bool { return n.Kind == nodeKindDir }

const nodeFrom = `nodes AS n
	LEFT JOIN content_versions AS cv
		ON cv.node_id = n.id AND cv.version_id = n.current_version_id`

const nodeCols = `n.id, n.parent_id, n.name, n.kind,
	COALESCE(n.current_version_id, ''), COALESCE(cv.blob_hash, ''),
	COALESCE(cv.size, 0), COALESCE(cv.mime_type, ''),
	n.revision, n.created_at, n.modified_at, n.trashed_at`

func scanNode(row interface{ Scan(args ...any) error }) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.ParentID, &n.Name, &n.Kind,
		&n.CurrentVersionID, &n.BlobHash, &n.Size, &n.MimeType,
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
		`SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id = ?`, id)
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

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, so path resolution
// can run standalone or inside a mutation's transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NodeByPath walks live nodes from the root along the given /-separated path.
func (s *Store) NodeByPath(ctx context.Context, path string) (Node, error) {
	return nodeByPath(ctx, s.db, s.rootID, path)
}

func nodeByPath(ctx context.Context, q rowQuerier, rootID int64, path string) (Node, error) {
	row := q.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM `+nodeFrom+` WHERE n.id = ?`, rootID)
	n, err := scanNode(row)
	if err != nil {
		return Node{}, fmt.Errorf("node %d: %w", rootID, err)
	}
	for _, seg := range splitPath(path) {
		seg, err := NormalizeName(seg)
		if err != nil {
			return Node{}, fmt.Errorf("path %q: %w", path, err)
		}
		row := q.QueryRowContext(ctx,
			`SELECT `+nodeCols+` FROM `+nodeFrom+`
			 WHERE n.parent_id = ? AND n.name = ? AND n.trashed_at IS NULL`, n.ID, seg)
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
		`SELECT `+nodeCols+` FROM `+nodeFrom+`
		 WHERE n.parent_id = ? AND n.trashed_at IS NULL
		 ORDER BY n.kind = 'file', n.name`, dirID)
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

// ChildrenPage lists one bounded page of a directory's live children, dirs
// first and name-sorted, and returns the complete child count. Target kind,
// total, and page come from one statement so callers never observe a mixture
// across concurrent tree mutations.
func (s *Store) ChildrenPage(
	ctx context.Context, dirID int64, limit, offset int,
) ([]Node, int, error) {
	if limit < 1 || limit > 5000 {
		return nil, 0, errors.New("children limit must be between 1 and 5000")
	}
	if offset < 0 {
		return nil, 0, errors.New("children offset must not be negative")
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH target AS (
		  SELECT kind FROM nodes WHERE id = ?
		), totals AS (
		  SELECT COUNT(*) AS total
		  FROM nodes
		  WHERE parent_id = ? AND trashed_at IS NULL
		), page AS (
		  SELECT n.id, n.parent_id, n.name, n.kind,
		         COALESCE(n.current_version_id, '') AS current_version_id,
		         COALESCE(cv.blob_hash, '') AS blob_hash,
		         COALESCE(cv.size, 0) AS size,
		         COALESCE(cv.mime_type, '') AS mime_type,
		         n.revision, n.created_at, n.modified_at, n.trashed_at
		  FROM `+nodeFrom+`
		  WHERE n.parent_id = ? AND n.trashed_at IS NULL
		  ORDER BY n.kind = 'file', n.name
		  LIMIT ? OFFSET ?
		)
		SELECT target.kind, totals.total,
		       COALESCE(page.id, 0), page.parent_id, COALESCE(page.name, ''),
		       COALESCE(page.kind, ''), COALESCE(page.current_version_id, ''),
		       COALESCE(page.blob_hash, ''), COALESCE(page.size, 0),
		       COALESCE(page.mime_type, ''), COALESCE(page.revision, 0),
		       COALESCE(page.created_at, ''), COALESCE(page.modified_at, ''),
		       page.trashed_at
		FROM target CROSS JOIN totals LEFT JOIN page ON true
		ORDER BY page.kind = 'file', page.name`, dirID, dirID, dirID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing children of %d: %w", dirID, err)
	}
	defer func() { _ = rows.Close() }()

	children := make([]Node, 0)
	var total int
	found := false
	for rows.Next() {
		found = true
		var targetKind string
		var child Node
		if err := rows.Scan(
			&targetKind, &total, &child.ID, &child.ParentID, &child.Name,
			&child.Kind, &child.CurrentVersionID, &child.BlobHash, &child.Size,
			&child.MimeType, &child.Revision, &child.CreatedAt, &child.ModifiedAt,
			&child.TrashedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("listing children of %d: scanning page: %w", dirID, err)
		}
		if targetKind != nodeKindDir {
			return nil, 0, fmt.Errorf("node %d: %w", dirID, ErrNotDir)
		}
		if child.ID != 0 {
			children = append(children, child)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing children of %d: %w", dirID, err)
	}
	if !found {
		return nil, 0, fmt.Errorf("node %d: %w", dirID, ErrNotFound)
	}
	return children, total, nil
}

// Path returns the display path of a node ("/" for the root).
func (s *Store) Path(ctx context.Context, id int64) (string, error) {
	if _, err := s.NodeByID(ctx, id); err != nil {
		return "", fmt.Errorf("computing path of node %d: %w", id, err)
	}
	return pathOf(ctx, s.db, id)
}

// pathOf computes a node's display path against q — the live database or a
// transaction, for callers that must see the path a mutation in flight is
// about to change (Trash captures the pre-trash path this way).
func pathOf(ctx context.Context, q rowQuerier, id int64) (string, error) {
	var path string
	err := q.QueryRowContext(ctx, `
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
