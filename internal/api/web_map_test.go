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
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/photos/map", nil))
	require.Equal(t, http.StatusOK, response.Code)
	body := response.Body.String()
	assert.Contains(t, body, `name="docbank-map-tiles"`)
	assert.Contains(t, html.UnescapeString(body), `"tile_url":"https://tiles.example.org/{z}/{x}/{y}.png"`)
	assert.NotContains(t, body, "<img src=x")
	csp := response.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "img-src 'self' data: blob: https://tiles.example.org;")
	assert.Equal(t, 1, strings.Count(csp, "https://tiles.example.org"))
	disabled := httptest.NewRecorder()
	mux = http.NewServeMux()
	registerWeb(mux, true, "http://localhost:8080", "", config.MapTilesConfig{})
	mux.ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, "/photos/map", nil))
	require.Equal(t, http.StatusOK, disabled.Code)
	assert.Contains(t, disabled.Header().Get("Content-Security-Policy"), "img-src 'self' data: blob:;")
	assert.Contains(t, html.UnescapeString(disabled.Body.String()), `"tile_url":""`)
}
