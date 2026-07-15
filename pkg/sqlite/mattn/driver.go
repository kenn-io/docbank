//go:build cgo

// Package mattn adapts github.com/mattn/go-sqlite3 to Docbank.
package mattn

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

// Driver is Docbank's CGO-backed SQLite implementation.
type Driver struct{}

func (Driver) Name() string { return "mattn/go-sqlite3" }

func (Driver) Open(path string, opts docsqlite.OpenOptions) (*sql.DB, error) {
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	query := url.Values{
		"_foreign_keys": {"on"},
		"_busy_timeout": {strconv.FormatInt(busy.Milliseconds(), 10)},
	}
	switch opts.Access {
	case docsqlite.Create:
		query.Set("mode", "rwc")
		query.Set("_journal_mode", "WAL")
	case docsqlite.ReadWriteExisting:
		query.Set("mode", "rw")
		query.Set("_journal_mode", "WAL")
	case docsqlite.ReadOnlyImmutable:
		query.Set("mode", "ro")
		query.Set("immutable", "1")
	default:
		return nil, fmt.Errorf("unsupported SQLite access mode %d", opts.Access)
	}
	if opts.Access != docsqlite.ReadOnlyImmutable {
		query.Set("_txlock", string(opts.TransactionMode))
	}
	return sql.Open("sqlite3", sqliteURI(path, query))
}

func sqliteURI(path string, query url.Values) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: query.Encode()}).String()
}

func (Driver) IsBusy(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrBusy
}

func (Driver) IsUniqueViolation(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique
}

var _ docsqlite.Driver = Driver{}
