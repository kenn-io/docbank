package api

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestWebBasePath(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-api-key-0123456789abcdef"
	cfg.Web.BasePath = "/tools/archive"
	cfg.Web.PublicOrigin = "https://library.example.test"
	s := NewServer(Deps{Cfg: cfg, WebLoginEnabled: true, WebURL: "http://localhost:7777/"})
	defer s.Close()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, cfg.Web.PublicOrigin+path, strings.NewReader(body))
		req.Header.Set("Origin", cfg.Web.PublicOrigin)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	w := request("GET", "/tools/archive?view=1", "")
	require.Equal(t, http.StatusTemporaryRedirect, w.Code)
	require.Equal(t, "/tools/archive/?view=1", w.Header().Get("Location"))
	w = request("GET", "/tools/archive//photos", "")
	require.Equal(t, http.StatusTemporaryRedirect, w.Code)
	require.Equal(t, "/tools/archive/photos", w.Header().Get("Location"))
	for _, path := range []string{"/", "/photos", "/searches", "/photos/albums/11111111-1111-4111-8111-111111111111"} {
		w = request("GET", cfg.Web.BasePath+path, "")
		require.Equal(t, http.StatusOK, w.Code, path)
		require.Contains(t, w.Body.String(), `name="docbank-base-path" content="/tools/archive"`)
		require.Contains(t, w.Body.String(), `="/tools/archive/assets/`)
		require.NotContains(t, w.Body.String(), `="/assets/`)
	}
	for _, path := range []string{"/api/daemon/web-auth", "/tools/archive/api/daemon/web-auth"} {
		w = request("GET", path, "")
		require.Equal(t, http.StatusOK, w.Code)
		require.JSONEq(t, `{"enabled":true}`, w.Body.String())
	}
	w = request("POST", "/tools/archive/api/daemon/web-auth/login", `{"api_key":"`+cfg.Server.APIKey+`"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	cookie := w.Result().Cookies()[0]
	require.Equal(t, "/tools/archive/", cookie.Path)
	require.True(t, cookie.Secure)
	require.True(t, cookie.HttpOnly)
	require.True(t, strings.HasPrefix(cookie.Name, "__Secure-"))
	var signed webSignInStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &signed))
	// Mounting cannot broaden the scoped browser credential to daemon control.
	req := httptest.NewRequest(http.MethodPost, cfg.Web.PublicOrigin+"/tools/archive/api/daemon/shutdown", nil)
	req.Header.Set(WebSessionHeader, signed.Session.Token)
	req.Header.Set("Origin", cfg.Web.PublicOrigin)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "web_session_read_only")
	// The proxy mount does not turn a remote peer into a local-filesystem caller.
	req = httptest.NewRequest(http.MethodPost, cfg.Web.PublicOrigin+"/tools/archive/api/v1/ingest", nil)
	req.Header.Set("X-Api-Key", cfg.Server.APIKey)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "loopback_only")
	req = httptest.NewRequest(http.MethodDelete, cfg.Web.PublicOrigin+"/tools/archive/api/daemon/web-session", nil)
	req.Header.Set(WebSessionHeader, signed.Session.Token)
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://other.example.test")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "web_browser")
	// CLI launch keeps the loopback origin but includes the configured mount.
	req = httptest.NewRequest(http.MethodPost, "http://localhost:7777/api/daemon/web-session", nil)
	req.Header.Set("X-Api-Key", cfg.Server.APIKey)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var launch struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &launch))
	require.Equal(t, "http://localhost:7777/tools/archive/", launch.URL)
}

func TestWebBasePathPreservesRequestBoundaries(t *testing.T) {
	t.Parallel()
	handler := webBasePathMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/mailbox/messages/a/b", r.URL.Path)
		assert.Equal(t, "/api/v1/mailbox/messages/a%2Fb", r.URL.EscapedPath())
		assert.Equal(t, "q=a%2Fb", r.URL.RawQuery)
		w.WriteHeader(http.StatusNoContent)
	}), "/archive")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://localhost/archive/api/v1/mailbox/messages/a%2Fb?q=a%2Fb", nil))
	require.Equal(t, http.StatusNoContent, w.Code)
}

func TestWebCookieNamesSeparateMounts(t *testing.T) {
	t.Parallel()
	root := webCookieName(&webLoginPolicy{origin: "https://library.example.test"})
	archive := webCookieName(&webLoginPolicy{origin: "https://library.example.test", basePath: "/archive"})
	other := webCookieName(&webLoginPolicy{origin: "https://library.example.test", basePath: "/other"})
	require.NotEqual(t, root, archive)
	require.NotEqual(t, archive, other)
	require.True(t, strings.HasPrefix(root, "__Host-"))
}
