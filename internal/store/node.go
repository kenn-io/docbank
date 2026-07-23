package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	nodeKindDir  = "dir"
	nodeKindFile = "file"
)

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

// NodeView binds one node snapshot to its live canonical path. Path is empty
// when the snapshot marks the node as trashed.
type NodeView struct {
	Node Node
	Path string
}

// DirectoryPageView binds a live directory and one child page to the same
// read snapshot. Callers can render child paths without combining directory
// authority from one point in time with children from another.
type DirectoryPageView struct {
	Directory NodeView
	Children  []Node
	Total     int
}

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

// NodeViewByID returns a node and its live path from one read transaction.
func (s *Store) NodeViewByID(ctx context.Context, id int64) (NodeView, error) {
	return s.nodeView(ctx, func(tx *sql.Tx) (Node, error) {
		return nodeByIDTx(tx, id)
	})
}

// NodeViewByPath resolves a live path and returns its node and canonical path
// from one read transaction.
func (s *Store) NodeViewByPath(ctx context.Context, path string) (NodeView, error) {
	return s.nodeView(ctx, func(tx *sql.Tx) (Node, error) {
		return nodeByPath(ctx, tx, s.rootID, path)
	})
}

func (s *Store) nodeView(
	ctx context.Context,
	resolve func(*sql.Tx) (Node, error),
) (NodeView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return NodeView{}, fmt.Errorf("starting node snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	node, err := resolve(tx)
	if err != nil {
		return NodeView{}, err
	}
	view, err := nodeViewForNode(ctx, tx, node)
	if err != nil {
		return NodeView{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeView{}, fmt.Errorf("closing node snapshot: %w", err)
	}
	return view, nil
}

func nodeViewForNode(ctx context.Context, q rowQuerier, node Node) (NodeView, error) {
	view := NodeView{Node: node}
	if node.TrashedAt != nil {
		return view, nil
	}
	path, err := pathOf(ctx, q, node.ID)
	if err != nil {
		return NodeView{}, err
	}
	view.Path = path
	return view, nil
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
	page, err := s.DirectoryChildrenPage(ctx, dirID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return page.Children, page.Total, nil
}

// DirectoryChildrenPage returns a live directory's current canonical path and
// one ordered child page from a single read transaction.
func (s *Store) DirectoryChildrenPage(
	ctx context.Context, dirID int64, limit, offset int,
) (DirectoryPageView, error) {
	if limit < 1 || limit > 5000 {
		return DirectoryPageView{}, errors.New("children limit must be between 1 and 5000")
	}
	if offset < 0 {
		return DirectoryPageView{}, errors.New("children offset must not be negative")
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DirectoryPageView{}, fmt.Errorf("starting directory snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	dir, err := nodeByIDTx(tx, dirID)
	if err != nil {
		return DirectoryPageView{}, err
	}
	if dir.TrashedAt != nil {
		return DirectoryPageView{}, fmt.Errorf("node %d: %w", dirID, ErrNotFound)
	}
	if !dir.IsDir() {
		return DirectoryPageView{}, fmt.Errorf("node %d: %w", dirID, ErrNotDir)
	}
	view, err := nodeViewForNode(ctx, tx, dir)
	if err != nil {
		return DirectoryPageView{}, err
	}

	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE parent_id = ? AND trashed_at IS NULL`,
		dirID,
	).Scan(&total); err != nil {
		return DirectoryPageView{}, fmt.Errorf("counting children of %d: %w", dirID, err)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT `+nodeCols+` FROM `+nodeFrom+`
		 WHERE n.parent_id = ? AND n.trashed_at IS NULL
		 ORDER BY n.kind = 'file', n.name
		 LIMIT ? OFFSET ?`, dirID, limit, offset)
	if err != nil {
		return DirectoryPageView{}, fmt.Errorf("listing children of %d: %w", dirID, err)
	}
	defer func() { _ = rows.Close() }()

	children := make([]Node, 0)
	for rows.Next() {
		child, err := scanNode(rows)
		if err != nil {
			return DirectoryPageView{}, err
		}
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return DirectoryPageView{}, fmt.Errorf("listing children of %d: %w", dirID, err)
	}
	if err := rows.Close(); err != nil {
		return DirectoryPageView{}, fmt.Errorf("closing children of %d: %w", dirID, err)
	}
	if err := tx.Commit(); err != nil {
		return DirectoryPageView{}, fmt.Errorf("closing directory snapshot: %w", err)
	}
	return DirectoryPageView{Directory: view, Children: children, Total: total}, nil
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
