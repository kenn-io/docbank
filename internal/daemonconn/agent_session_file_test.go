package daemonconn

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestAgentSessionFileAttachesWithoutDaemonDiscovery(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seen = request.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "session.json")
	contents, err := json.Marshal(AgentSessionFile{
		Version: 1, Origin: server.URL, Token: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, contents, 0o600))
	t.Setenv(AgentSessionFileEnv, file)
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "missing-vault"))
	client, err := Ensure(t.Context())
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/read", nil)
	require.NoError(t, err)
	request.Header.Set("X-Api-Key", "must-strip")
	response, err := client.hc.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.Equal(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", seen.Get(api.AgentSessionHeader))
	require.Empty(t, seen.Get("X-Api-Key"))
	_, err = EnsureWeb(t.Context())
	require.ErrorIs(t, err, ErrAgentSessionFile)
	_, err = EnsureDaemon(t.Context(), os.Getenv("DOCBANK_HOME"))
	require.ErrorIs(t, err, ErrAgentSessionFile)
	_, statErr := os.Stat(filepath.Join(os.Getenv("DOCBANK_HOME"), "daemon.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestAgentSessionFileRejectsUnsafeOrExpiredInputWithoutFallback(t *testing.T) {
	file := filepath.Join(t.TempDir(), "session.json")
	valid := []byte(`{"version":1,"origin":"http://127.0.0.1:43210","token":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","expires_at":"2999-01-01T00:00:00Z"}`)
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "missing-vault"))
	for name, contents := range map[string][]byte{
		"unknown field": append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"extra":true}`)...),
		"expired":       []byte(`{"version":1,"origin":"http://127.0.0.1:43210","token":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","expires_at":"2000-01-01T00:00:00Z"}`),
		"oversized":     make([]byte, 4097),
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(file, contents, 0o600))
			t.Setenv(AgentSessionFileEnv, file)
			_, err := Ensure(t.Context())
			require.Error(t, err)
		})
	}
	t.Run("empty binding", func(t *testing.T) {
		t.Setenv(AgentSessionFileEnv, "")
		_, err := Ensure(t.Context())
		require.Error(t, err)
	})
	if runtime.GOOS != "windows" {
		t.Run("broad permissions", func(t *testing.T) {
			require.NoError(t, os.WriteFile(file, valid, 0o600))
			require.NoError(t, os.Chmod(file, 0o644))
			_, err := ConnectAgentSessionFile(context.Background(), file)
			require.Error(t, err)
		})
	}
	t.Run("symlink", func(t *testing.T) {
		require.NoError(t, os.WriteFile(file, valid, 0o600))
		link := filepath.Join(t.TempDir(), "linked-session.json")
		if err := os.Symlink(file, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		_, err := ConnectAgentSessionFile(context.Background(), link)
		require.Error(t, err)
	})
}

func TestAgentSessionMCPBearerExclusionUsesScopedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	client, err := AttachAgentSession(server.URL, token)
	require.NoError(t, err)
	require.True(t, NewAPIKeyExclusionPolicy("independent-mcp-bearer").Allows(client))
	require.False(t, NewAPIKeyExclusionPolicy(token).Allows(client))
}
