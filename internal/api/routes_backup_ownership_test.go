package api_test

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/kit/backup"
)

func TestBackupRepositoryInitAndClaimOwnership(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	first, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	second, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	response, body := do(t, first, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, second, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": true})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "another vault")
	require.Contains(t, body, `"code":"backup_repository_owner_conflict"`)
	response, body = do(t, first, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, second, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	// Model a repository written before owner markers existed.
	require.NoError(t, os.Remove(filepath.Join(path, "docbank-owner")))
	response, body = do(t, first, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	require.Contains(t, body, "backup claim")
	require.Contains(t, body, `"code":"backup_repository_claim_required"`)
	var identity string
	for _, confirmed := range []bool{false, true} {
		response, body = do(t, first, http.MethodPost, "/api/v1/backup/claim", nil, map[string]any{"confirm": confirmed, "preview": identity})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var preview backupapp.RepositoryClaim
		require.NoError(t, json.Unmarshal([]byte(body), &preview))
		require.Equal(t, confirmed, preview.Owned)
		require.Equal(t, 1, preview.SnapshotCount)
		require.NotEmpty(t, preview.ID)
		require.NotEmpty(t, preview.NewestAt)
		identity = preview.Preview
	}
	response, body = do(t, first, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	repo, err := backup.Open(path)
	require.NoError(t, err)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 2)
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

func TestBackupRepositoryClaimRejectsChangedHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	server, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	response, body := do(t, server, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	for range 2 {
		response, body = do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
	}
	require.NoError(t, os.Remove(filepath.Join(path, "docbank-owner")))
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

func TestBackupConcurrentDaemonAndEmbeddedFirstBinding(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "repo")
	repo, err := backup.Init(path)
	require.NoError(t, err)
	server, catalog := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = path })
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	embeddedRepo, err := docbank.OpenBackupRepository(path)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, server.URL+"/api/v1/backup/snapshots", strings.NewReader(`{"tag":"daemon"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", testAPIKey)
	request.RemoteAddr = "127.0.0.1:12345"
	start, reply, captured := make(chan struct{}), make(chan int, 1), make(chan error, 1)
	go func() {
		<-start
		response := httptest.NewRecorder()
		catalog.Server.Handler().ServeHTTP(response, request)
		reply <- response.Code
	}()
	go func() {
		<-start
		_, err := vault.CreateBackup(t.Context(), embeddedRepo, docbank.BackupOptions{Tag: "embedded"})
		captured <- err
	}()
	close(start)
	status, err := <-reply, <-captured
	require.Contains(t, []int{http.StatusOK, http.StatusConflict}, status)
	require.True(t, err == nil || errors.Is(err, backup.ErrRepoLocked) || errors.Is(err, backupapp.ErrRepositoryOwnerConflict), "%v", err)
	preview, err := backupapp.ClaimRepository(t.Context(), repo, catalog.VaultID(), false, "")
	winner := "daemon"
	if errors.Is(err, backupapp.ErrRepositoryOwnerConflict) {
		winner = "embedded"
		_, err = vault.CreateBackup(t.Context(), embeddedRepo, docbank.BackupOptions{Tag: winner})
		require.NoError(t, err)
	} else {
		require.NoError(t, err)
		require.True(t, preview.Owned)
		response, body := do(t, server, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{"tag": winner})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
	}
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.NotEmpty(t, points)
	for _, point := range points {
		require.Equal(t, winner, point.Options.Tag)
	}
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
