package api

import (
	"bytes"
	"encoding/json/v2"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/query"
	docweb "go.kenn.io/docbank/internal/web"
)

// registerWeb serves the embedded SPA without granting it a privileged data
// path. JavaScript and styles are public static bytes; every vault read still
// crosses the authenticated /api/v1 surface.
func registerWeb(mux *http.ServeMux, enabled bool, webURL, loginOrigin string, tiles config.MapTilesConfig) {
	if !enabled {
		return
	}
	origins := []string{webURL, loginOrigin}
	assets := docweb.Assets()
	indexHTML, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic("api: embedded web index is missing")
	}
	public, err := json.Marshal(tiles)
	if err != nil {
		panic(err)
	}
	indexHTML = bytes.Replace(indexHTML, []byte("</head>"), []byte(`<meta name="docbank-map-tiles" content="`+html.EscapeString(string(public))+`"></head>`), 1)
	images := "'self' data: blob:"
	if tiles.TileURL != "" {
		images += " " + tiles.Origin()
	}
	static := http.FileServer(http.FS(assets))
	index := func(w http.ResponseWriter, _ *http.Request) {
		setWebHeaders(w, images, origins...)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /photos", index)
	mux.HandleFunc("GET /photos/hidden", index)
	mux.HandleFunc("GET /photos/map", index)
	mux.HandleFunc("GET /photos/map/{set_id}", func(w http.ResponseWriter, r *http.Request) {
		if query.ValidateTextOperand("set", r.PathValue("set_id")) != nil {
			http.NotFound(w, r)
			return
		}
		index(w, r)
	})
	mux.HandleFunc("GET /photos/albums", index)
	mux.HandleFunc("GET /photos/albums/{set_id}", func(w http.ResponseWriter, r *http.Request) {
		if !isPhotoAlbumPage(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		index(w, r)
	})
	mux.Handle("GET /assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setWebHeaders(w, images, origins...)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
}

func isPhotoAlbumPage(path string) bool {
	if path == "/photos/albums" {
		return true
	}
	id, ok := strings.CutPrefix(path, "/photos/albums/")
	return ok && query.ValidateTextOperand("set", id) == nil
}

func setWebHeaders(w http.ResponseWriter, images string, webURLs ...string) {
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
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src "+images+"; "+
			"connect-src "+connectSources.String()+"; object-src 'none'; "+
			"base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
