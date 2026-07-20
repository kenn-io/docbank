//go:build windows || !cgo

package vector

import (
	"net/url"

	_ "modernc.org/sqlite"
)

const sidecarDriver = "sqlite"

func sidecarDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "synchronous(NORMAL)")
	return sidecarURI(path, query)
}
