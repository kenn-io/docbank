// Package web owns the frontend assets embedded in the Docbank binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist all:fallback
var assetFS embed.FS

// Assets returns the compiled frontend when a release or local frontend build
// installed it, otherwise the tracked backend-only fallback.
func Assets() fs.FS {
	dist, err := fs.Sub(assetFS, "dist")
	if err == nil {
		if _, statErr := fs.Stat(dist, "index.html"); statErr == nil {
			return dist
		}
	}
	fallback, err := fs.Sub(assetFS, "fallback")
	if err != nil {
		panic("web: embedded fallback is missing")
	}
	return fallback
}
