package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
)

func TestDocuSealUnconfigured(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	for _, path := range []string{"/api/v1/integrations/docuseal/status", "/api/v1/integrations/docuseal/templates"} {
		resp, body := do(t, ts, http.MethodGet, path, nil, nil)
		require.Equal(t, 503, resp.StatusCode, body)
		require.Contains(t, body, "docuseal_unconfigured")
	}
}

func TestDocuSealHTTPCheckAndTemplates(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "synthetic-key", r.Header.Get("X-Auth-Token"))
		switch r.URL.Path {
		case "/api/user":
			_, _ = w.Write([]byte(`{"id":7,"email":"private@example.test"}`))
		case "/api/templates":
			_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"Lease","submitters":[{"name":"Tenant"}]}],"pagination":{"next":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	t.Setenv("DOCUSEAL_API_TEST_KEY", "synthetic-key")
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.DocuSeal = config.DocuSealConfig{Enabled: true, Endpoint: provider.URL, APIPath: "/api", APIKeyEnv: "DOCUSEAL_API_TEST_KEY", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.0/8"}}
	})
	for _, path := range []string{"/api/v1/integrations/docuseal/status", "/api/v1/integrations/docuseal/templates"} {
		resp, body := do(t, ts, http.MethodGet, path, nil, nil)
		require.Equal(t, 200, resp.StatusCode, body)
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		require.NotContains(t, body, "synthetic-key")
		require.NotContains(t, body, "private@example.test")
	}
}
