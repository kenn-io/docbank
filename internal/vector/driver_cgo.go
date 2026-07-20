//go:build !windows && cgo

package vector

import (
	"net/url"

	// Register the CGO-backed SQLite driver used by the standalone daemon.
	_ "github.com/mattn/go-sqlite3"
)

const sidecarDriver = "sqlite3"

func sidecarDSN(path string) string {
	query := url.Values{
		"_journal_mode": {"WAL"},
		"_busy_timeout": {"5000"},
		"_synchronous":  {"NORMAL"},
	}
	return sidecarURI(path, query)
}
