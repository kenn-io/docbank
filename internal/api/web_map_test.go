package api

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
)

func TestWebMapConfiguration(t *testing.T) {
	t.Parallel()
	tiles := config.MapTilesConfig{TileURL: "https://tiles.example.org/{z}/{x}/{y}.png", Attribution: `<img src=x onerror="alert(1)">`, MaxZoom: 19}
	mux := http.NewServeMux()
	registerWeb(mux, true, "http://localhost:8080", "", tiles)
	paths := []string{"/photos/map", "/photos/map/22222222-2222-4222-8222-0000000000aa"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, response.Code)
			body := response.Body.String()
			assert.Contains(t, strings.ToLower(body), "<!doctype html>")
			assert.Contains(t, body, `name="docbank-map-tiles"`)
			assert.Contains(t, html.UnescapeString(body), `"tile_url":"https://tiles.example.org/{z}/{x}/{y}.png"`)
			assert.NotContains(t, body, "<img src=x")
			csp := response.Header().Get("Content-Security-Policy")
			assert.Contains(t, csp, "img-src 'self' data: blob: https://tiles.example.org;")
			assert.Equal(t, 1, strings.Count(csp, "https://tiles.example.org"))
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
		})
	}
	invalid := httptest.NewRecorder()
	mux.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/photos/map/invalid", nil))
	require.Equal(t, http.StatusNotFound, invalid.Code)
	mux = http.NewServeMux()
	registerWeb(mux, true, "http://localhost:8080", "", config.MapTilesConfig{})
	for _, path := range paths {
		disabled := httptest.NewRecorder()
		mux.ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, disabled.Code)
		assert.Contains(t, disabled.Header().Get("Content-Security-Policy"), "img-src 'self' data: blob:;")
		assert.Contains(t, html.UnescapeString(disabled.Body.String()), `"tile_url":""`)
	}
}
