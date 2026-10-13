package filepublish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootStageNoReplaceAndCleanup(t *testing.T) {
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
	decoy := rootStagePrefix + strings.Repeat("c", 32)
	wrongName := rootStagePrefix + "user-file"
	require.NoError(t, root.Mkdir(decoy, 0700))
	require.NoError(t, root.WriteFile(filepath.Join(decoy, "my-edit.jpg"), []byte("keep"), 0600))
	require.NoError(t, root.WriteFile(wrongName, []byte("keep"), 0600))
	require.NoError(t, CleanupRootStages(root))
	_, err = root.Stat(collision.name)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, name := range []string{decoy, wrongName} {
		_, err = root.Stat(name)
		require.NoError(t, err)
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
		t.Skip("platform pins an opened directory against rename")
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

func TestRootStageSyncsNewDirectoryParentsBeforePublication(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"success", "parent failure", "publication failure"} {
		t.Run(phase, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			stage, err := CreateRootStage(root)
			require.NoError(t, err)
			defer func() { require.NoError(t, stage.Cleanup()) }()
			var synced []string
			failure := errors.New("directory sync failed")
			publishing := false
			stage.syncDirectory = func(root *os.Root, name string) error {
				synced = append(synced, filepath.Join(root.Name(), name))
				if phase == "parent failure" || phase == "publication failure" && publishing {
					return failure
				}
				return nil
			}
			destination := filepath.Join("2024", "01", "02", "asset", "photo.jpg")
			err = stage.MakeParentDirectories(destination)
			if phase == "parent failure" {
				require.ErrorIs(t, err, failure)
				_, err = root.Stat(filepath.Join("2024", "01"))
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{root.Name(), filepath.Join(root.Name(), "2024"), filepath.Join(root.Name(), "2024", "01"), filepath.Join(root.Name(), "2024", "01", "02")}, synced)
			require.NoError(t, stage.MakeParentDirectories(destination))
			require.Len(t, synced, 4)
			_, err = stage.File.WriteString("verified")
			require.NoError(t, err)
			require.NoError(t, stage.File.Close())
			stage.File = nil
			publishing = true
			installed, err := stage.Publish(destination)
			require.True(t, installed)
			if phase == "publication failure" {
				require.ErrorIs(t, err, failure)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, filepath.Join(root.Name(), filepath.Dir(destination)), synced[4])
			content, err := root.ReadFile(destination)
			require.NoError(t, err)
			require.Equal(t, "verified", string(content))
		})
	}
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
