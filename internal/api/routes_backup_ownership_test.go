package api_test

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func TestBackupRepositoryClaimMovesCaptureBetweenInstallations(t *testing.T) {
	t.Parallel()
	var path string
	server, _ := newTestServer(t, func(d *api.Deps) {
		path = filepath.Join(t.TempDir(), "snapshots")
		d.Cfg.Backup.Repo = path
	})
	response, body := do(t, server, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	_, err := os.Stat(filepath.Join(path, "docbank-owner"))
	require.NoError(t, err)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)

	// A restored copy of the vault is a new installation. Its claim moves
	// capture access away from the original daemon.
	repo, err := backup.Open(path)
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	restoredBlobs, err := blob.New(store.NewPackCatalog(restored), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restoredBlobs.Close()) })
	takeover, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, restored)
	require.NoError(t, err)
	require.True(t, takeover.Takeover)
	_, err = backupapp.ClaimRepository(t.Context(), repo, restored, takeover.Fingerprint)
	require.NoError(t, err)

	response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"backup_repository_claim_required"`)

	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var preview backupapp.RepositoryClaim
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.False(t, preview.Owned)
	require.True(t, preview.Takeover)
	require.Equal(t, 1, preview.SnapshotCount)

	// The restored copy keeps capturing, so the reviewed history is stale.
	_, err = backupapp.Create(t.Context(), repo, "test", restored, restoredBlobs, backup.CreateOptions{})
	require.NoError(t, err)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"fingerprint": preview.Fingerprint})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"backup_claim_changed"`)

	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.Equal(t, 2, preview.SnapshotCount)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"fingerprint": preview.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.Contains(t, body, `"owned":true`)

	response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	_, err = backupapp.Create(t.Context(), repo, "test", restored, restoredBlobs, backup.CreateOptions{})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
}
