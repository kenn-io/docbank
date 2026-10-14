package api

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json/v2"
	"errors"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/query"
	docweb "go.kenn.io/docbank/internal/web"
)

const (
	webMapTilePath  = "/api/daemon/web-map-tiles"
	mapTileTimeout  = 10 * time.Second
	mapTileMaxBytes = 2 << 20
)

// webMapTiles exposes the provider host while keeping its keyed URL server-side.
type webMapTiles struct {
	TileURL     string `json:"tile_url"`
	Host        string `json:"host"`
	Attribution string `json:"attribution"`
	MaxZoom     int    `json:"max_zoom"`
}

// registerWeb serves the embedded SPA without granting it a privileged data
// path. JavaScript and styles are public static bytes; every vault read still
// crosses the authenticated /api/v1 surface.
// A nil logger selects slog.Default.
func registerWeb(mux *http.ServeMux, enabled bool, webURL, loginOrigin string, tiles config.MapTilesConfig, logger *slog.Logger) {
	if !enabled {
		return
	}
	origins := []string{webURL, loginOrigin}
	assets := docweb.Assets()
	indexHTML, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic("api: embedded web index is missing")
	}
	indexHTML = configureWebMap(indexHTML, tiles)
	images := "'self' data: blob:"
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
		if !isPhotoMapPage(r.URL.Path) {
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
	if tiles.TileURL != "" {
		tileClient, err := newMapTileClient(tiles)
		if err != nil {
			panic("api: map tile client: " + err.Error())
		}
		if logger == nil {
			logger = slog.Default()
		}
		mux.HandleFunc("GET "+webMapTilePath+"/{z}/{x}/{y}", mapTileHandler(tiles, tileClient, logger))
	}
	mux.Handle("GET /assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setWebHeaders(w, images, origins...)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
}

func configureWebMap(indexHTML []byte, tiles config.MapTilesConfig) []byte {
	if !bytes.Contains(indexHTML, []byte("</head>")) {
		panic("api: embedded web index is missing </head>")
	}
	public := webMapTiles{Attribution: tiles.Attribution, MaxZoom: tiles.MaxZoom}
	if tiles.TileURL != "" {
		public.TileURL = webMapTilePath + "/{z}/{x}/{y}"
		public.Host = tiles.Host()
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		panic(err)
	}
	return bytes.Replace(indexHTML, []byte("</head>"), []byte(`<meta name="docbank-map-tiles" content="`+html.EscapeString(string(encoded))+`"></head>`), 1)
}

// Only screenshot builds override the system roots.
var mapTileRootCAs func() (*x509.CertPool, error)

// The operator-configured host is the destination trust boundary.
func newMapTileClient(tiles config.MapTilesConfig) (*http.Client, error) {
	u, _ := url.Parse(tiles.TileURL)
	port, _ := strconv.ParseUint(u.Port(), 10, 16)
	if port == 0 {
		port = 443
	}
	policy := providerhttp.EgressPolicy{
		Scheme: u.Scheme, Host: u.Hostname(), Port: uint16(port), AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}, ProxyMode: providerhttp.ProxyDisabled,
	}
	if mapTileRootCAs != nil {
		roots, err := mapTileRootCAs()
		if err != nil {
			return nil, err
		}
		policy.TLS.RootCAs = roots
	}
	transport, err := providerhttp.NewTransport(policy, nil)
	if err != nil {
		return nil, err
	}
	return providerhttp.IsolateClient(&http.Client{Timeout: mapTileTimeout, Transport: transport}), nil
}

// mapTileHandler keeps the provider URL and key on the server.
func mapTileHandler(tiles config.MapTilesConfig, client *http.Client, logger *slog.Logger) http.HandlerFunc {
	unavailable := NewError(http.StatusBadGateway, "map_tile_unavailable", "The map tile provider did not return a usable tile.")
	return func(w http.ResponseWriter, r *http.Request) {
		// Only a fixed reason is logged: an error from Do embeds the keyed URL.
		fail := func(reason string, status int) {
			if r.Context().Err() != nil {
				return
			}
			logger.Warn("map tile unavailable", "reason", reason, "status", status)
			writeError(w, unavailable)
		}
		z, zok := tileNumber(r.PathValue("z"), tiles.MaxZoom)
		var x, y int
		xok, yok := false, false
		if zok {
			limit := 1<<z - 1
			x, xok = tileNumber(r.PathValue("x"), limit)
			y, yok = tileNumber(r.PathValue("y"), limit)
		}
		if !zok || !xok || !yok {
			writeError(w, NewError(http.StatusBadRequest, "invalid_map_tile", "The map tile coordinates are out of range."))
			return
		}
		target := strings.NewReplacer("{z}", strconv.Itoa(z), "{x}", strconv.Itoa(x), "{y}", strconv.Itoa(y)).Replace(tiles.TileURL)
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
		if err != nil {
			fail("request", 0)
			return
		}
		request.Header.Set("Accept", "image/*")
		request.Header.Set("User-Agent", "docbank")
		response, err := client.Do(request)
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				fail("timeout", 0)
			default:
				fail("connection", 0)
			}
			return
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			fail("status", response.StatusCode)
			return
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, mapTileMaxBytes+1))
		if err != nil {
			fail("read", response.StatusCode)
			return
		}
		if len(body) > mapTileMaxBytes {
			fail("size", response.StatusCode)
			return
		}
		mediaType := http.DetectContentType(body)
		if !slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}, mediaType) {
			fail("content_type", response.StatusCode)
			return
		}
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Header().Add("Vary", "X-Api-Key, Authorization, "+WebSessionHeader)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	}
}

// tileNumber parses a canonical unsigned decimal within the coordinate bound.
func tileNumber(raw string, limit int) (int, bool) {
	if strings.Trim(raw, "0123456789") != "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	return value, err == nil && value <= limit && strconv.Itoa(value) == raw
}

func isPhotoMapPage(path string) bool {
	return isPhotoSetPage(path, "/photos/map/")
}

func isPhotoAlbumPage(path string) bool {
	if path == "/photos/albums" {
		return true
	}
	return isPhotoSetPage(path, "/photos/albums/")
}

func isPhotoSetPage(path, prefix string) bool {
	id, ok := strings.CutPrefix(path, prefix)
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
