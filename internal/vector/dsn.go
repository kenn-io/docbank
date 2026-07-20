package vector

import (
	"net/url"
	"path/filepath"
	"strings"
)

func sidecarURI(path string, query url.Values) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}
