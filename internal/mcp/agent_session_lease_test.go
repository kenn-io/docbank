package mcp

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestAgentSessionLeaseReconnectNeverStartsMasterDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "session.json")
	write := func(token string, expiry time.Time) {
		t.Helper()
		raw, err := json.Marshal(daemonconn.AgentSessionFile{
			Version: 1, Origin: server.URL, Token: token, ExpiresAt: expiry,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, raw, 0o600))
	}
	write("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", time.Now().Add(time.Hour))
	t.Setenv(daemonconn.AgentSessionFileEnv, file)
	home := filepath.Join(t.TempDir(), "missing-vault")
	t.Setenv("DOCBANK_HOME", home)
	lease := newDaemonLease()
	first, err := lease.acquire(t.Context())
	require.NoError(t, err)
	require.NotNil(t, first.daemonconn)
	write("bad-token", time.Now().Add(time.Hour))
	_, err = lease.replace(t.Context(), first)
	require.Error(t, err)
	_, statErr := os.Stat(home)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
