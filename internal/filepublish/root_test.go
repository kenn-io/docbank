package filepublish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootStageNoReplace(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	stage, err := CreateRootStage(root, ".")
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	_, err = stage.File.WriteString("verified")
	require.NoError(t, err)
	require.NoError(t, stage.File.Close())
	stage.File = nil
	installed, err := stage.Publish("photo.jpg")
	require.NoError(t, err)
	require.True(t, installed)
	installed, err = stage.Publish("photo.jpg")
	require.Error(t, err)
	require.False(t, installed)
	content, err := root.ReadFile("photo.jpg")
	require.NoError(t, err)
	require.Equal(t, "verified", string(content))
}

func TestRootStageRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skip("symlink creation unavailable")
	}
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	_, err = CreateRootStage(root, "escape")
	require.Error(t, err)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries)
}
