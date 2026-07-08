// Package store implements the SQLite-backed virtual tree.
package store

import (
	"context"
	"database/sql"
	_ "embed"
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
	s := &Store{db: db}
	if err := s.bootstrap(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// bootstrap applies the schema and creates the root node if missing, all in
// one transaction, retrying SQLITE_BUSY. Both halves matter for concurrent
// first-time Opens: _txlock=immediate makes the transaction BEGIN IMMEDIATE
// (autocommit DDL upgrades a read lock mid-statement, which fails without
// invoking the busy handler), and the retry loop covers converting the fresh
// database to WAL, which needs an exclusive lock and likewise fails
// immediately while another connection is mid-bootstrap. Every statement is
// idempotent, so retrying the whole transaction is safe. The root
// insert-then-select stays a single atomic statement as extra insurance
// against racing a read-then-insert into the one_root unique index.
func (s *Store) bootstrap() error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := s.bootstrapTx()
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) bootstrapTx() error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(schemaSQL); err != nil {
			return fmt.Errorf("applying schema: %w", err)
		}
		now := nowRFC3339()
		if _, err := tx.Exec(
			`INSERT INTO nodes (parent_id, name, kind, created_at, modified_at)
			 SELECT NULL, '', 'dir', ?, ?
			 WHERE NOT EXISTS (SELECT 1 FROM nodes WHERE parent_id IS NULL)`,
			now, now); err != nil {
			return fmt.Errorf("creating root node: %w", err)
		}
		if err := tx.QueryRow(
			`SELECT id FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID); err != nil {
			return fmt.Errorf("looking up root node: %w", err)
		}
		return nil
	})
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

// timestampLayout is RFC 3339 UTC with fixed-width nanoseconds. Timestamps
// are compared and sorted as strings in SQL (EmptyTrash, TrashedRoots), so
// lexicographic order must match chronological order; RFC3339Nano trims
// trailing zeros and breaks that within a second.
const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nowRFC3339() string {
	return time.Now().UTC().Format(timestampLayout)
}
