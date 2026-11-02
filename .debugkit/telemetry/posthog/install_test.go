package posthog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadOrCreateInstallKeepsTheFirstInstall(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateInstall(dir)
	require.NoError(t, err)
	again, err := LoadOrCreateInstall(dir)
	require.NoError(t, err)

	assert.NotEmpty(t, first.ID)
	assert.False(t, first.InstalledAt.IsZero())
	assert.Equal(t, first.ID, again.ID)
	assert.True(t, first.InstalledAt.Equal(again.InstalledAt))
}

func TestLoadOrCreateInstallSharesOneInstallAcrossConcurrentCallers(t *testing.T) {
	for name, content := range map[string]string{"absent": "", "garbage": "not json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if content != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, InstallFileName), []byte(content), 0o600))
			}
			assertOneInstall(t, dir)
		})
	}
}

func assertOneInstall(t *testing.T, dir string) {
	t.Helper()
	ids := make([]string, 16)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			inst, err := LoadOrCreateInstall(dir)
			assert.NoError(t, err)
			ids[i] = inst.ID
		})
	}
	wg.Wait()

	for _, id := range ids {
		assert.Equal(t, ids[0], id)
	}
}

func TestLoadOrCreateInstallReplacesAFileThatDoesNotParse(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":  "not json",
		"empty_id": `{"install_id":" ","installed_at":"2026-01-02T03:04:05Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, InstallFileName)
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			inst, err := LoadOrCreateInstall(dir)
			require.NoError(t, err)
			again, err := LoadOrCreateInstall(dir)
			require.NoError(t, err)

			assert.NotEmpty(t, inst.ID)
			assert.Equal(t, inst.ID, again.ID)
		})
	}
}
