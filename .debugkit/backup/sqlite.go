package backup

import (
	"database/sql"
	"fmt"
	"time"

	// DefaultSQLiteOpener preserves Kit's existing standalone behavior. An
	// embedding application may supply a different registered SQLite driver.
	_ "github.com/mattn/go-sqlite3"
)

// SQLiteAccess describes how a backup operation may use a database file.
type SQLiteAccess uint8

const (
	// SQLiteReadWriteExisting opens an existing database without creating it.
	SQLiteReadWriteExisting SQLiteAccess = iota + 1
	// SQLiteReadOnlyImmutable opens a completed, writer-free database without
	// creating SQLite sidecars.
	SQLiteReadOnlyImmutable
)

// SQLiteOpenOptions describes the filesystem and locking semantics required
// by one backup database open.
type SQLiteOpenOptions struct {
	Access      SQLiteAccess
	BusyTimeout time.Duration
}

// SQLiteOpener lets an application keep backup and restore on the same SQLite
// implementation as its live metadata store.
type SQLiteOpener interface {
	OpenSQLite(path string, opts SQLiteOpenOptions) (*sql.DB, error)
}

// SQLiteOpenFunc adapts a function to SQLiteOpener.
type SQLiteOpenFunc func(path string, opts SQLiteOpenOptions) (*sql.DB, error)

// OpenSQLite implements SQLiteOpener.
func (f SQLiteOpenFunc) OpenSQLite(path string, opts SQLiteOpenOptions) (*sql.DB, error) {
	return f(path, opts)
}

// DefaultSQLiteOpener uses mattn/go-sqlite3 and preserves Kit's historical
// connection behavior.
type DefaultSQLiteOpener struct{}

// OpenSQLite implements SQLiteOpener.
func (DefaultSQLiteOpener) OpenSQLite(path string, opts SQLiteOpenOptions) (*sql.DB, error) {
	var query string
	switch opts.Access {
	case SQLiteReadWriteExisting:
		query = "mode=rw"
		if opts.BusyTimeout > 0 {
			query = fmt.Sprintf("_busy_timeout=%d&%s", opts.BusyTimeout.Milliseconds(), query)
		}
	case SQLiteReadOnlyImmutable:
		query = "immutable=1&mode=ro"
	default:
		return nil, fmt.Errorf("backup: unsupported SQLite access mode %d", opts.Access)
	}
	return sql.Open("sqlite3", sqliteURIDSN(path, query))
}

func sqliteOpener(opener SQLiteOpener) SQLiteOpener {
	if opener == nil {
		return DefaultSQLiteOpener{}
	}
	return opener
}
