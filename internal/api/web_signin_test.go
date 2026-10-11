package api

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebSignInStatusAvailableWithoutMasterKey(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-master-key"
	s := NewServer(Deps{Cfg: cfg})
	defer s.Close()
	r := httptest.NewRequest(http.MethodGet, "http://localhost/api/daemon/web-auth", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"enabled":false}`, w.Body.String())
	require.NotContains(t, w.Body.String(), cfg.Server.APIKey)
}

func TestWebSignInPublicOriginAllowsVerifiedUploadInCSP(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-master-key"
	cfg.Web.PublicOrigin = "https://archive.example.test"
	s := NewServer(Deps{Cfg: cfg, WebLoginEnabled: true, WebURL: "http://localhost:7777/"})
	defer s.Close()
	for _, path := range []string{"/", "/photos", "/assets/index.js"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, cfg.Web.PublicOrigin+path, nil))
		require.Contains(t, w.Header().Get("Content-Security-Policy"), "wss://archive.example.test")
		require.Contains(t, w.Header().Get("Content-Security-Policy"), "ws://localhost:7777")
	}
}
