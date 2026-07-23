// Package web owns the frontend assets embedded in the Docbank binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist all:fallback
var assetFS embed.FS

// Available reports whether this binary contains the compiled frontend rather
// than only the tracked backend-build fallback.
func Available() bool {
	dist, err := fs.Sub(assetFS, "dist")
	if err != nil {
		return false
	}
	_, err = fs.Stat(dist, "index.html")
	return err == nil
}

// Assets returns the compiled frontend when a release or local frontend build
// installed it, otherwise the tracked backend-only fallback.
func Assets() fs.FS {
	if Available() {
		dist, _ := fs.Sub(assetFS, "dist")
		return dist
	}
	fallback, err := fs.Sub(assetFS, "fallback")
	if err != nil {
		panic("web: embedded fallback is missing")
	}
	return fallback
}
