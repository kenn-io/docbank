package backupapp

import (
	"database/sql"
	"fmt"

	"go.kenn.io/kit/backup"

	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

type sqliteOpener struct{ driver docsqlite.Driver }

// SQLiteOpener adapts Docbank's selected driver to Kit's backup access modes.
func SQLiteOpener(driver docsqlite.Driver) backup.SQLiteOpener {
	return sqliteOpener{driver: driver}
}

func (o sqliteOpener) OpenSQLite(path string, opts backup.SQLiteOpenOptions) (*sql.DB, error) {
	access := docsqlite.ReadWriteExisting
	switch opts.Access {
	case backup.SQLiteReadWriteExisting:
	case backup.SQLiteReadOnlyImmutable:
		access = docsqlite.ReadOnlyImmutable
	default:
		return nil, fmt.Errorf("backupapp: unsupported SQLite access mode %d", opts.Access)
	}
	return o.driver.Open(path, docsqlite.OpenOptions{
		Access: access, TransactionMode: docsqlite.Deferred, BusyTimeout: opts.BusyTimeout,
	})
}
