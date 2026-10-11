package api_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

const signInOrigin = "https://archive.example.test"

type signInStatus struct {
	Enabled bool `json:"enabled"`
	Session *struct {
		Token        string `json:"token"`
		UploadSecret string `json:"upload_secret"`
	} `json:"session"`
}

func signInCall(t *testing.T, s *api.Server, method, path, body, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, signInOrigin+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", signInOrigin)
	if token != "" {
		req.Header.Set(api.WebSessionHeader, token)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func loginBrowser(t *testing.T, s *api.Server) (signInStatus, *http.Cookie) {
	t.Helper()
	good := signInCall(t, s, http.MethodPost, "/api/daemon/web-auth/login", `{"api_key":"`+testAPIKey+`"}`, "", nil)
	require.Equal(t, http.StatusOK, good.Code, good.Body.String())
	cookies := good.Result().Cookies()
	require.Len(t, cookies, 1)
	cookie := cookies[0]
	require.True(t, cookie.HttpOnly)
	require.True(t, cookie.Secure)
	require.True(t, strings.HasPrefix(cookie.Name, "__Host-docbank_session_"))
	require.Equal(t, "/", cookie.Path)
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.Empty(t, cookie.Domain)
	require.NotEqual(t, testAPIKey, cookie.Value)
	var status signInStatus
	require.NoError(t, json.Unmarshal(good.Body.Bytes(), &status))
	require.NotNil(t, status.Session)
	require.NotContains(t, good.Body.String(), cookie.Value)
	require.NotContains(t, good.Body.String(), testAPIKey)
	return status, cookie
}

func TestWebSignInKeyExchangeScopeAndRestart(t *testing.T) {
	t.Parallel()
	var deps api.Deps
	_, catalog := newTestServer(t, func(d *api.Deps) { d.WebLoginEnabled = true; d.Cfg.Web.PublicOrigin = signInOrigin; deps = *d })
	s := catalog.Server
	bad := signInCall(t, s, http.MethodPost, "/api/daemon/web-auth/login", `{"api_key":"wrong"}`, "", nil)
	require.Equal(t, http.StatusUnauthorized, bad.Code)
	status, cookie := loginBrowser(t, s)
	token := status.Session.Token
	require.Equal(t, http.StatusOK, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", token, cookie).Code)
	require.Equal(t, http.StatusForbidden, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", token, nil).Code)
	require.Equal(t, http.StatusUnauthorized, signInCall(t, s, http.MethodPost, "/api/v1/tags", `{"name":"synthetic"}`, "", cookie).Code)
	require.Equal(t, http.StatusForbidden, signInCall(t, s, http.MethodPost, "/api/v1/tags", `{"name":"synthetic"}`, token, nil).Code)
	require.Equal(t, http.StatusCreated, signInCall(t, s, http.MethodPost, "/api/v1/tags", `{"name":"synthetic"}`, token, cookie).Code)
	require.Equal(t, http.StatusForbidden, signInCall(t, s, http.MethodGet, "/api/daemon/web-auth/sessions", "", token, cookie).Code)
	require.Equal(t, http.StatusForbidden, signInCall(t, s, http.MethodPost, "/api/daemon/web-session", "", token, cookie).Code)
	require.Equal(t, http.StatusForbidden, signInCall(t, s, http.MethodPost, "/api/v1/ingest", `{}`, token, cookie).Code)
	s.Close()
	s = api.NewServer(deps)
	t.Cleanup(s.Close)
	require.Equal(t, http.StatusUnauthorized, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", token, cookie).Code, "restart discards browser authority")
	bootstrap := signInCall(t, s, http.MethodGet, "/api/daemon/web-auth", "", "", cookie)
	require.JSONEq(t, `{"enabled":true}`, bootstrap.Body.String(), "restart asks for a new key exchange")
	renewed, newCookie := loginBrowser(t, s)
	require.NotEqual(t, cookie.Value, newCookie.Value)
	require.Equal(t, http.StatusOK, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", renewed.Session.Token, newCookie).Code)
}

func TestWebSignInTabLogoutAndAdminRevocation(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, func(d *api.Deps) { d.WebLoginEnabled = true; d.Cfg.Web.PublicOrigin = signInOrigin })
	s := catalog.Server
	first, cookie := loginBrowser(t, s)
	second, secondCookie := loginBrowser(t, s)
	require.Equal(t, cookie.Name, secondCookie.Name, "tabs at one origin share the instance cookie name")
	require.Equal(t, cookie.Value, secondCookie.Value, "shared cookie must not rotate across tabs")
	logout := signInCall(t, s, http.MethodDelete, "/api/daemon/web-session", "", first.Session.Token, cookie)
	require.Equal(t, http.StatusNoContent, logout.Code)
	require.Empty(t, logout.Result().Cookies(), "logout must not delete another tab's ambient cookie")
	require.Equal(t, http.StatusUnauthorized, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", first.Session.Token, cookie).Code)
	require.Equal(t, http.StatusOK, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", second.Session.Token, cookie).Code)
	listReq := httptest.NewRequest(http.MethodGet, signInOrigin+"/api/daemon/web-auth/sessions", nil)
	listReq.Header.Set("X-Api-Key", testAPIKey)
	listed := httptest.NewRecorder()
	s.Handler().ServeHTTP(listed, listReq)
	require.Equal(t, http.StatusOK, listed.Code)
	var records struct {
		Items []api.WebSignInRecord `json:"items"`
	}
	require.NoError(t, json.Unmarshal(listed.Body.Bytes(), &records))
	require.Len(t, records.Items, 1)
	revoke := httptest.NewRequest(http.MethodDelete, signInOrigin+"/api/daemon/web-auth/sessions/"+records.Items[0].ID, nil)
	revoke.Header.Set("X-Api-Key", testAPIKey)
	revoked := httptest.NewRecorder()
	s.Handler().ServeHTTP(revoked, revoke)
	require.Equal(t, http.StatusNoContent, revoked.Code)
	require.Equal(t, http.StatusUnauthorized, signInCall(t, s, http.MethodGet, "/api/v1/tags", "", second.Session.Token, cookie).Code)
}

func TestWebSignInCookieNamesArePortScoped(t *testing.T) {
	t.Parallel()
	origins := []string{"http://archive.example.test:8181", "http://archive.example.test:8182"}
	servers := make([]*testStore, 0, len(origins))
	for _, origin := range origins {
		_, store := newTestServer(t, func(d *api.Deps) {
			d.WebLoginEnabled = true
			d.Cfg.Web.PublicOrigin = origin
			d.Cfg.Web.TrustPrivateNetwork = true
		})
		servers = append(servers, store)
	}
	login := func(s *api.Server, origin string) *http.Cookie {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, origin+"/api/daemon/web-auth/login", strings.NewReader(`{"api_key":"`+testAPIKey+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		cookies := w.Result().Cookies()
		require.Len(t, cookies, 1)
		cookie := cookies[0]
		require.True(t, cookie.HttpOnly)
		require.False(t, cookie.Secure)
		require.Empty(t, cookie.Domain)
		require.Equal(t, "/", cookie.Path)
		return cookie
	}
	first := login(servers[0].Server, origins[0])
	second := login(servers[1].Server, origins[1])
	require.NotEqual(t, first.Name, second.Name, "browser cookies are not scoped by port")
}

func TestWebSignInHostOriginAndProxyHeaders(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, func(d *api.Deps) {
		d.WebLoginEnabled = true
		d.Cfg.Web.PublicOrigin = signInOrigin
		d.Cfg.Web.AllowedHosts = []string{"backend.example.test:7777"}
	})
	s := catalog.Server
	for _, test := range []struct{ host, origin, site string }{{"evil.example.test", signInOrigin, "same-origin"}, {"archive.example.test", "https://evil.example.test", "cross-site"}, {"archive.example.test", "", "same-origin"}} {
		req := httptest.NewRequest(http.MethodPost, signInOrigin+"/api/daemon/web-auth/login", strings.NewReader(`{"api_key":"`+testAPIKey+`"}`))
		req.Host = test.host
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Sec-Fetch-Site", test.site)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-Host", "archive.example.test")
		req.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)
	}
	for _, path := range []string{"/health", "/api/v1/tags", "/"} {
		req := httptest.NewRequest(http.MethodGet, "http://evil.example.test"+path, nil)
		req.Header.Set("X-Api-Key", testAPIKey)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code, "a master key never bypasses Host validation")
	}
	req := httptest.NewRequest(http.MethodGet, "http://backend.example.test:7777/api/v1/tags", nil)
	req.Header.Set("X-Api-Key", testAPIKey)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "allowed backend authority keeps CLI/API traffic working")
	status, cookie := loginBrowser(t, s)
	originless := httptest.NewRequest(http.MethodPost, signInOrigin+"/api/v1/tags", strings.NewReader(`{"name":"originless"}`))
	originless.Header.Set(api.WebSessionHeader, status.Session.Token)
	originless.AddCookie(cookie)
	denied := httptest.NewRecorder()
	s.Handler().ServeHTTP(denied, originless)
	require.Equal(t, http.StatusForbidden, denied.Code, "browser mutations require the exact public Origin")
	read := signInCall(t, s, http.MethodGet, "/api/v1/tags", "", status.Session.Token, cookie)
	require.Equal(t, "private, no-store", read.Header().Get("Cache-Control"))
	vary := strings.Join(read.Header().Values("Vary"), ",")
	require.Contains(t, vary, "Cookie")
	require.Contains(t, vary, api.WebSessionHeader)
}

func TestWebSignInHostGateRequiresPublicOrigin(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, func(d *api.Deps) {
		d.WebLoginEnabled = true
		d.WebURL = "http://docbank-synthetic.localhost:40000/"
		d.APIAddress = "127.0.0.1:7777"
		d.Cfg.Server.AllowedHosts = []string{"nas.example.test:7777"}
	})
	for _, host := range []string{"127.0.0.1:7777", "localhost:7777", "nas.example.test:7777"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/v1/tags", nil)
		req.Header.Set("X-Api-Key", testAPIKey)
		w := httptest.NewRecorder()
		catalog.Server.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "key login adds no Host gate without public_origin: %s", host)
	}
}

func TestWebSignInStatusIsDisabledAwayFromSignInOrigin(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, func(d *api.Deps) {
		d.WebLoginEnabled = true
		d.Cfg.Web.PublicOrigin = signInOrigin
		d.Cfg.Web.AllowedHosts = []string{"backend.example.test:7777"}
	})
	status := func(host string) string {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/daemon/web-auth", nil)
		w := httptest.NewRecorder()
		catalog.Server.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return w.Body.String()
	}
	require.JSONEq(t, `{"enabled":true}`, status("archive.example.test"))
	require.JSONEq(t, `{"enabled":false}`, status("backend.example.test:7777"))
}

