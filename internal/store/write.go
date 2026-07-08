package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func nodeByIDTx(tx *sql.Tx, id int64) (Node, error) {
	n, err := scanNode(tx.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
	if err != nil {
		return Node{}, fmt.Errorf("node %d: %w", id, err)
	}
	return n, nil
}

func bumpRevisionTx(tx *sql.Tx, id int64, now string) error {
	if _, err := tx.Exec(
		`UPDATE nodes SET revision = revision + 1, modified_at = ? WHERE id = ?`,
		now, id); err != nil {
		return fmt.Errorf("bumping revision of node %d: %w", id, err)
	}
	return nil
}

// liveDirTx loads id and errors unless it is a live directory.
//
//nolint:unparam // Node result is part of the shared tx-helper contract; later tasks consume it.
func liveDirTx(tx *sql.Tx, id int64) (Node, error) {
	n, err := nodeByIDTx(tx, id)
	if err != nil {
		return Node{}, err
	}
	if n.TrashedAt != nil {
		return Node{}, fmt.Errorf("node %d: %w", id, ErrNotFound)
	}
	if !n.IsDir() {
		return Node{}, fmt.Errorf("node %d: %w", id, ErrNotDir)
	}
	return n, nil
}

func isUniqueViolation(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && se.ExtendedCode == sqlite3.ErrConstraintUnique
}

func isBusy(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && se.Code == sqlite3.ErrBusy
}

// Mkdir creates a directory under parentID.
func (s *Store) Mkdir(ctx context.Context, parentID int64, name string) (Node, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	var created Node
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := liveDirTx(tx, parentID); err != nil {
			return err
		}
		now := nowRFC3339()
		res, err := tx.Exec(
			`INSERT INTO nodes (parent_id, name, kind, created_at, modified_at)
			 VALUES (?, ?, 'dir', ?, ?)`, parentID, name, now, now)
		if isUniqueViolation(err) {
			return fmt.Errorf("mkdir %q under node %d: %w", name, parentID, ErrExists)
		}
		if err != nil {
			return fmt.Errorf("mkdir %q under node %d: %w", name, parentID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("mkdir %q: reading id: %w", name, err)
		}
		if err := bumpRevisionTx(tx, parentID, now); err != nil {
			return err
		}
		created, err = nodeByIDTx(tx, id)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return created, nil
}

// childByName returns the live child of dirID named name (name must already
// be normalized).
func (s *Store) childByName(ctx context.Context, dirID int64, name string) (Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeCols+` FROM nodes
		 WHERE parent_id = ? AND name = ? AND trashed_at IS NULL`, dirID, name)
	return scanNode(row)
}

// MkdirAll creates every missing directory along path and returns the leaf.
func (s *Store) MkdirAll(ctx context.Context, path string) (Node, error) {
	n, err := s.NodeByID(ctx, s.rootID)
	if err != nil {
		return Node{}, err
	}
	for _, seg := range splitPath(path) {
		n, err = s.EnsureDir(ctx, n.ID, seg)
		if err != nil {
			return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
		}
	}
	return n, nil
}

// EnsureDir returns the live directory named name under parentID, creating
// it if missing and converging on a concurrent creation. ID-based on
// purpose: callers that resolved a destination once (ingest) must not
// re-derive it from a path, which a concurrent move or trash can redirect.
func (s *Store) EnsureDir(ctx context.Context, parentID int64, name string) (Node, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	next, err := s.childByName(ctx, parentID, name)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		created, mkErr := s.Mkdir(ctx, parentID, name)
		if mkErr == nil {
			return created, nil
		}
		if !errors.Is(mkErr, ErrExists) {
			return Node{}, mkErr
		}
		// Lost a race with a concurrent create: re-read the now-existing
		// child and converge on it.
		next, err = s.childByName(ctx, parentID, name)
		if err != nil {
			return Node{}, err
		}
	default:
		return Node{}, err
	}
	if !next.IsDir() {
		return Node{}, fmt.Errorf("%q is a file: %w", name, ErrNotDir)
	}
	return next, nil
}

