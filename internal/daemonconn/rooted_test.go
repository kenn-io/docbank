package daemonconn

import (
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestRootedDaemonAcquisitionIgnoresAmbientHome(t *testing.T) {
	ambient := filepath.Join(t.TempDir(), "ambient")
	t.Setenv("DOCBANK_HOME", ambient)
	root := t.TempDir()
	const token, key = "synthetic-shutdown", "synthetic-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			_ = json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: Service, Version: version.Version, PID: os.Getpid()})
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.Empty(t, r.Header.Get("X-Api-Key"))
			_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)})
		case "/observed":
			assert.Equal(t, key, r.Header.Get("X-Api-Key"))
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err := RuntimeStore(root).Write(NewRecord(strings.TrimPrefix(server.URL, "http://"), key, token, ""))
	require.NoError(t, err)
	for _, ensure := range []bool{false, true} {
		var c *Connection
		if ensure {
			c, err = EnsureFor(t.Context(), root)
		} else {
			var running bool
			c, running, err = RunningFor(t.Context(), root)
			require.True(t, running)
		}
		require.NoError(t, err)
		require.NotNil(t, c)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/observed", nil)
		require.NoError(t, err)
		req.Header.Set("X-Api-Key", key)
		response, err := c.hc.Do(req)
		require.NoError(t, err)
		require.Equal(t, 200, response.StatusCode)
		require.NoError(t, response.Body.Close())
		require.NoError(t, c.Close())
	}
	_, err = os.Stat(ambient)
	require.ErrorIs(t, err, os.ErrNotExist)
	missing := filepath.Join(t.TempDir(), "missing")
	c, running, err := RunningFor(t.Context(), missing)
	require.NoError(t, err)
	require.Nil(t, c)
	require.False(t, running)
	_, err = os.Stat(missing)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRootedDaemonAcquisitionRejectsUnpinnedRoot(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"", "relative"} {
		c, err := EnsureFor(t.Context(), root)
		require.Error(t, err)
		require.Nil(t, c)
		c, ok, err := RunningFor(t.Context(), root)
		require.Error(t, err)
		require.Nil(t, c)
		require.False(t, ok)
	}
}
