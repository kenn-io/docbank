package daemonconn_test

import (
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestWebSessionURLMount(t *testing.T) {
	t.Parallel()
	origin := "http://docbank-" + strings.Repeat("a", 32) + ".docbank.localhost:7777"
	for _, tc := range []struct {
		path  string
		valid bool
	}{
		{"/", true}, {"/archive/", true}, {"/tools/archive/", true},
		{"/archive", false}, {"//other/", false}, {"/a/../b/", false}, {"/a%2fb/", false}, {"/api/", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/daemon/web-session", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.MarshalWrite(w, map[string]string{"token": "synthetic-session", "upload_secret": base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "url": origin + tc.path}))
			}))
			defer server.Close()
			got, err := daemonconn.New(server.URL, "synthetic-key").WebSessionURL(t.Context())
			if !tc.valid {
				require.ErrorContains(t, err, "invalid browser origin")
				return
			}
			require.NoError(t, err)
			parsed, err := url.Parse(got)
			require.NoError(t, err)
			require.Equal(t, tc.path, parsed.Path)
			fragment, err := url.ParseQuery(parsed.Fragment)
			require.NoError(t, err)
			require.Equal(t, "synthetic-session", fragment.Get("web_session"))
			require.NotContains(t, got, "synthetic-key")
		})
	}
}
