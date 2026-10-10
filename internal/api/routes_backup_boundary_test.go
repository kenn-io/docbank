package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestBackupRestoreMapErrorDoesNotEchoFileContents(t *testing.T) {
	t.Parallel()
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, live := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	const paddedCredential = "c3ludGhldGljLXBhdGRlZC1jcmVkZW50aWFs=="
	storeMap := filepath.Join(t.TempDir(), "malformed-map.toml")
	require.NoError(t, os.WriteFile(storeMap, []byte("\""+paddedCredential+"\" = 1\n"), 0o600))

	requestBody, err := json.Marshal(map[string]any{
		"target":      filepath.Join(t.TempDir(), "restored"),
		"repo":        repoPath,
		"snapshot_id": "synthetic-snapshot",
		"store_map":   storeMap,
	})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, ts.URL+"/api/v1/backup/restore", bytes.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", testAPIKey)
	request.RemoteAddr = "127.0.0.1:12345"

	response := httptest.NewRecorder()
	live.Server.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "restore store map could not be loaded")
	assert.NotContains(t, response.Body.String(), paddedCredential)
}

func TestBackupRestoreRejectsNetworkPeers(t *testing.T) {
	t.Parallel()
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, live := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil,
		map[string]any{"tag": "remote-restore", "jobs": 1})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var snapshot api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(body), &snapshot))

	for _, endpoint := range []string{
		"/api/v1/backup/restore",
		"/api/v1/backup/restore/stream",
	} {
		t.Run(endpoint, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "restored")
			requestBody, err := json.Marshal(map[string]any{
				"target":      target,
				"snapshot_id": snapshot.ID,
			})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, ts.URL+endpoint, bytes.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", testAPIKey)
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
			request.RemoteAddr = "192.0.2.45:12345"

			response := httptest.NewRecorder()
			live.Server.Handler().ServeHTTP(response, request)
			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"loopback_only"`)
			assert.NoDirExists(t, target)
		})
	}
}

func TestBackupExplicitRepositoryRejectsNetworkPeers(t *testing.T) {
	t.Parallel()
	ts, live := newTestServer(t, nil)
	explicitRepo := filepath.Join(t.TempDir(), "explicit-repo")
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/backup/init", map[string]any{"repo": explicitRepo}},
		{http.MethodPost, "/api/v1/backup/snapshots", map[string]any{"repo": explicitRepo}},
		{http.MethodPost, "/api/v1/backup/snapshots/stream", map[string]any{"repo": explicitRepo}},
		{http.MethodGet, "/api/v1/backup/snapshots?repo=" + explicitRepo, nil},
		{http.MethodPost, "/api/v1/backup/verify", map[string]any{"repo": explicitRepo}},
		{http.MethodPost, "/api/v1/backup/verify/stream", map[string]any{"repo": explicitRepo}},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			var requestBody []byte
			if test.body != nil {
				var err error
				requestBody, err = json.Marshal(test.body)
				require.NoError(t, err)
			}
			request := httptest.NewRequest(test.method, ts.URL+test.path, bytes.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", testAPIKey)
			request.RemoteAddr = "192.0.2.46:12345"

			response := httptest.NewRecorder()
			live.Server.Handler().ServeHTTP(response, request)
			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"loopback_only"`)
			assert.NoDirExists(t, explicitRepo)
		})
	}
}

func TestBackupConfiguredRepositoryRemainsAvailableToNetworkPeers(t *testing.T) {
	t.Parallel()
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, live := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	remote := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var requestBody []byte
		if body != nil {
			var err error
			requestBody, err = json.Marshal(body)
			require.NoError(t, err)
		}
		request := httptest.NewRequest(method, ts.URL+path, bytes.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", testAPIKey)
		request.RemoteAddr = "192.0.2.47:12345"
		response := httptest.NewRecorder()
		live.Server.Handler().ServeHTTP(response, request)
		return response
	}

	response := remote(http.MethodPost, "/api/v1/backup/init", map[string]any{})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = remote(http.MethodPost, "/api/v1/backup/snapshots", map[string]any{"tag": "remote", "jobs": 1})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var snapshot api.BackupSnapshot
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &snapshot))

	response = remote(http.MethodGet, "/api/v1/backup/snapshots", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var list api.BackupSnapshotList
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &list))
	require.Len(t, list.Items, 1)
	assert.Equal(t, snapshot.ID, list.Items[0].ID)
}
