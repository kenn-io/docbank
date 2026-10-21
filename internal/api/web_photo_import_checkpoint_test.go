package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoImportCheckpointBrowserProfile(t *testing.T) {
	t.Parallel()
	var checkpointDir string
	ts, _ := newTestServer(t, func(d *api.Deps) { checkpointDir = filepath.Join(d.VaultRoot, "web-photo-imports") })
	token := issueWebSession(t, ts)
	request := func(method, path, body string, cookie *http.Cookie, credential, host, origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Host = host
		req.Header.Set("X-Api-Key", "")
		req.Header.Set(api.WebSessionHeader, credential)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	const endpoint = "/api/daemon/photo-import-checkpoint"
	const host = "docbank-0123456789abcdef0123456789abcdef.localhost:43210"
	origin := strings.TrimSuffix(testWebURL, "/")
	call := func(method, path, body string, cookie *http.Cookie) *http.Response {
		return request(method, path, body, cookie, token, host, origin)
	}
	t.Run("removes only expired checkpoint files when creating a profile", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(checkpointDir, 0700))
		old := time.Now().Add(-367 * 24 * time.Hour)
		stale := strings.Repeat("b", 64) + ".jsonl"
		temp := "." + strings.Repeat("c", 64) + ".jsonl.tmp-123456789"
		for _, name := range []string{stale, temp, "unrelated.jsonl", ".unrelated.jsonl.tmp-123456789", strings.Repeat("d", 64) + ".jsonl", "." + strings.Repeat("f", 64) + ".jsonl.tmp-invalid"} {
			path := filepath.Join(checkpointDir, name)
			require.NoError(t, os.WriteFile(path, []byte("old"), 0600))
			if name != strings.Repeat("d", 64)+".jsonl" {
				require.NoError(t, os.Chtimes(path, old, old))
			}
		}

		marginPath := filepath.Join(checkpointDir, strings.Repeat("a", 64)+".jsonl")
		require.NoError(t, os.WriteFile(marginPath, []byte("margin"), 0600))
		margin := time.Now().Add(-365*24*time.Hour - 12*time.Hour)
		require.NoError(t, os.Chtimes(marginPath, margin, margin))
		directoryPath := filepath.Join(checkpointDir, strings.Repeat("0", 64)+".jsonl")
		require.NoError(t, os.Mkdir(directoryPath, 0700))
		require.NoError(t, os.Chtimes(directoryPath, old, old))
		first := call("GET", endpoint+"/header", "", nil)
		require.Equal(t, 200, first.StatusCode)
		require.Len(t, first.Cookies(), 1)
		require.NoFileExists(t, filepath.Join(checkpointDir, stale))
		require.NoFileExists(t, filepath.Join(checkpointDir, temp))
		require.FileExists(t, marginPath)
		require.DirExists(t, directoryPath)
		require.FileExists(t, filepath.Join(checkpointDir, "."+strings.Repeat("f", 64)+".jsonl.tmp-invalid"))
		for _, name := range []string{"unrelated.jsonl", ".unrelated.jsonl.tmp-123456789", strings.Repeat("d", 64) + ".jsonl"} {
			require.FileExists(t, filepath.Join(checkpointDir, name))
		}
		sweepLater := filepath.Join(checkpointDir, strings.Repeat("e", 64)+".jsonl")
		require.NoError(t, os.WriteFile(sweepLater, []byte("old"), 0600))
		require.NoError(t, os.Chtimes(sweepLater, old, old))
		require.Equal(t, 200, call("GET", endpoint, "", first.Cookies()[0]).StatusCode)
		require.FileExists(t, sweepLater)
		require.Equal(t, 200, call("GET", endpoint+"/header", "", nil).StatusCode)
		require.NoFileExists(t, sweepLater)
	})
	t.Run("issues a persistent private profile cookie", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		require.True(t, profile.HttpOnly)
		require.Equal(t, http.SameSiteStrictMode, profile.SameSite)
		require.Positive(t, profile.MaxAge)
	})
	const source = "e1f7a31e-6b61-4aba-a660-360de07064db"
	const revision = "17a02b95-45f7-4c13-b146-bf3df59e458b"
	checkpoint := `{"sourceID":"` + source + `","revision":"` + revision + `","destination":"/photos","manifestVersion":2,"pickerKind":"folder","manifestHash":"` + strings.Repeat("a", 64) + `"}`
	change := `{"next":` + checkpoint + `,"expected":null}`
	t.Run("rejects an oversized serialized replacement and preserves saved progress", func(t *testing.T) {
		profile := call("GET", endpoint+"/header", "", nil).Cookies()[0]
		require.Equal(t, 204, call("PUT", endpoint, change, profile).StatusCode)
		// Compact UUIDs fit the request cap while the stored header expands them.
		expected := `{"sourceID":"` + strings.ReplaceAll(source, "-", "") + `","revision":"` + strings.ReplaceAll(revision, "-", "") + `"}`
		replacement := strings.Replace(checkpoint, "/photos", "/"+strings.Repeat("x", 65175), 1)
		body := `{"next":` + replacement + `,"expected":` + expected + `}`
		require.Less(t, len(body), 64<<10)
		response := call("PUT", endpoint, body, profile)
		require.Equal(t, 500, response.StatusCode)
		problem, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Contains(t, string(problem), "checkpoint_storage")
		loaded, err := io.ReadAll(call("GET", endpoint, "", profile).Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"checkpoint":`+checkpoint+`}`, string(loaded))
		entries, err := os.ReadDir(checkpointDir)
		require.NoError(t, err)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			require.NoError(t, err)
			require.LessOrEqual(t, info.Size(), int64(64<<10), entry.Name())
		}
	})
	t.Run("refreshes the cookie when saving progress", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		require.Equal(t, 409, call("PUT", endpoint, change, nil).StatusCode)
		oldManifest := strings.Replace(change, `"manifestVersion":2`, `"manifestVersion":1`, 1)
		require.Equal(t, 400, call("PUT", endpoint, oldManifest, profile).StatusCode)
		saved := call("PUT", endpoint, change, profile)
		require.Equal(t, 204, saved.StatusCode)
		require.Len(t, saved.Cookies(), 1)
		require.Equal(t, profile.Value, saved.Cookies()[0].Value)
		require.Equal(t, profile.MaxAge, saved.Cookies()[0].MaxAge)
		require.Empty(t, call("GET", endpoint+"/header", "", profile).Cookies())
		loaded := call("GET", endpoint, "", profile)
		require.Equal(t, 200, loaded.StatusCode)
		body, err := io.ReadAll(loaded.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"checkpoint":`+checkpoint+`}`, string(body))
	})
	t.Run("isolates profiles", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		require.Equal(t, 204, call("PUT", endpoint, change, profile).StatusCode)
		// A fresh browser profile gets a new locator and sees no saved selection.
		second := call("GET", endpoint+"/header", "", nil)
		require.Len(t, second.Cookies(), 1)
		require.NotEqual(t, profile.Value, second.Cookies()[0].Value)
		empty, err := io.ReadAll(call("GET", endpoint, "", second.Cookies()[0]).Body)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(empty))
	})
	t.Run("requires a session and matching host and origin", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		for _, tc := range []struct {
			token, host, origin string
			want                int
		}{
			{"", host, origin, 401}, {token, "attacker.invalid", origin, 403}, {token, host, "http://attacker.invalid", 403},
		} {
			require.Equal(t, tc.want, request("GET", endpoint, "", profile, tc.token, tc.host, tc.origin).StatusCode)
		}
	})
	t.Run("isolates the same profile across vaults", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		require.Equal(t, 204, call("PUT", endpoint, change, profile).StatusCode)
		// The same profile locator on another vault cannot retrieve this manifest.
		other, _ := newTestServer(t, nil)
		req, err := http.NewRequest(http.MethodGet, other.URL+endpoint, nil)
		require.NoError(t, err)
		req.Host = host
		req.Header.Set("Origin", origin)
		req.AddCookie(profile)
		response, err := other.Client().Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		require.Equal(t, 200, response.StatusCode)
		empty, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(empty))
	})
	t.Run("replaces and clears only the current revision", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		require.Equal(t, 204, call("PUT", endpoint, change, profile).StatusCode)
		require.Equal(t, 409, call("PUT", endpoint, change, profile).StatusCode)
		require.Equal(t, 409, call("PUT", endpoint, `{"next":null,"expected":null}`, profile).StatusCode)
		expected := `{"sourceID":"` + source + `","revision":"` + revision + `"}`
		const resumedRevision = "d9c7508d-6226-4249-9c64-f7dd17f01b26"
		resumed := strings.Replace(checkpoint, revision, resumedRevision, 1)
		require.Equal(t, 204, call("PUT", endpoint, `{"next":`+resumed+`,"expected":`+expected+`}`, profile).StatusCode)
		require.Equal(t, 409, call("PUT", endpoint, `{"next":`+checkpoint+`,"expected":`+expected+`}`, profile).StatusCode)
		require.Equal(t, 409, call("PUT", endpoint, `{"next":null,"expected":`+expected+`}`, profile).StatusCode)
		expected = strings.Replace(expected, revision, resumedRevision, 1)
		require.Equal(t, 204, call("PUT", endpoint, `{"next":null,"expected":`+expected+`}`, profile).StatusCode)
		empty, err := io.ReadAll(call("GET", endpoint, "", profile).Body)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(empty))
	})
	t.Run("blocks a malformed saved format", func(t *testing.T) {
		created := call("GET", endpoint+"/header", "", nil)
		require.Len(t, created.Cookies(), 1)
		profile := created.Cookies()[0]
		header := `{"formatVersion":2,"sourceID":"` + source + `","revision":"` + revision + `"}`
		require.NoError(t, os.WriteFile(filepath.Join(checkpointDir, profile.Value+".jsonl"), []byte(header+"\n{broken"), 0600))
		resp := call("GET", endpoint, "", profile)
		require.Equal(t, 409, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), `"code":"checkpoint_format"`)
	})
	t.Run("rejects malformed bodies", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			saved      bool
		}{
			{"omitted next", `{"expected":{"sourceID":"` + source + `","revision":"` + revision + `"}}`, true},
			{"unknown member", `{"next":` + checkpoint + `,"expected":null,"unknown":true}`, false},
			{"omitted expected", `{"next":` + checkpoint + `}`, false},
			{"uppercase hash", strings.ReplaceAll(change, strings.Repeat("a", 64), strings.Repeat("A", 64)), false},
			{"oversized body", strings.Repeat(" ", (64<<10)+1), true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				created := call("GET", endpoint+"/header", "", nil)
				require.Len(t, created.Cookies(), 1)
				profile := created.Cookies()[0]
				want := `{}`
				if tc.saved {
					require.Equal(t, 204, call("PUT", endpoint, change, profile).StatusCode)
					want = `{"checkpoint":` + checkpoint + `}`
				}
				assert.Equal(t, 400, call("PUT", endpoint, tc.body, profile).StatusCode)
				loaded := call("GET", endpoint, "", profile)
				require.Equal(t, 200, loaded.StatusCode)
				body, err := io.ReadAll(loaded.Body)
				require.NoError(t, err)
				assert.JSONEq(t, want, string(body))
			})
		}
	})
}

