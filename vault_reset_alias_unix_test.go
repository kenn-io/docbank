//go:build !windows

package docbank

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

func TestResetVaultRevalidatesSourceAliasAfterOwnerRelease(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	parked := filepath.Join(base, "vault.parked")
	diagnostic := filepath.Join(base, "vault.reset")
	current, err := New(t.Context(), Config{Root: root, SQLite: modernc.Driver{}})
	require.NoError(t, err)

	fresh, err := ResetVault(
		t.Context(),
		Config{Root: root, SQLite: modernc.Driver{}},
		ResetOptions{
			DiagnosticRoot: diagnostic,
			ReleaseCurrent: func() error {
				if err := current.Close(); err != nil {
					return err
				}
				if err := os.Rename(root, parked); err != nil {
					return err
				}
				return os.Symlink(parked, root)
			},
		},
	)
	if fresh != nil {
		t.Cleanup(func() { _ = fresh.Close() })
	}

	assert.Nil(t, fresh)
	require.ErrorContains(t, err, "symlink")
	info, statErr := os.Lstat(root)
	require.NoError(t, statErr)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	assert.DirExists(t, parked)
	assert.NoDirExists(t, diagnostic)
}
