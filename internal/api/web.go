package api

import (
	"io/fs"
	"net/http"
	"net/url"

	docweb "go.kenn.io/docbank/internal/web"
)

// registerWeb serves the embedded SPA without granting it a privileged data
// path. JavaScript and styles are public static bytes; every vault read still
// crosses the authenticated /api/v1 surface.
func registerWeb(mux *http.ServeMux, enabled bool, webURL string) {
	if !enabled {
		return
	}
	assets := docweb.Assets()
	indexHTML, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic("api: embedded web index is missing")
	}
	static := http.FileServer(http.FS(assets))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		setWebHeaders(w, webURL)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.Handle("GET /assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setWebHeaders(w, webURL)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
}

func setWebHeaders(w http.ResponseWriter, webURL string) {
	connectSources := "'self'"
	if parsed, err := url.Parse(webURL); err == nil && parsed.Host != "" {
		switch parsed.Scheme {
		case "http":
			connectSources += " ws://" + parsed.Host
		case "https":
			connectSources += " wss://" + parsed.Host
		}
	}
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"connect-src "+connectSources+"; object-src 'none'; "+
			"base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
