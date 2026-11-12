package api_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/docusealclient"
)

func TestDocuSealUnconfigured(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	for _, path := range []string{"/api/v1/integrations/docuseal/status", "/api/v1/integrations/docuseal/templates"} {
		resp, body := do(t, ts, http.MethodGet, path, nil, nil)
		require.Equal(t, 503, resp.StatusCode, body)
		require.Contains(t, body, "docuseal_unconfigured")
	}
}

func TestDocuSealHTTPCheckAndTemplates(t *testing.T) {
	templateBody := ""
	queries := make(chan string, 10)
	var items []string
	for id := 200; id >= 101; id-- {
		items = append(items, fmt.Sprintf(`{"id":%d,"name":"Lease","submitters":[{"name":"Tenant"}]}`, id))
	}
	firstPage := `{"data":[` + strings.Join(items, ",") + `],"pagination":{"next":101}}`
	finalPage := `{"data":[{"id":100,"name":"Final","submitters":[]}],"pagination":{"next":100}}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "synthetic-key" {
			t.Error("missing credential")
		}
		switch r.URL.Path {
		case "/api/user":
			_, _ = w.Write([]byte(`{"id":7,"email":"private@example.test"}`))
		case "/api/templates":
			queries <- r.URL.RawQuery
			body := templateBody
			if body == "" {
				if r.URL.Query().Get("after") == "101" {
					body = finalPage
				} else {
					body = firstPage
				}
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	t.Setenv("DOCUSEAL_API_TEST_KEY", "synthetic-key")
	ts, _ := newTestServer(t, func(d *api.Deps) {
		d.Cfg.Integrations.DocuSeal = config.DocuSealConfig{Enabled: true, Endpoint: provider.URL, APIPath: "/api", APIKeyEnv: "DOCUSEAL_API_TEST_KEY", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.0/8"}}
	})
	resp, body := do(t, ts, http.MethodGet, "/api/v1/integrations/docuseal/status", nil, nil)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.NotContains(t, body, "synthetic-key")
	resp, body = do(t, ts, http.MethodGet, "/api/v1/integrations/docuseal/templates", nil, nil)
	require.Equal(t, 200, resp.StatusCode, body)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.NotContains(t, body, "synthetic-key")
	var page docusealclient.TemplatePage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Templates, 100)
	require.EqualValues(t, 101, page.Next)
	require.Equal(t, "limit=100", <-queries)
	resp, body = do(t, ts, http.MethodGet, fmt.Sprintf("/api/v1/integrations/docuseal/templates?after=%d", page.Next), nil, nil)
	require.Equal(t, 200, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Templates, 1)
	require.EqualValues(t, 100, page.Templates[0].ID)
	require.EqualValues(t, 0, page.Next)
	require.NotNil(t, page.Templates[0].Roles)
	require.Equal(t, "limit=100&after=101", <-queries)

	for _, malformed := range []string{`{"data":[{"id":1,"submitters":[]}]}`, `{"data":[{"id":1,"name":null,"submitters":[]}]}`, `{"data":[{"name":"Lease","submitters":[]}]}`, `{"data":[{"id":null,"name":"Lease","submitters":[]}]}`, `{"data":[{"id":1,"name":"Lease","submitters":[{}]}]}`, `{"data":[{"id":1,"name":"Lease","submitters":[{"name":null}]}]}`, `{"data":[{"id":1,"name":"Lease","submitters":[null]}]}`} {
		templateBody = malformed
		resp, body := do(t, ts, http.MethodGet, "/api/v1/integrations/docuseal/templates", nil, nil)
		require.Equal(t, 502, resp.StatusCode, body)
		require.Contains(t, body, "docuseal_invalid_response")
		<-queries
	}
}

func TestDocuSealUnknownQueryAndInvalidConfig(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Integrations.DocuSeal = config.DocuSealConfig{Enabled: true} })
	resp, body := do(t, ts, http.MethodGet, "/api/v1/integrations/docuseal/templates?aftre=1", nil, nil)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodGet, "/api/v1/integrations/docuseal/status", nil, nil)
	require.Equal(t, 503, resp.StatusCode, body)
	require.Contains(t, body, "docuseal_config_invalid")
}

func TestDocuSealProviderFailuresAreSafe(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{401, "docuseal_unauthorized"}, {404, "docuseal_not_found"}, {500, "docuseal_provider_error"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "synthetic-key private provider body", tc.status)
			}))
			defer provider.Close()
			t.Setenv("DOCUSEAL_API_TEST_KEY", "synthetic-key")
			var logs bytes.Buffer
			ts, _ := newTestServer(t, func(d *api.Deps) {
				d.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				d.Cfg.Integrations.DocuSeal = config.DocuSealConfig{Enabled: true, Endpoint: provider.URL, APIKeyEnv: "DOCUSEAL_API_TEST_KEY", TrustBoundary: "operator_network", AllowedCIDRs: []string{"127.0.0.0/8"}}
			})
			resp, body := do(t, ts, http.MethodGet, "/api/v1/integrations/docuseal/status", nil, nil)
			require.Equal(t, 502, resp.StatusCode, body)
			require.Contains(t, body, tc.code)
			require.NotContains(t, body, "synthetic-key")
			require.NotContains(t, body, "private provider body")
			require.Contains(t, logs.String(), "request")
			require.NotContains(t, logs.String(), "synthetic-key")
			require.NotContains(t, logs.String(), "private provider body")
		})
	}
}

func TestDocuSealDisabledMalformedConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		enabled any
		code    string
	}{{false, "docuseal_unconfigured"}, {"false", "docuseal_config_invalid"}} {
		var cfg config.DocuSealConfig
		require.NoError(t, cfg.UnmarshalTOML(map[string]any{"enabled": tc.enabled, "unknown": true}))
		ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Integrations.DocuSeal = cfg })
		for _, path := range []string{"/api/v1/integrations/docuseal/status", "/api/v1/integrations/docuseal/templates"} {
			resp, body := do(t, ts, http.MethodGet, path, nil, nil)
			require.Equal(t, 503, resp.StatusCode, body)
			require.Contains(t, body, tc.code)
		}
	}
}