// EnsureBlobTx records a blob row if missing. The blob file must already be
// durable on disk before the enclosing transaction commits. If a blob row
// already exists under hash, its recorded size must match size: a mismatch
// means two different contents hashed to the same value, or a caller passed
// a wrong size, either of which is a corruption signal rather than ordinary
// dedup and must not be silently accepted.
func (s *Store) EnsureBlobTx(tx *sql.Tx, hash string, size int64) error {
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO blobs (hash, size, created_at) VALUES (?, ?, ?)`,
		hash, size, nowRFC3339()); err != nil {
		return fmt.Errorf("recording blob %s: %w", hash, err)
	}
	var stored int64
	if err := tx.QueryRow(`SELECT size FROM blobs WHERE hash = ?`, hash).Scan(&stored); err != nil {
		return fmt.Errorf("verifying blob %s: %w", hash, err)
	}
	if stored != size {
		return fmt.Errorf("blob %s: recorded size %d does not match caller size %d", hash, stored, size)
	}
	return nil
}

func (s *Store) createFileTx(tx *sql.Tx, parentID int64, name, blobHash string, size int64, mimeType string) (Node, error) {
	if _, err := liveDirTx(tx, parentID); err != nil {
		return Node{}, err
	}
	if err := s.EnsureBlobTx(tx, blobHash, size); err != nil {
		return Node{}, err
	}
	now := nowRFC3339()
	res, err := tx.Exec(
		`INSERT INTO nodes (parent_id, name, kind, blob_hash, size, mime_type, created_at, modified_at)
		 VALUES (?, ?, 'file', ?, ?, ?, ?, ?)`,
		parentID, name, blobHash, size, mimeType, now, now)
	if isUniqueViolation(err) {
		return Node{}, fmt.Errorf("creating file %q under node %d: %w", name, parentID, ErrExists)
	}
	if err != nil {
		return Node{}, fmt.Errorf("creating file %q under node %d: %w", name, parentID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, fmt.Errorf("creating file %q: reading id: %w", name, err)
	}
	if err := bumpRevisionTx(tx, parentID, now); err != nil {
		return Node{}, err
	}
	return nodeByIDTx(tx, id)
}

// CreateFile creates a file node pointing at an already-durable blob.
func (s *Store) CreateFile(ctx context.Context, parentID int64, name, blobHash string, size int64, mimeType string) (Node, error) {
	name, err := NormalizeName(name)
	if err != nil {
		return Node{}, err
	}
	var created Node
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		created, err = s.createFileTx(tx, parentID, name, blobHash, size, mimeType)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return created, nil
}

// isAncestorTx reports whether maybeAncestor is candidate itself or one of
// its ancestors (walking parent links).
func isAncestorTx(tx *sql.Tx, maybeAncestor, candidate int64) (bool, error) {
	cur := candidate
	for {
		if cur == maybeAncestor {
			return true, nil
		}
		var parent sql.NullInt64
		err := tx.QueryRow(`SELECT parent_id FROM nodes WHERE id = ?`, cur).Scan(&parent)
		if err != nil {
			return false, fmt.Errorf("walking ancestry of node %d: %w", candidate, err)
		}
		if !parent.Valid {
			return false, nil
		}
		cur = parent.Int64
	}
}

// Move renames and/or reparents a live node in one transaction. If ifRev is
// not -1, the mutation fails with ErrStaleRevision unless it matches the
// node's current revision.
func (s *Store) Move(ctx context.Context, id, newParentID int64, newName string, ifRev int64) (Node, error) {
	var moved Node
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		moved, err = s.moveTx(tx, id, newParentID, newName, ifRev)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return moved, nil
}

// MovePath resolves srcPath and destPath and moves inside one transaction,
// so a concurrent operation cannot relocate either between resolution and
// mutation. A destPath naming an existing live directory means "move into,
// keep name"; otherwise its parent must exist and its basename becomes the
// new name.
func (s *Store) MovePath(ctx context.Context, srcPath, destPath string) (Node, error) {
	var moved Node
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		src, err := nodeByPath(ctx, tx, s.rootID, srcPath)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", srcPath, err)
		}
		if src.ID == s.rootID {
			return ErrIsRoot
		}
		newParentID, newName, err := s.resolveMoveTargetTx(ctx, tx, destPath, src.Name)
		if err != nil {
			return err
		}
		moved, err = s.moveTx(tx, src.ID, newParentID, newName, -1)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return moved, nil
}

func (s *Store) resolveMoveTargetTx(
	ctx context.Context, tx *sql.Tx, destPath, keepName string) (int64, string, error) {
	// Validate every segment up front, literally. Virtual paths have no
	// dot-segment semantics, and deriving the parent with path.Dir would
	// Clean them: "/missing/../renamed" must be rejected, not silently
	// collapsed into a rename at the root.
	segs := splitPath(destPath)
	for i, seg := range segs {
		seg, err := NormalizeName(seg)
		if err != nil {
			return 0, "", fmt.Errorf("destination %q: %w", destPath, err)
		}
		segs[i] = seg
	}
	if dest, err := nodeByPath(ctx, tx, s.rootID, destPath); err == nil {
		if dest.IsDir() {
			return dest.ID, keepName, nil
		}
		return 0, "", fmt.Errorf("destination %q: %w", destPath, ErrExists)
	} else if !errors.Is(err, ErrNotFound) {
		return 0, "", fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	if len(segs) == 0 {
		// Unreachable: "/" always resolves as a directory above.
		return 0, "", fmt.Errorf("internal: unresolvable empty destination %q", destPath)
	}
	parentPath := "/" + strings.Join(segs[:len(segs)-1], "/")
	parent, err := nodeByPath(ctx, tx, s.rootID, parentPath)
	if err != nil {
		return 0, "", fmt.Errorf("resolving destination parent %q: %w", parentPath, err)
	}
	return parent.ID, segs[len(segs)-1], nil
}

func (s *Store) moveTx(tx *sql.Tx, id, newParentID int64, newName string, ifRev int64) (Node, error) {
	if id == s.rootID {
		return Node{}, ErrIsRoot
	}
	newName, err := NormalizeName(newName)
	if err != nil {
		return Node{}, err
	}
	n, err := nodeByIDTx(tx, id)
	if err != nil {
		return Node{}, err
	}
	if n.TrashedAt != nil {
		return Node{}, fmt.Errorf("node %d is trashed: %w", id, ErrNotFound)
	}
	if ifRev >= 0 && n.Revision != ifRev {
		return Node{}, fmt.Errorf("node %d at revision %d, expected %d: %w",
			id, n.Revision, ifRev, ErrStaleRevision)
	}
	if _, err := liveDirTx(tx, newParentID); err != nil {
		return Node{}, err
	}
	// The destination may not be the node itself or any of its
	// descendants (equivalently: id may not be an ancestor of dest).
	inCycle, err := isAncestorTx(tx, id, newParentID)
	if err != nil {
		return Node{}, err
	}
	if inCycle {
		return Node{}, fmt.Errorf("moving node %d under %d: %w", id, newParentID, ErrCycle)
	}
	now := nowRFC3339()
	_, err = tx.Exec(
		`UPDATE nodes SET parent_id = ?, name = ?, revision = revision + 1,
		        modified_at = ? WHERE id = ?`,
		newParentID, newName, now, id)
	if isUniqueViolation(err) {
		return Node{}, fmt.Errorf("moving node %d to %q: %w", id, newName, ErrExists)
	}
	if err != nil {
		return Node{}, fmt.Errorf("moving node %d: %w", id, err)
	}
	oldParent := *n.ParentID
	if err := bumpRevisionTx(tx, oldParent, now); err != nil {
		return Node{}, err
	}
	if newParentID != oldParent {
		if err := bumpRevisionTx(tx, newParentID, now); err != nil {
			return Node{}, err
		}
	}
	return nodeByIDTx(tx, id)
}
