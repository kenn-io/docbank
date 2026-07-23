//go:build windows

package docbank

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/pkg/sqlite/modernc"
	"golang.org/x/sys/windows"
)

func TestResetVaultRejectsWindowsDirectoryReparsePoint(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-vault")
	vault, err := New(t.Context(), Config{Root: realRoot, SQLite: modernc.Driver{}})
	require.NoError(t, err)
	require.NoError(t, vault.Close())

	alias := filepath.Join(base, "vault-alias")
	require.NoError(t, createWindowsJunction(alias, realRoot))
	attributes, err := resetSourceAttributesNoFollow(alias)
	require.NoError(t, err)
	require.True(t, attributes.directory)
	require.True(t, attributes.reparse)

	diagnostic := filepath.Join(base, "vault.reset")
	fresh, err := ResetVault(
		t.Context(),
		Config{Root: alias, SQLite: modernc.Driver{}},
		ResetOptions{DiagnosticRoot: diagnostic},
	)
	if fresh != nil {
		t.Cleanup(func() { _ = fresh.Close() })
	}

	assert.Nil(t, fresh)
	require.ErrorContains(t, err, "reparse point")
	assert.DirExists(t, realRoot)
	assert.NoDirExists(t, diagnostic)
}

func TestResetVaultRejectsWindowsDirectoryReparsePointAfterOwnerRelease(t *testing.T) {
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
				return createWindowsJunction(root, parked)
			},
		},
	)
	if fresh != nil {
		t.Cleanup(func() { _ = fresh.Close() })
	}

	assert.Nil(t, fresh)
	require.ErrorContains(t, err, "reparse point")
	attributes, inspectErr := resetSourceAttributesNoFollow(root)
	require.NoError(t, inspectErr)
	assert.True(t, attributes.directory)
	assert.True(t, attributes.reparse)
	assert.DirExists(t, parked)
	assert.NoDirExists(t, diagnostic)
}

func createWindowsJunction(alias, target string) error {
	output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", alias, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating directory junction: %w: %s", err, output)
	}
	return nil
}

func TestRenameVaultNoReplaceSupportsWindowsExtendedLengthPaths(t *testing.T) {
	parent := t.TempDir()
	for len(parent) < 280 {
		parent = filepath.Join(parent, strings.Repeat("segment", 8))
	}
	require.NoError(t, os.MkdirAll(parent, 0o700))
	source := filepath.Join(parent, "vault")
	destination := filepath.Join(parent, "vault.reset")
	require.NoError(t, os.Mkdir(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "sentinel"), []byte("kept"), 0o600))

	require.NoError(t, renameVaultNoReplace(source, destination))

	assert.NoDirExists(t, source)
	assert.Equal(t, []byte("kept"), mustReadResetFile(t, filepath.Join(destination, "sentinel")))
}

func TestRenameVaultNoReplacePassesExtendedPathsToMoveFileW(t *testing.T) {
	for name, paths := range map[string][4]string{
		"dos": {
			`C:\vault`,
			`C:\vault.reset`,
			`\\?\C:\vault`,
			`\\?\C:\vault.reset`,
		},
		"unc": {
			`\\server\share\vault`,
			`\\server\share\vault.reset`,
			`\\?\UNC\server\share\vault`,
			`\\?\UNC\server\share\vault.reset`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var movedSource, movedDestination string
			err := renameVaultNoReplaceWithMove(
				paths[0],
				paths[1],
				func(source, destination *uint16) error {
					movedSource = windows.UTF16PtrToString(source)
					movedDestination = windows.UTF16PtrToString(destination)
					return nil
				},
			)

			require.NoError(t, err)
			assert.Equal(t, paths[2], movedSource)
			assert.Equal(t, paths[3], movedDestination)
		})
	}
}
