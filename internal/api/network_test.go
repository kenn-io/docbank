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
	cfg := config.Default()
	cfg.Server.BindAddr = "0.0.0.0"
	cfg.Server.APIKey = "synthetic-key"
	cfg.Server.AllowedHosts = []string{"docbank:8485"}
	server := NewServer(Deps{Cfg: cfg, WebURL: "http://docbank-synthetic.localhost:1234/"})
	t.Cleanup(server.Close)
	handler := server.Handler()
	for _, test := range []struct {
		host, key string
		status    int
	}{
		{"docbank:8485", "synthetic-key", 404},
		{"attacker.example:8485", "wrong", 403}, {"0.0.0.0:8485", "synthetic-key", 403},
		{"docbank:8486", "synthetic-key", 403}, {"docbank-synthetic.localhost:1234", "synthetic-key", 404},
		{"docbank-other.localhost:1234", "synthetic-key", 403}, {"127.0.0.1:8485", "synthetic-key", 404},
	} {
		t.Run(test.host+test.key, func(t *testing.T) {
			path := "/api/v1/unknown"
			if test.host == "attacker.example:8485" {
				path = "/api/v1/nodes/1"
			}
			r := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
			r.Host = test.host
			r.Header.Set("X-Api-Key", test.key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, test.status, w.Code)
		})
	}
}
