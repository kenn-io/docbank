//go:build unix

package api_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
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
}

func TestBackupRestoreRejectsCaseEquivalentRepositoryAlias(t *testing.T) {
	parent := t.TempDir()
	repoPath := filepath.Join(parent, "CaseSensitiveRepo")
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = repoPath })
	resp, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)

	repoAlias := filepath.Join(parent, "casesensitiverepo")
	repoInfo, err := os.Stat(repoPath)
	require.NoError(t, err)
	aliasInfo, err := os.Stat(repoAlias)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("filesystem is case-sensitive")
	}
	require.NoError(t, err)
	if !os.SameFile(repoInfo, aliasInfo) {
		t.Skip("case variant does not identify the repository directory")
	}

	resp, body = do(t, ts, http.MethodPost, "/api/v1/backup/restore", nil,
		map[string]any{"target": filepath.Join(repoAlias, "nested-target"), "overwrite": true})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"validation"`)
}
