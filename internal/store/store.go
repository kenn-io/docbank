// Package store implements the SQLite-backed virtual tree.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers sqlite3 driver
)

//go:embed schema.sql
var schemaSQL string

// Store is the single access path to the docbank database.
type Store struct {
	db     *sql.DB
	rootID int64
}

// Open opens (creating if needed) the database at path, applies the schema,
// and guarantees the root directory node exists.
func Open(path string) (*Store, error) {
	dsn := path + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	s := &Store{db: db}
	if err := s.bootstrapRoot(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) bootstrapRoot() error {
	err := s.db.QueryRow(`SELECT id FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("looking up root node: %w", err)
	}
	now := nowRFC3339()
	res, err := s.db.Exec(
		`INSERT INTO nodes (parent_id, name, kind, created_at, modified_at)
		 VALUES (NULL, '', 'dir', ?, ?)`, now, now)
	if err != nil {
		return fmt.Errorf("creating root node: %w", err)
	}
	s.rootID, err = res.LastInsertId()
	if err != nil {
		return fmt.Errorf("reading root node id: %w", err)
	}
	return nil
}

// RootID returns the id of the tree root.
func (s *Store) RootID() int64 { return s.rootID }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// withTx runs fn inside a transaction, committing on nil and rolling back
// on error. Used by later packages (Task 4+) for transactional operations.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %w)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
