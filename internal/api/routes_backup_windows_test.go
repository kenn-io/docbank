//go:build windows

package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestBackupRestoreRejectsWindowsCaseEquivalentRepositoryAlias(t *testing.T) {
	parent := t.TempDir()
	repoPath := filepath.Join(parent, "CaseSensitiveRepo")
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	repoAlias := strings.ToLower(repoPath)
	repoInfo, err := os.Stat(repoPath)
	require.NoError(t, err)
	aliasInfo, err := os.Stat(repoAlias)
	require.NoError(t, err)
	require.True(t, os.SameFile(repoInfo, aliasInfo))

	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore", nil,
		map[string]any{"target": filepath.Join(repoAlias, "nested-target"), "overwrite": true})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
}

func TestBackupRestoreRejectsWindowsRepositoryReparseAlias(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	alias := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(repoPath, alias); err != nil {
		t.Skipf("creating a Windows symlink requires developer mode: %v", err)
	}
	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore", nil,
		map[string]any{"target": filepath.Join(alias, "nested-target"), "overwrite": true})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
}
