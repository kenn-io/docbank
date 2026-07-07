package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
		seg, err := NormalizeName(seg)
		if err != nil {
			return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
		}
		next, err := s.childByName(ctx, n.ID, seg)
		switch {
		case err == nil:
			if !next.IsDir() {
				return Node{}, fmt.Errorf("mkdir -p %q: %q is a file: %w", path, seg, ErrNotDir)
			}
			n = next
		case errors.Is(err, ErrNotFound):
			created, mkErr := s.Mkdir(ctx, n.ID, seg)
			if mkErr == nil {
				n = created
				continue
			}
			if !errors.Is(mkErr, ErrExists) {
				return Node{}, fmt.Errorf("mkdir -p %q: %w", path, mkErr)
			}
			// Lost a race with a concurrent MkdirAll/Mkdir: re-read the
			// now-existing child and converge on it.
			next, err = s.childByName(ctx, n.ID, seg)
			if err != nil {
				return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
			}
			if !next.IsDir() {
				return Node{}, fmt.Errorf("mkdir -p %q: %q is a file: %w", path, seg, ErrNotDir)
			}
			n = next
		default:
			return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
		}
	}
	return n, nil
}

// EnsureBlobTx records a blob row if missing. The blob file must already be
// durable on disk before the enclosing transaction commits.
func (s *Store) EnsureBlobTx(tx *sql.Tx, hash string, size int64) error {
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO blobs (hash, size, created_at) VALUES (?, ?, ?)`,
		hash, size, nowRFC3339()); err != nil {
		return fmt.Errorf("recording blob %s: %w", hash, err)
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

// Move renames and/or reparents a live node in one transaction.
func (s *Store) Move(ctx context.Context, id, newParentID int64, newName string) (Node, error) {
	newName, err := NormalizeName(newName)
	if err != nil {
		return Node{}, err
	}
	if id == s.rootID {
		return Node{}, ErrIsRoot
	}
	var moved Node
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(tx, id)
		if err != nil {
			return err
		}
		if n.TrashedAt != nil {
			return fmt.Errorf("node %d is trashed: %w", id, ErrNotFound)
		}
		if _, err := liveDirTx(tx, newParentID); err != nil {
			return err
		}
		// The destination may not be the node itself or any of its
		// descendants (equivalently: id may not be an ancestor of dest).
		inCycle, err := isAncestorTx(tx, id, newParentID)
		if err != nil {
			return err
		}
		if inCycle {
			return fmt.Errorf("moving node %d under %d: %w", id, newParentID, ErrCycle)
		}
		now := nowRFC3339()
		_, err = tx.Exec(
			`UPDATE nodes SET parent_id = ?, name = ?, revision = revision + 1,
			        modified_at = ? WHERE id = ?`,
			newParentID, newName, now, id)
		if isUniqueViolation(err) {
			return fmt.Errorf("moving node %d to %q: %w", id, newName, ErrExists)
		}
		if err != nil {
			return fmt.Errorf("moving node %d: %w", id, err)
		}
		oldParent := *n.ParentID
		if err := bumpRevisionTx(tx, oldParent, now); err != nil {
			return err
		}
		if newParentID != oldParent {
			if err := bumpRevisionTx(tx, newParentID, now); err != nil {
				return err
			}
		}
		moved, err = nodeByIDTx(tx, id)
		return err
	})
	if err != nil {
		return Node{}, err
	}
	return moved, nil
}
