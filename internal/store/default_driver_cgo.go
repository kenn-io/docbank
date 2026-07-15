//go:build cgo

package store

import (
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/mattn"
)

func defaultSQLiteDriver() docsqlite.Driver { return mattn.Driver{} }
