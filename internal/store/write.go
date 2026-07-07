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
	return errors.As(err, &se) && se.Code == sqlite3.ErrConstraint
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
			n, err = s.Mkdir(ctx, n.ID, seg)
			if err != nil {
				return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
			}
		default:
			return Node{}, fmt.Errorf("mkdir -p %q: %w", path, err)
		}
	}
	return n, nil
}
