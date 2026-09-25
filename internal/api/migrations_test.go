package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/fotobanktest"
	"go.kenn.io/docbank/internal/store"
)

func migrationFixtureRequest(t *testing.T) api.FotobankInventoryRequest {
	t.Helper()
	fixture, err := fotobanktest.CreateInstall(filepath.Join(t.TempDir(), "source"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	fixture.OwnerMapPath = filepath.Join(t.TempDir(), "owner-map.json")
	return api.FotobankInventoryRequest{CatalogPath: fixture.CatalogPath, VaultRoot: fixture.VaultRoot, OwnerMapPath: fixture.OwnerMapPath}
}

func TestFotobankInventoryOperatorRoute(t *testing.T) {
	request := migrationFixtureRequest(t)
	ts, server := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var run api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(body), &run))
	require.NotEmpty(t, run.ID)
	require.Equal(t, request.OwnerMapPath, run.OwnerMapPath)
	require.Equal(t, int64(1), run.Report.Counts.Owners)

	unauthorized, unauthorizedBody := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", map[string]string{"X-Api-Key": ""}, request)
	require.Equal(t, http.StatusUnauthorized, unauthorized.StatusCode, unauthorizedBody)

	browser, browserBody := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", map[string]string{
		"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts),
	}, request)
	require.Equal(t, http.StatusForbidden, browser.StatusCode, browserBody)
	require.Contains(t, browserBody, `"code":"web_session_read_only"`)

	raw, err := json.Marshal(request)
	require.NoError(t, err)
	remoteRequest := httptest.NewRequest(http.MethodPost, "/api/v1/migrations/fotobank/inventories", bytes.NewReader(raw))
	remoteRequest.Header.Set("Content-Type", "application/json")
	remoteRequest.Header.Set("X-Api-Key", testAPIKey)
	remoteRequest.RemoteAddr = "192.0.2.1:4444"
	remote := httptest.NewRecorder()
	server.Server.Handler().ServeHTTP(remote, remoteRequest)
	require.Equal(t, http.StatusForbidden, remote.Code)
	require.Contains(t, remote.Body.String(), `"code":"loopback_only"`)

	lock := flock.New(request.CatalogPath + ".server.lock")
	ok, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, ok)
	t.Cleanup(func() { _ = lock.Unlock() })
	locked, lockedBody := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, locked.StatusCode, lockedBody)
	require.Contains(t, lockedBody, `"code":"fotobank_running"`)
}

func TestMigrationRunRoutes(t *testing.T) {
	request := migrationFixtureRequest(t)
	ts, _ := newTestServer(t, nil)
	created, body := do(t, ts, http.MethodPost, "/api/v1/migrations/fotobank/inventories", nil, request)
	require.Equal(t, http.StatusCreated, created.StatusCode, body)
	var run api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(body), &run))

	listed, listBody := get(t, ts, "/api/v1/migrations/runs?limit=1", nil)
	require.Equal(t, http.StatusOK, listed.StatusCode, listBody)
	var page api.MigrationRunPage
	require.NoError(t, json.Unmarshal([]byte(listBody), &page))
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)

	shown, showBody := get(t, ts, "/api/v1/migrations/runs/"+run.ID, nil)
	require.Equal(t, http.StatusOK, shown.StatusCode, showBody)
	var got api.MigrationRun
	require.NoError(t, json.Unmarshal([]byte(showBody), &got))
	require.Equal(t, run.ID, got.ID)

	missing, missingBody := get(t, ts, "/api/v1/migrations/runs/00000000-0000-4000-8000-000000000099", nil)
	require.Equal(t, http.StatusNotFound, missing.StatusCode, missingBody)
}
