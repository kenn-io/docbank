//go:build !cgo

package store

import (
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func defaultSQLiteDriver() docsqlite.Driver { return modernc.Driver{} }
