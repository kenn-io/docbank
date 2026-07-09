package api

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// registerWeb serves the placeholder frontend at the root. The real
// frontend later replaces the embedded assets; the route layout ("/" = UI,
// /api/v1 = API, /docs = API reference) is fixed now.
func registerWeb(mux *http.ServeMux, enabled bool) {
	if !enabled {
		return
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
}
