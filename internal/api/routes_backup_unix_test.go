//go:build unix

package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/home"
)

func TestBackupRestoreRejectsSymlinkedVaultAndRepositoryAliases(t *testing.T) {
	repoPath := filepath.Join(t.TempDir(), "repo")
	ts, live := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	aliases := t.TempDir()
	repoAlias := filepath.Join(aliases, "repo-alias")
	liveAlias := filepath.Join(aliases, "live-alias")
	require.NoError(t, os.Symlink(repoPath, repoAlias))
	require.NoError(t, os.Symlink(filepath.Dir(live.BlobsDir), liveAlias))
	for _, target := range []string{
		repoAlias,
		filepath.Join(repoAlias, "missing-child"),
		liveAlias,
		filepath.Join(liveAlias, "missing-child"),
	} {
		resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore", nil,
			map[string]any{"target": target, "overwrite": true})
		assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
		assert.Contains(t, body, `"code":"validation"`)
	}

	activeTarget := filepath.Join(t.TempDir(), "active-target")
	require.NoError(t, os.MkdirAll(activeTarget, 0o700))
	lock, err := (home.Layout{Root: activeTarget}).TryLockExclusive()
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore", nil,
		map[string]any{"target": activeTarget, "overwrite": true})
	assert.Equal(t, http.StatusConflict, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"backup_restore_target_active"`)
}
