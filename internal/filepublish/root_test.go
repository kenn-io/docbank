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
	stage, err := CreateRootStage(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	_, err = stage.File.WriteString("verified")
	require.NoError(t, err)
	require.NoError(t, stage.File.Close())
	stage.File = nil
	installed, err := stage.Publish("photo.jpg")
	require.NoError(t, err)
	require.True(t, installed)
	_, err = root.Stat(stage.name)
	require.ErrorIs(t, err, os.ErrNotExist)
	collision, err := CreateRootStage(root)
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

func TestRootStageSyncFailure(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"parent preparation", "publication"} {
		t.Run(phase, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			stage, err := CreateRootStage(root)
			require.NoError(t, err)
			defer func() { require.NoError(t, stage.Cleanup()) }()
			failure := errors.New("directory sync failed")
			stage.syncDirectory = func(*os.Root, string) error { return failure }
			if phase == "parent preparation" {
				require.ErrorIs(t, stage.MakeParentDirectories(filepath.Join("year", "asset", "photo.jpg")), failure)
				_, err = root.Stat(filepath.Join("year", "asset"))
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			_, err = stage.File.WriteString("verified")
			require.NoError(t, err)
			require.NoError(t, stage.File.Close())
			stage.File = nil
			installed, err := stage.Publish("photo.jpg")
			require.True(t, installed)
			require.ErrorIs(t, err, failure)
			content, err := root.ReadFile("photo.jpg")
			require.NoError(t, err)
			require.Equal(t, "verified", string(content))
		})
	}
}

func TestRootStageRejectsDestinationAfterPathReplacement(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	destination := filepath.Join(base, "destination")
	require.NoError(t, os.Mkdir(destination, 0o755))
	root, err := os.OpenRoot(destination)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	stage, err := CreateRootStage(root)
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
	require.Error(t, err)
	require.False(t, installed)
	for _, dir := range []string{destination, moved} {
		_, err = os.Stat(filepath.Join(dir, "photo.jpg"))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
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
	stage, err := CreateRootStage(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	require.NoError(t, stage.File.Close())
	stage.File = nil
	_, err = stage.Publish("escape/photo.jpg")
	require.Error(t, err)
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRootStageSyncsNewDirectoryParentsBeforePublication(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	stage, err := CreateRootStage(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	var synced []string
	stage.syncDirectory = func(root *os.Root, name string) error {
		synced = append(synced, filepath.Join(root.Name(), name))
		return nil
	}
	destination := filepath.Join("2024", "01", "02", "asset", "photo.jpg")
	require.NoError(t, stage.MakeParentDirectories(destination))
	require.Equal(t, []string{root.Name(), filepath.Join(root.Name(), "2024"), filepath.Join(root.Name(), "2024", "01"), filepath.Join(root.Name(), "2024", "01", "02")}, synced)
	require.NoError(t, stage.MakeParentDirectories(destination))
	require.Len(t, synced, 4)
	require.NoError(t, stage.File.Close())
	stage.File = nil
	installed, err := stage.Publish(destination)
	require.NoError(t, err)
	require.True(t, installed)
	require.Equal(t, filepath.Join(root.Name(), filepath.Dir(destination)), synced[4])
}

func TestRootStageRejectsMovedParentBeforeRename(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	stage, err := CreateRootStage(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	destination := filepath.Join("date", "asset", "photo.jpg")
	require.NoError(t, stage.MakeParentDirectories(destination))
	require.NoError(t, stage.File.Close())
	stage.File = nil
	parent, err := root.OpenRoot(filepath.Dir(destination))
	require.NoError(t, err)
	defer func() { _ = parent.Close() }()
	outside := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(filepath.Join(root.Name(), filepath.Dir(destination)), outside); err != nil {
		t.Skip("platform pins an opened directory against rename")
	}
	require.NoError(t, root.Mkdir(filepath.Dir(destination), 0700))
	installed, err := stage.publish(parent, destination)
	require.ErrorContains(t, err, "checkout destination changed before publication")
	require.False(t, installed)
	_, err = os.Stat(filepath.Join(outside, "photo.jpg"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
