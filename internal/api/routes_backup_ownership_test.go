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
	"go.kenn.io/kit/backup"
)

func TestBackupRepositoryInitAndClaimOwnership(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	server, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	other, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	response, body := do(t, server, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, other, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": true})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, `"code":"backup_repository_owner_conflict"`)
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
	var preview backupapp.RepositoryClaim
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.Equal(t, 2, preview.SnapshotCount)
	repo, err := backup.Open(path)
	require.NoError(t, err)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	// Replace an older point without changing the count or newest timestamp.
	replacement := *points[0]
	replacement.Options.Tag = "replacement"
	_, err = repo.WriteManifest(&replacement)
	require.NoError(t, err)
	_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: []string{points[0].SnapshotID}})
	require.NoError(t, err)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": true, "preview": preview.Preview})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "history changed")
	require.Contains(t, body, `"code":"backup_repository_claim_changed"`)
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
	// A fresh preview can adopt the changed history.
	newest := preview.NewestAt
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.Equal(t, 2, preview.SnapshotCount)
	require.Equal(t, newest, preview.NewestAt)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": true, "preview": preview.Preview})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestBackupRepositoryInitCompletesMissingOwner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	server, catalog := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	// Initialization can stop after Kit publishes its config but before owner binding.
	repo, err := backup.Init(path)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
	response, body := do(t, server, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	owner, err := os.ReadFile(filepath.Join(path, "docbank-owner"))
	require.NoError(t, err)
	require.Equal(t, catalog.VaultID()+"\n", string(owner))
	reopened, err := backup.Open(path)
	require.NoError(t, err)
	require.Equal(t, repo.Config().RepoID, reopened.Config().RepoID)
	require.NoError(t, backupapp.EnsureRepositoryOwner(t.Context(), reopened, catalog.VaultID(), false))
}

func TestBackupRepositoryClaimRequiresOperator(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	_, err := backup.Init(path)
	require.NoError(t, err)
	server, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	response, body := do(t, server, http.MethodPost, "/api/daemon/web-session", nil, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}, map[string]any{"confirm": true})
	require.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var preview backupapp.RepositoryClaim
	require.NoError(t, json.Unmarshal([]byte(body), &preview))
	require.False(t, preview.Owned)
}

func TestBackupRepositoryClaimRejectsVaultOverlap(t *testing.T) {
	t.Parallel()
	var path string
	server, _ := newTestServer(t, func(d *api.Deps) {
		path = filepath.Join(d.VaultRoot, "nested-repository")
		d.Cfg.Backup.Repo = path
	})
	_, err := backup.Init(path)
	require.NoError(t, err)
	response, body := do(t, server, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": true})
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	_, err = os.Stat(filepath.Join(path, "docbank-owner"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestBackupRepositoryMalformedOwnerProblem(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	server, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	_, err := backup.Init(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(path, "docbank-owner"), []byte("damaged"), 0o600))
	for _, route := range []string{"/api/v1/backup/snapshots", "/api/v1/backup/claim"} {
		response, body := do(t, server, http.MethodPost, route, nil, map[string]any{})
		require.Equal(t, http.StatusConflict, response.StatusCode, body)
		require.Contains(t, body, `"code":"backup_repository_owner_invalid"`)
		require.Contains(t, body, "owner marker is malformed or unsupported")
	}
}
