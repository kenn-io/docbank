package api

import (
	"context"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/internal/config"
)

func TestWebMapConfigurationMissingHead(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t, "api: embedded web index is missing </head>", func() {
		configureWebMap([]byte("<!doctype html><html><body></body></html>"), config.MapTilesConfig{})
	})
}

func TestWebMapConfiguration(t *testing.T) {
	t.Parallel()
	tiles := config.MapTilesConfig{TileURL: "https://tiles.example.org/{z}/{x}/{y}.png?key=synthetic-provider-key", Attribution: `<img src=x onerror="alert(1)">`, MaxZoom: 19}
	mux := http.NewServeMux()
	registerWeb(mux, true, "http://localhost:8080", "", tiles, nil)
	paths := []string{"/", "/photos", "/photos/map", "/photos/map/22222222-2222-4222-8222-0000000000aa"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, response.Code)
			body := response.Body.String()
			assert.Contains(t, strings.ToLower(body), "<!doctype html>")
			assert.Contains(t, body, `name="docbank-map-tiles"`)
			page := html.UnescapeString(body)
			assert.NotContains(t, page, "synthetic-provider-key")
			assert.NotContains(t, page, "tiles.example.org/{z}")
			assert.Contains(t, page, `"tile_url":"/api/daemon/web-map-tiles/{z}/{x}/{y}"`)
			assert.Contains(t, page, `"host":"tiles.example.org"`)
			assert.NotContains(t, body, "<img src=x")
			csp := response.Header().Get("Content-Security-Policy")
			assert.Contains(t, csp, "img-src 'self' data: blob:;")
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		})
	}
	invalid := httptest.NewRecorder()
	mux.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/photos/map/invalid", nil))
	require.Equal(t, http.StatusNotFound, invalid.Code)
	mux = http.NewServeMux()
	registerWeb(mux, true, "http://localhost:8080", "", config.MapTilesConfig{}, nil)
	for _, path := range paths {
		disabled := httptest.NewRecorder()
		mux.ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, disabled.Code)
		assert.Contains(t, disabled.Header().Get("Content-Security-Policy"), "img-src 'self' data: blob:;")
		assert.Contains(t, html.UnescapeString(disabled.Body.String()), `"tile_url":""`)
	}
	disabled := httptest.NewRecorder()
	mux.ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, webMapTilePath+"/3/4/5", nil))
	assert.Equal(t, http.StatusNotFound, disabled.Code)
}

type tileRig struct {
	handler http.Handler
	logs    *strings.Builder
}

func newTileRig(t *testing.T, upstream *httptest.Server, client *http.Client) tileRig {
	t.Helper()
	tiles := config.MapTilesConfig{TileURL: upstream.URL + "/t/{z}/{x}/{y}.png?key=synthetic-provider-key", Attribution: "Example", MaxZoom: 5}
	if client == nil {
		client = upstream.Client()
	}
	client = providerhttp.IsolateClient(client)
	logs := &strings.Builder{}
	return tileRig{handler: mapTileHandler(tiles, client, slog.New(slog.NewTextHandler(logs, nil))), logs: logs}
}

func (rig tileRig) get(path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Cookie", "session=browser")
	request.Header.Set("Referer", "http://localhost:8080/photos/map")
	request.Header.Set(WebSessionHeader, "tab")
	request.Header.Set("Authorization", "Bearer browser-key")
	request.Header.Set("X-Api-Key", "browser-key")
	response := httptest.NewRecorder()
	coordinates := strings.Split(strings.TrimPrefix(path, webMapTilePath+"/"), "/")
	for i, name := range []string{"z", "x", "y"} {
		request.SetPathValue(name, coordinates[i])
	}
	rig.handler.ServeHTTP(response, request)
	return response
}

