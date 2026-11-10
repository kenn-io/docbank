package mcp

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"net/http"
	"time"
)

// serverIconPNG is the MCP document-and-vault mark rendered at 256x256.
// Regenerate from the repository root with:
//
//	rsvg-convert -w 256 -h 256 internal/mcp/assets/icon.svg | magick - -colors 32 -strip -define png:exclude-chunks=date,time PNG8:internal/mcp/assets/icon.png
//
//go:embed assets/icon.png
var serverIconPNG []byte

// A data URI works over both transports without a deployment-specific URL.
var serverIconSource = "data:image/png;base64," + base64.StdEncoding.EncodeToString(serverIconPNG)

func registerIconRoutes(mux *http.ServeMux) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(serverIconPNG))
	})
	for _, path := range []string{"/icon.png", "/favicon.ico"} {
		mux.Handle("GET "+path, handler)
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		})
	}
}
