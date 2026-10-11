package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/docbank/internal/config"
)

func TestNetworkHostGuardPrecedesAuthentication(t *testing.T) {
	t.Parallel()
	cfg := config.Default().Server
	cfg.BindAddr = "0.0.0.0"
	cfg.APIKey = "synthetic-key"
	cfg.AllowedHosts = []string{"docbank:8485"}
	handler := hostMiddleware(authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), cfg.APIKey, nil, ""), cfg, "http://docbank-synthetic.localhost:1234/", nil)
	for _, test := range []struct {
		host, key string
		status    int
	}{
		{"docbank:8485", "synthetic-key", 204}, {"docbank:8485", "", 401}, {"docbank:8485", "wrong", 401},
		{"attacker.example:8485", "synthetic-key", 403}, {"0.0.0.0:8485", "synthetic-key", 403},
		{"docbank:8486", "synthetic-key", 403}, {"docbank-synthetic.localhost:1234", "synthetic-key", 204},
		{"docbank-other.localhost:1234", "synthetic-key", 403}, {"127.0.0.1:8485", "synthetic-key", 204},
	} {
		t.Run(test.host+test.key, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://localhost/api/v1/info", nil)
			r.Host = test.host
			r.Header.Set("X-Api-Key", test.key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, test.status, w.Code)
		})
	}
}

func TestNetworkServerPathRoutesIgnoreForwardingHeaders(t *testing.T) {
	t.Parallel()
	handler := loopbackMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, path := range []string{"/api/v1/ingest", "/api/v1/ingest/stream", "/api/v1/ingest/preflight", "/api/v1/packages/preflights", "/api/v1/photos/imports"} {
		r := httptest.NewRequest(http.MethodPost, "http://docbank:8485"+path, nil)
		r.RemoteAddr = "192.0.2.20:1234"
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("Forwarded", "for=127.0.0.1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, 403, w.Code, path)
		r.RemoteAddr = "127.0.0.1:1234"
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		assert.Equal(t, 204, w.Code, path)
	}
}
