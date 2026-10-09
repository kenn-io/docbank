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

func TestBackupRestoreStoreMapLoopbackOnly(t *testing.T) {
	t.Parallel()
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, live := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	const paddedCredential = "c3ludGhldGljLXBhdGRlZC1jcmVkZW50aWFs=="
	storeMap := filepath.Join(t.TempDir(), "owner-private-map.toml")
	require.NoError(t, os.WriteFile(storeMap, []byte("\""+paddedCredential+"\" = 1\n"), 0o600))
	endpoints := []string{
		"/api/v1/backup/restore",
		"/api/v1/backup/restore/stream",
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint, func(t *testing.T) {
			requestBody, err := json.Marshal(map[string]any{
				"target":      filepath.Join(t.TempDir(), "restored"),
				"repo":        repoPath,
				"snapshot_id": "synthetic-snapshot",
				"store_map":   storeMap,
			})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, ts.URL+endpoint, bytes.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", testAPIKey)
			request.Header.Set("X-Forwarded-For", "127.0.0.1")
			request.Header.Set("Forwarded", "for=127.0.0.1")
			request.RemoteAddr = "192.0.2.44:12345"

			response := httptest.NewRecorder()
			live.Server.Handler().ServeHTTP(response, request)

			assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"loopback_only"`)
			assert.NotContains(t, response.Body.String(), paddedCredential)
		})
	}
}

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

func TestBackupRestoreWithoutStoreMapRemainsAvailableRemotely(t *testing.T) {
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
			requestBody, err := json.Marshal(map[string]any{
				"target":      filepath.Join(t.TempDir(), "restored"),
				"repo":        repoPath,
				"snapshot_id": snapshot.ID,
			})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, ts.URL+endpoint, bytes.NewReader(requestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", testAPIKey)
			request.RemoteAddr = "192.0.2.45:12345"

			response := httptest.NewRecorder()
			live.Server.Handler().ServeHTTP(response, request)
			assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if endpoint == "/api/v1/backup/restore" {
				var report api.BackupRestoreReport
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
				assert.Equal(t, snapshot.ID, report.SnapshotID)
				return
			}
			events := decodeBackupRestoreEvents(t, response.Body.String())
			require.NotEmpty(t, events)
			assert.Equal(t, "result", events[len(events)-1].Type)
		})
	}
}
