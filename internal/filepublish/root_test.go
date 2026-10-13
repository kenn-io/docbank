package filepublish

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootStageNoReplaceAndCleanup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"cleanup", "destination root", "parent", "success", "parent failure"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "destination")
			require.NoError(t, os.Mkdir(destination, 0o755))
			root, err := os.OpenRoot(destination)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			stage, err := CreateRootStage(root)
			require.NoError(t, err)
			defer func() { require.NoError(t, stage.Cleanup()) }()
			switch scenario {
			case "cleanup":
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
			case "destination root", "parent":
				_, err = stage.File.WriteString("verified")
				require.NoError(t, err)
				require.NoError(t, stage.File.Close())
				stage.File = nil
				relativePath := "photo.jpg"
				parent := root
				if scenario == "parent" {
					relativePath = filepath.Join("date", "asset", "photo.jpg")
					require.NoError(t, stage.MakeParentDirectories(relativePath))
					parent, err = root.OpenRoot(filepath.Dir(relativePath))
					require.NoError(t, err)
					defer func() { _ = parent.Close() }()
				}
				replacedPath := filepath.Join(destination, filepath.Dir(relativePath))
				moved := filepath.Join(base, "moved")
				if err := os.Rename(replacedPath, moved); err != nil {
					if runtime.GOOS == "windows" {
						t.Skip("Windows pins an opened directory against rename")
					}
					require.NoError(t, err)
				}
				require.NoError(t, os.Mkdir(replacedPath, 0o755))
				var installed bool
				if scenario == "parent" {
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
					if scenario == "parent failure" {
						return failure
					}
					return nil
				}
				relativePath := filepath.Join("2024", "01", "02", "asset", "photo.jpg")
				err = stage.MakeParentDirectories(relativePath)
				if scenario == "parent failure" {
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
		})
	}
}
