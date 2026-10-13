package filepublish

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishHonorsNoReplaceAndReportsPostInstallDurability(t *testing.T) {
	for _, method := range []string{"path", "root"} {
		t.Run(method, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			failure := errors.New("synthetic directory sync failure")
			publish := func(name, value, destination string, overwrite, failSync bool) (bool, error) {
				if method == "root" {
					stage, err := CreateRootStage(root)
					require.NoError(t, err)
					defer func() { require.NoError(t, stage.Cleanup()) }()
					_, err = stage.File.WriteString(value)
					require.NoError(t, err)
					require.NoError(t, stage.File.Close())
					stage.File = nil
					if failSync {
						stage.syncDirectory = func(*os.Root, string) error { return failure }
					}
					published, err := stage.Publish(destination)
					if !published {
						require.NoError(t, stage.Cleanup())
					}
					_, statErr := root.Stat(stage.name)
					require.ErrorIs(t, statErr, os.ErrNotExist)
					return published, err
				}
				path := filepath.Join(dir, name)
				require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
				if failSync {
					previous := syncDirectory
					syncDirectory = func(string) error { return failure }
					defer func() { syncDirectory = previous }()
				}
				return Publish(path, filepath.Join(dir, destination), overwrite)
			}
			published, err := publish("first.tmp", "first", "out.pdf", false, false)
			require.NoError(t, err)
			require.True(t, published)
			published, err = publish("second.tmp", "second", "out.pdf", false, false)
			require.Error(t, err)
			require.False(t, published)
			got, err := root.ReadFile("out.pdf")
			require.NoError(t, err)
			require.Equal(t, "first", string(got))
			destination := "out.pdf"
			if method == "root" {
				destination = "durable.pdf"
			}
			published, err = publish("replace.tmp", "replacement", destination, true, true)
			require.ErrorContains(t, err, "directory sync")
			require.True(t, published)
			got, err = root.ReadFile(destination)
			require.NoError(t, err)
			require.Equal(t, "replacement", string(got))
		})
	}
}

func TestCreateStageRemovesOnlyStaleAbandonedStages(t *testing.T) {
	parent := t.TempDir()
	stale := time.Now().Add(-2 * staleStageAge)
	abandoned, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	require.NoError(t, abandoned.File.Close())
	if abandoned.pin != nil {
		// A crashed process no longer holds its Windows directory pin.
		require.NoError(t, abandoned.pin.Close())
	}
	require.NoError(t, os.Chtimes(abandoned.dir, stale, stale))
	live, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Cleanup() })
	userDir := filepath.Join(parent, ".docbank-test-user")
	require.NoError(t, os.Mkdir(userDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(userDir, "notes.txt"), []byte("keep"), 0o600))
	require.NoError(t, os.Chtimes(userDir, stale, stale))
	emptyUserDir := filepath.Join(parent, ".docbank-test-empty")
	require.NoError(t, os.Mkdir(emptyUserDir, 0o700))
	require.NoError(t, os.Chtimes(emptyUserDir, stale, stale))

	next, err := CreateStage(parent, ".docbank-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = next.Cleanup() })

	require.NoDirExists(t, abandoned.dir)
	require.DirExists(t, live.dir)
	require.FileExists(t, filepath.Join(userDir, "notes.txt"))
	require.DirExists(t, emptyUserDir, "an old empty user folder sharing the prefix must survive")
}