func TestWebMapTileProxy(t *testing.T) {
	t.Parallel()
	t.Run("newMapTileClient", func(t *testing.T) {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("client accepted an untrusted certificate")
		}))
		defer upstream.Close()
		client, err := newMapTileClient(config.MapTilesConfig{TileURL: upstream.URL})
		require.NoError(t, err)
		assert.Equal(t, mapTileTimeout, client.Timeout)
		response, err := client.Get("https://elsewhere.invalid/0/0/0")
		if response != nil {
			_ = response.Body.Close()
		}
		require.ErrorIs(t, err, providerhttp.ErrDestinationDenied)
		response, err = client.Get(upstream.URL)
		if response != nil {
			_ = response.Body.Close()
		}
		require.ErrorContains(t, err, "certificate signed by unknown authority")
	})
	const png = "\x89PNG\r\n\x1a\nbody"
	for _, tc := range []struct {
		name, contentType, body, reason string
		status                          int
		timeout, untrusted, canceled    bool
	}{
		{name: "png", contentType: "image/png", body: png},
		{name: "jpeg", body: "\xff\xd8\xffimage"},
		{name: "gif", body: "GIF89aimage"},
		{name: "webp", body: "RIFF0000WEBPVPimage"},
		{name: "mislabeled png", contentType: "text/html", body: png},
		{name: "mislabeled html", contentType: "image/png", body: "<html>", reason: "content_type"},
		{name: "oversize", body: strings.Repeat("x", mapTileMaxBytes+1), reason: "size"},
		{name: "redirect", status: http.StatusFound, reason: "status"},
		{name: "provider error", status: http.StatusInternalServerError, reason: "status"},
		{name: "timeout", timeout: true, reason: "timeout"},
		{name: "canceled", canceled: true},
		{name: "TLS refused", untrusted: true, reason: "connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			seen := make(chan *http.Request, 1)
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				seen <- r
				if tc.timeout {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				if tc.status != 0 {
					w.Header().Set("Location", "/t/0/0/0.png?key=synthetic-provider-key")
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			client := upstream.Client()
			client.Timeout = mapTileTimeout
			if tc.timeout {
				client.Timeout = time.Second
			}
			if tc.untrusted {
				client = &http.Client{Timeout: mapTileTimeout}
			}
			rig := newTileRig(t, upstream, client)
			tile := webMapTilePath + "/1/1/0"
			if tc.name == "png" {
				for _, path := range []string{"/6/0/0", "/-1/0/0", "/1/2/0", "/1/0/2", "/1/x/0", "/1/+1/0", "/01/0/0", "/1/00/0", "/1/0/00", "/999999999999999999999/0/0"} {
					response := rig.get(webMapTilePath + path)
					assert.Equal(t, http.StatusBadRequest, response.Code, path)
					assert.Contains(t, response.Body.String(), "invalid_map_tile")
				}
				assert.Zero(t, calls.Load())
			}
			var response *httptest.ResponseRecorder
			if tc.canceled {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				request := httptest.NewRequestWithContext(ctx, http.MethodGet, tile, nil)
				request.SetPathValue("z", "1")
				request.SetPathValue("x", "1")
				request.SetPathValue("y", "0")
				response = httptest.NewRecorder()
				response.Code = 0
				rig.handler.ServeHTTP(response, request)
			} else {
				response = rig.get(tile)
			}
			if tc.reason != "" {
				assert.Equal(t, http.StatusBadGateway, response.Code)
				assert.Contains(t, response.Body.String(), "map_tile_unavailable")
				assert.Contains(t, rig.logs.String(), "reason="+tc.reason)
				assert.Equal(t, 1, strings.Count(rig.logs.String(), "level=WARN"))
				if tc.status != 0 {
					assert.Contains(t, rig.logs.String(), "status="+strconv.Itoa(tc.status))
				}
			} else if tc.canceled {
				assert.NotContains(t, rig.logs.String(), "level=WARN")
				assert.Zero(t, response.Code)
				assert.Empty(t, response.Header())
				assert.Empty(t, response.Body.String())
				assert.Zero(t, calls.Load())
			} else {
				require.Equal(t, http.StatusOK, response.Code)
				assert.Equal(t, tc.body, response.Body.String())
				assert.Equal(t, http.DetectContentType([]byte(tc.body)), response.Header().Get("Content-Type"))
				if tc.name == "png" {
					assert.Equal(t, strconv.Itoa(len(tc.body)), response.Header().Get("Content-Length"))
					assert.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
					assert.Equal(t, "private, max-age=3600", response.Header().Get("Cache-Control"))
					assert.Equal(t, "X-Api-Key, Authorization, "+WebSessionHeader, strings.Join(response.Header().Values("Vary"), ", "))
					request := <-seen
					assert.Equal(t, "key=synthetic-provider-key", request.URL.RawQuery)
					assert.Equal(t, "image/*", request.Header.Get("Accept"))
					assert.Equal(t, "docbank", request.Header.Get("User-Agent"))
					for _, header := range []string{"Cookie", "Referer", WebSessionHeader, "Authorization", "X-Api-Key"} {
						assert.Empty(t, request.Header.Get(header), header)
					}
				}
			}
			for _, text := range []string{response.Body.String(), rig.logs.String()} {
				assert.NotContains(t, text, "synthetic-provider-key")
				assert.NotContains(t, text, "127.0.0.1")
			}
		})
	}
}

func TestWebMapTileBrowserAllowlist(t *testing.T) {
	t.Parallel()
	allowed := webMapTilePath + "/3/4/5"
	assert.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodGet, allowed, nil)))
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, allowed + "?key=x"},
		{http.MethodPost, allowed},
		{http.MethodGet, allowed + "/6"},
		{http.MethodGet, webMapTilePath + "/3/4"},
	} {
		assert.False(t, webSessionRequestAllowed(httptest.NewRequest(tc.method, tc.path, nil)), tc.method+" "+tc.path)
	}
}
