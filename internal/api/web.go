package api

import (
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	docweb "go.kenn.io/docbank/internal/web"
)

// registerWeb serves the embedded SPA without granting it a privileged data
// path. JavaScript and styles are public static bytes; every vault read still
// crosses the authenticated /api/v1 surface.
func registerWeb(mux *http.ServeMux, enabled bool, webURL, loginOrigin string) {
	if !enabled {
		return
	}
	origins := []string{webURL, loginOrigin}
	assets := docweb.Assets()
	indexHTML, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic("api: embedded web index is missing")
	}
	static := http.FileServer(http.FS(assets))
	index := func(w http.ResponseWriter, _ *http.Request) {
		setWebHeaders(w, origins...)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /photos", index)
	mux.Handle("GET /assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setWebHeaders(w, origins...)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
}

func setWebHeaders(w http.ResponseWriter, webURLs ...string) {
	var connectSources strings.Builder
	connectSources.WriteString("'self'")
	seen := map[string]bool{}
	for _, webURL := range webURLs {
		if parsed, err := url.Parse(webURL); err == nil && parsed.Host != "" {
			source := ""
			switch parsed.Scheme {
			case "http":
				source = "ws://" + parsed.Host
			case "https":
				source = "wss://" + parsed.Host
			}
			if source != "" && !seen[source] {
				connectSources.WriteString(" " + source)
				seen[source] = true
			}
		}
	}
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; "+
			"connect-src "+connectSources.String()+"; object-src 'none'; "+
			"base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
