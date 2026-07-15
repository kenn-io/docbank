//go:build cgo

package store

import (
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/mattn"
)

func defaultSQLiteDriver() docsqlite.Driver { return mattn.Driver{} }
