// Package store implements the SQLite-backed virtual tree.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store is the single access path to the docbank database.
type Store struct {
	db      *sql.DB
	path    string
	rootID  int64
	vaultID string
	driver  docsqlite.Driver
}

// DefaultSQLiteDriver returns the build's standalone-compatible adapter: CGO
// builds use mattn/go-sqlite3 and no-CGO builds use modernc.org/sqlite.
func DefaultSQLiteDriver() docsqlite.Driver { return defaultSQLiteDriver() }

// Open opens (creating if needed) the database at path, applies the schema,
// and guarantees the root directory node exists. When driver is omitted, CGO
// builds use mattn/go-sqlite3 and no-CGO builds use modernc.org/sqlite.
func Open(path string, drivers ...docsqlite.Driver) (*Store, error) {
	if len(drivers) > 1 {
		return nil, errors.New("opening database: at most one sqlite driver may be supplied")
	}
	driver := DefaultSQLiteDriver()
	if len(drivers) == 1 {
		driver = drivers[0]
	}
	if err := docsqlite.Validate(driver); err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.Create, TransactionMode: docsqlite.Immediate,
	})
	if err != nil {
		return nil, fmt.Errorf("opening database %s with %s: %w", path, driver.Name(), err)
	}
	s := &Store{db: db, path: path, driver: driver}
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
		if err == nil || !s.driver.IsBusy(err) || time.Now().After(deadline) {
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
		if err := tx.QueryRow(`SELECT vault_id FROM vault_metadata WHERE singleton = 1`).Scan(
			&s.vaultID,
		); errors.Is(err, sql.ErrNoRows) {
			vaultID, idErr := newUUIDv4()
			if idErr != nil {
				return fmt.Errorf("creating vault identity: %w", idErr)
			}
			if _, idErr = tx.Exec(
				`INSERT INTO vault_metadata(singleton, vault_id) VALUES(1, ?)`, vaultID,
			); idErr != nil {
				return fmt.Errorf("creating vault identity: %w", idErr)
			}
			s.vaultID = vaultID
		} else if err != nil {
			return fmt.Errorf("looking up vault identity: %w", err)
		}
		if err := validateUUIDv4(s.vaultID); err != nil {
			return fmt.Errorf("validating vault identity: %w", err)
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

// VaultID returns the stable logical identity preserved by metadata export,
// backup, and restore. Moving or restoring a vault does not change it.
func (s *Store) VaultID() string { return s.vaultID }

// SQLiteDriver returns the exact adapter used by this store. Backup snapshots
// and embedded lifecycle helpers reuse it for every auxiliary connection.
func (s *Store) SQLiteDriver() docsqlite.Driver { return s.driver }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Checkpoint truncates the WAL after all application writes have completed.
// Portable restore uses it before closing a freshly imported database so the
// main file is self-contained and no sidecar is required for publication.
func (s *Store) Checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpointing database: %w", err)
	}
	if busy != 0 || logFrames != 0 {
		return fmt.Errorf(
			"checkpointing database: WAL remains busy=%d log=%d checkpointed=%d",
			busy, logFrames, checkpointed)
	}
	return nil
}

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
