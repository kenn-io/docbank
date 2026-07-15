//go:build !cgo

package store

import (
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func defaultSQLiteDriver() docsqlite.Driver { return modernc.Driver{} }
