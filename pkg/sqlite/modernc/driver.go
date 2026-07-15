// Package modernc adapts modernc.org/sqlite to Docbank without CGO.
package modernc

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	sqliteBusy             = 5
	sqliteConstraintUnique = 2067
)

// Driver is Docbank's pure-Go SQLite implementation.
type Driver struct{}

func (Driver) Name() string { return "modernc.org/sqlite" }

func (Driver) Open(path string, opts docsqlite.OpenOptions) (*sql.DB, error) {
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	query := url.Values{
		"_pragma": {
			fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()),
			"foreign_keys(1)",
		},
	}
	switch opts.Access {
	case docsqlite.Create:
		query.Set("mode", "rwc")
		query.Add("_pragma", "journal_mode(WAL)")
	case docsqlite.ReadWriteExisting:
		query.Set("mode", "rw")
		query.Add("_pragma", "journal_mode(WAL)")
	case docsqlite.ReadOnlyImmutable:
		query.Set("mode", "ro")
		query.Set("immutable", "1")
	default:
		return nil, fmt.Errorf("unsupported SQLite access mode %d", opts.Access)
	}
	if opts.Access != docsqlite.ReadOnlyImmutable {
		query.Set("_txlock", string(opts.TransactionMode))
	}
	return sql.Open("sqlite", sqliteURI(path, query))
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
	code, ok := errorCode(err)
	return ok && code&0xff == sqliteBusy
}

func (Driver) IsUniqueViolation(err error) bool {
	code, ok := errorCode(err)
	return ok && code == sqliteConstraintUnique
}

func errorCode(err error) (int, bool) {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return 0, false
	}
	return sqliteErr.Code(), true
}

var _ docsqlite.Driver = Driver{}