func TestWebSignInMatchesBrowserCanonicalIPv6Origin(t *testing.T) {
	t.Parallel()
	configuredOrigin := "http://[2001:0db8:0000:0000:0000:0000:0000:0001]:08080"
	browserOrigin := "http://[2001:db8::1]:8080"
	_, catalog := newTestServer(t, func(d *api.Deps) {
		d.WebLoginEnabled = true
		d.Cfg.Web.PublicOrigin = configuredOrigin
		d.Cfg.Web.TrustPrivateNetwork = true
	})
	req := httptest.NewRequest(http.MethodPost, browserOrigin+"/api/daemon/web-auth/login", strings.NewReader(`{"api_key":"`+testAPIKey+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", browserOrigin)
	w := httptest.NewRecorder()
	catalog.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestWebSignInMatchesBrowserIPv4MappedIPv6Origin(t *testing.T) {
	t.Parallel()
	configuredOrigin := "http://[::ffff:127.0.0.1]:7777"
	browserOrigin := "http://[::ffff:7f00:1]:7777"
	_, catalog := newTestServer(t, func(d *api.Deps) {
		d.WebLoginEnabled = true
		d.Cfg.Web.PublicOrigin = configuredOrigin
		d.Cfg.Web.TrustPrivateNetwork = true
	})
	req := httptest.NewRequest(http.MethodPost, browserOrigin+"/api/daemon/web-auth/login", strings.NewReader(`{"api_key":"`+testAPIKey+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", browserOrigin)
	w := httptest.NewRecorder()
	catalog.Server.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestWebSignInAcceptsEscapeHeavyMaximumKey(t *testing.T) {
	t.Parallel()
	key := strings.Repeat(`"\`, 2048)
	require.Len(t, key, 4096)
	_, catalog := newTestServer(t, func(d *api.Deps) {
		d.WebLoginEnabled = true
		d.Cfg.Web.PublicOrigin = signInOrigin
		d.Cfg.Server.APIKey = key
	})
	for _, encode := range []func(string) string{
		func(value string) string {
			body, err := json.Marshal(map[string]string{"api_key": value})
			require.NoError(t, err)
			return string(body)
		},
		func(value string) string {
			var body strings.Builder
			for _, r := range value {
				_, _ = fmt.Fprintf(&body, `\u%04x`, r)
			}
			return `{"api_key":"` + body.String() + `"}`
		},
	} {
		body := encode(key)
		response := signInCall(t, catalog.Server, http.MethodPost, "/api/daemon/web-auth/login", body, "", nil)
		require.Equal(t, http.StatusOK, response.Code, "a %d-byte login body must fit: %s", len(body), response.Body.String())
	}
}

func TestWebSignInRejectsMalformedLoginWithoutLockout(t *testing.T) {
	t.Parallel()
	_, catalog := newTestServer(t, func(d *api.Deps) { d.WebLoginEnabled = true; d.Cfg.Web.PublicOrigin = signInOrigin })
	s := catalog.Server
	for _, body := range []string{`{"api_key":"synthetic","unknown":true}`, `{"api_key":`} {
		response := signInCall(t, s, http.MethodPost, "/api/daemon/web-auth/login", body, "", nil)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.NotContains(t, response.Body.String(), "synthetic")
	}
	validLogin := `{"api_key":"` + testAPIKey + `"}`
	oversized := validLogin + strings.Repeat(" ", (32<<10)+1-len(validLogin))
	response := signInCall(t, s, http.MethodPost, "/api/daemon/web-auth/login", oversized, "", nil)
	require.Equal(t, http.StatusBadRequest, response.Code, "a valid login over 32 KiB must be rejected")
	for range 5 {
		response := signInCall(t, s, http.MethodPost, "/api/daemon/web-auth/login", `{"api_key":"synthetic-wrong-key"}`, "", nil)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.NotContains(t, response.Body.String(), "synthetic-wrong-key")
	}
	response = signInCall(t, s, http.MethodPost, "/api/daemon/web-auth/login", validLogin, "", nil)
	require.Equal(t, http.StatusOK, response.Code, "failed guesses must not lock out the operator")
}
