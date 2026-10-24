package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestBackupRepositoryInitAndClaimOwnership(t *testing.T) {
	t.Parallel()
	var path string
	server, catalog := newTestServer(t, func(d *api.Deps) {
		path = filepath.Join(d.VaultRoot, "snapshots")
		d.Cfg.Backup.Repo = path
	})
	response, body := do(t, server, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	owner, err := os.ReadFile(filepath.Join(path, "docbank-owner"))
	require.NoError(t, err)
	require.Equal(t, catalog.VaultID()+"\n", string(owner))
	for range 2 {
		response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
	}
	require.NoError(t, os.Remove(filepath.Join(path, "docbank-owner")))
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"backup_repository_claim_required"`)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": true})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}
