package filepublish

import (
	"errors"
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
	_, err = stage.stage.Stat("payload.tmp")
	require.ErrorIs(t, err, os.ErrNotExist)
	collision, err := CreateRootStage(root, ".")
	require.NoError(t, err)
	defer func() { require.NoError(t, collision.Cleanup()) }()
	_, err = collision.File.WriteString("replacement")
	require.NoError(t, err)
	require.NoError(t, collision.File.Close())
	collision.File = nil
	installed, err = collision.Publish("photo.jpg")
	require.Error(t, err)
	require.False(t, installed)
	content, err := root.ReadFile("photo.jpg")
	require.NoError(t, err)
	require.Equal(t, "verified", string(content))
}

func TestRootStageSyncFailureReportsInstalled(t *testing.T) {
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
	failure := errors.New("directory sync failed")
	stage.syncDirectory = func(*os.Root, string) error { return failure }
	installed, err := stage.Publish("photo.jpg")
	require.True(t, installed)
	require.ErrorIs(t, err, failure)
	content, err := root.ReadFile("photo.jpg")
	require.NoError(t, err)
	require.Equal(t, "verified", string(content))
}

func TestRootStageHoldsDestinationAfterPathReplacement(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	destination := filepath.Join(base, "destination")
	require.NoError(t, os.Mkdir(destination, 0o755))
	root, err := os.OpenRoot(destination)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	stage, err := CreateRootStage(root, ".")
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	_, err = stage.File.WriteString("verified")
	require.NoError(t, err)
	require.NoError(t, stage.File.Close())
	stage.File = nil
	moved := filepath.Join(base, "moved")
	if err := os.Rename(destination, moved); err != nil {
		// Windows can pin the destination against pathname replacement.
		installed, publishErr := stage.Publish("photo.jpg")
		require.NoError(t, publishErr)
		require.True(t, installed)
		return
	}
	require.NoError(t, os.Mkdir(destination, 0o755))
	installed, err := stage.Publish("photo.jpg")
	require.NoError(t, err)
	require.True(t, installed)
	content, err := os.ReadFile(filepath.Join(moved, "photo.jpg"))
	require.NoError(t, err)
	require.Equal(t, "verified", string(content))
	_, err = os.Stat(filepath.Join(destination, "photo.jpg"))
	require.ErrorIs(t, err, os.ErrNotExist)
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