func TestPhotoImportCheckpointCookieOrigin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, origin string
		secure       bool
		tls          bool
		proxy        bool
	}{
		{"HTTPS proxy", "https://archive.example.test", true, false, true},
		{"HTTP loopback", strings.TrimSuffix(testWebURL, "/"), false, false, false},
		{"direct TLS", "", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := tc.origin
			var tlsServer *httptest.Server
			if tc.tls {
				tlsServer = httptest.NewUnstartedServer(nil)
				t.Cleanup(tlsServer.Close)
				origin = "https://" + tlsServer.Listener.Addr().String()
			}
			ts, catalog := newTestServer(t, func(d *api.Deps) {
				if tc.proxy {
					d.WebLoginEnabled = true
					d.Cfg.Web.PublicOrigin = origin
				}
				if tc.tls {
					d.Cfg.Web.PublicOrigin = origin
				}
			})
			if tlsServer != nil {
				tlsServer.Config.Handler = catalog.Server.Handler()
				tlsServer.StartTLS()
				tlsServer.Client().Transport = &apiKeyTransport{key: testAPIKey, next: tlsServer.Client().Transport}
				ts = tlsServer
			}
			var token string
			var loginCookie *http.Cookie
			if tc.proxy {
				status, cookie := loginBrowser(t, catalog.Server)
				token, loginCookie = status.Session.Token, cookie
			} else {
				token = issueWebSession(t, ts)
			}
			profileURL, err := url.Parse(origin)
			require.NoError(t, err)
			call := func(method, path, body string, cookie *http.Cookie) *http.Response {
				t.Helper()
				req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
				require.NoError(t, err)
				req.Host = profileURL.Host
				req.Header.Set("X-Api-Key", "")
				req.Header.Set(api.WebSessionHeader, token)
				if method == http.MethodPut {
					req.Header.Set("Origin", origin)
				}
				req.Header.Set("Content-Type", "application/json")
				if loginCookie != nil {
					req.AddCookie(loginCookie)
				}
				if cookie != nil {
					req.AddCookie(cookie)
				}
				resp, err := ts.Client().Do(req)
				require.NoError(t, err)
				t.Cleanup(func() { _ = resp.Body.Close() })
				return resp
			}
			const endpoint = "/api/daemon/photo-import-checkpoint"
			created := call(http.MethodGet, endpoint+"/header", "", nil)
			require.Equal(t, http.StatusOK, created.StatusCode)
			require.Len(t, created.Cookies(), 1)
			profile := created.Cookies()[0]
			assert.Equal(t, tc.secure, profile.Secure)
			saved := call(http.MethodPut, endpoint, `{"next":null,"expected":null}`, profile)
			require.Equal(t, http.StatusNoContent, saved.StatusCode)
			require.Len(t, saved.Cookies(), 1)
			assert.Equal(t, tc.secure, saved.Cookies()[0].Secure)
		})
	}
}
