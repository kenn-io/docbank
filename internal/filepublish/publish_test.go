package filepublish

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishHonorsNoReplaceAndReportsPostInstallDurability(t *testing.T) {
	for _, method := range []string{"path", "root", "destination root", "parent", "parent sync", "parent failure"} {
		t.Run(method, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			if method != "path" && method != "root" {
				stage, err := CreateRootStage(root)
				require.NoError(t, err)
				defer func() { require.NoError(t, stage.Cleanup()) }()
				switch method {
				case "destination root", "parent":
					_, err = stage.File.WriteString("verified")
					require.NoError(t, err)
					require.NoError(t, stage.File.Close())
					stage.File = nil
					relativePath := "photo.jpg"
					parent := root
					if method == "parent" {
						relativePath = filepath.Join("date", "asset", "photo.jpg")
						require.NoError(t, stage.MakeParentDirectories(relativePath))
						parent, err = root.OpenRoot(filepath.Dir(relativePath))
						require.NoError(t, err)
						defer func() { _ = parent.Close() }()
					}
					replacedPath := filepath.Join(dir, filepath.Dir(relativePath))
					moved := filepath.Join(t.TempDir(), "moved")
					if err := os.Rename(replacedPath, moved); err != nil {
						if runtime.GOOS == "windows" {
							t.Skip("Windows pins an opened directory against rename")
						}
						require.NoError(t, err)
					}
					require.NoError(t, os.Mkdir(replacedPath, 0o755))
					var installed bool
					if method == "parent" {
						installed, err = stage.publish(parent, relativePath)
					} else {
						installed, err = stage.Publish(relativePath)
					}
					require.ErrorContains(t, err, "checkout destination changed before publication")
					require.False(t, installed)
					for _, dir := range []string{replacedPath, moved} {
						_, err = os.Stat(filepath.Join(dir, "photo.jpg"))
						require.ErrorIs(t, err, os.ErrNotExist)
					}
				default:
					var synced []string
					failure := errors.New("directory sync failed")
					stage.syncDirectory = func(root *os.Root, name string) error {
						synced = append(synced, filepath.Join(root.Name(), name))
						if method == "parent failure" {
							return failure
						}
						return nil
					}
					relativePath := filepath.Join("2024", "01", "02", "asset", "photo.jpg")
					err = stage.MakeParentDirectories(relativePath)
					if method == "parent failure" {
						require.ErrorIs(t, err, failure)
						_, err = root.Stat(filepath.Join("2024", "01"))
						require.ErrorIs(t, err, os.ErrNotExist)
						return
					}
					require.NoError(t, err)
					require.Equal(t, []string{filepath.Join(root.Name(), "."), filepath.Join(root.Name(), "2024"), filepath.Join(root.Name(), "2024", "01"), filepath.Join(root.Name(), "2024", "01", "02")}, synced)
					require.NoError(t, stage.MakeParentDirectories(relativePath))
					require.Len(t, synced, 4)
					_, err = stage.File.WriteString("verified")
					require.NoError(t, err)
					require.NoError(t, stage.File.Close())
					stage.File = nil
					installed, err := stage.Publish(relativePath)
					require.True(t, installed)
					require.NoError(t, err)
					require.Equal(t, filepath.Join(root.Name(), filepath.Dir(relativePath)), synced[4])
					content, err := root.ReadFile(relativePath)
					require.NoError(t, err)
					require.Equal(t, "verified", string(content))
				}
				return
			}
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
			if method == "path" {
				require.ErrorIs(t, err, os.ErrExist)
			} else {
				require.Error(t, err)
			}
			require.False(t, published)
			got, err := root.ReadFile("out.pdf")
			require.NoError(t, err)
			require.Equal(t, "first", string(got))
			destination := "out.pdf"
			if method == "root" {
				destination = "durable.pdf"
			}
			published, err = publish("replace.tmp", "replacement", destination, true, true)
			require.ErrorIs(t, err, failure)
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
	t.Run("root stages", func(t *testing.T) {
		root, err := os.OpenRoot(t.TempDir())
		require.NoError(t, err)
		defer func() { _ = root.Close() }()
		stage, err := CreateRootStage(root)
		require.NoError(t, err)
		defer func() { require.NoError(t, stage.Cleanup()) }()
		require.NoError(t, stage.Cleanup())
		_, err = root.Stat(stage.name)
		require.ErrorIs(t, err, os.ErrNotExist)
		stage, err = CreateRootStage(root)
		require.NoError(t, err)
		require.NoError(t, stage.File.Close())
		stage.File = nil
		decoy := rootStagePrefix + strings.Repeat("c", 32)
		wrongName := rootStagePrefix + "user-file"
		require.NoError(t, root.Mkdir(decoy, 0700))
		require.NoError(t, root.WriteFile(filepath.Join(decoy, "my-edit.jpg"), []byte("keep"), 0600))
		require.NoError(t, root.WriteFile(wrongName, []byte("keep"), 0600))
		require.NoError(t, CleanupRootStages(root))
		_, err = root.Stat(stage.name)
		require.ErrorIs(t, err, os.ErrNotExist)
		for _, name := range []string{decoy, wrongName} {
			_, err = root.Stat(name)
			require.NoError(t, err)
		}
	})
}
