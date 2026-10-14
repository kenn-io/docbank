package filepublish

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublishHonorsNoReplaceAndReportsPostInstallDurability(t *testing.T) {
	dir := t.TempDir()
	failure := errors.New("synthetic directory sync failure")
	publish := func(name, value string, overwrite, failSync bool) (bool, error) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
		if failSync {
			previous := syncDirectory
			syncDirectory = func(string) error { return failure }
			defer func() { syncDirectory = previous }()
		}
		return Publish(path, filepath.Join(dir, "out.pdf"), overwrite)
	}
	published, err := publish("first.tmp", "first", false, false)
	require.NoError(t, err)
	require.True(t, published)
	published, err = publish("second.tmp", "second", false, false)
	require.ErrorIs(t, err, os.ErrExist)
	require.False(t, published)
	got, err := os.ReadFile(filepath.Join(dir, "out.pdf"))
	require.NoError(t, err)
	require.Equal(t, "first", string(got))
	published, err = publish("replace.tmp", "replacement", true, true)
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "directory sync")
	require.True(t, published)
	got, err = os.ReadFile(filepath.Join(dir, "out.pdf"))
	require.NoError(t, err)
	require.Equal(t, "replacement", string(got))
}

func TestRootStageRejectsChangedDestinationIdentity(t *testing.T) {
	for _, method := range []string{"public Publish rejects replaced root", "rejects parent replaced after opening (skips on Windows: opened directory pinned)"} {
		t.Run(method, func(t *testing.T) {
			destination := t.TempDir()
			rootPath := destination
			public := method == "public Publish rejects replaced root"
			junction := func(path, target string) {
				t.Helper()
				out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Clean(filepath.FromSlash(path)), filepath.Clean(filepath.FromSlash(target))).CombinedOutput()
				require.NoError(t, err, string(out))
			}
			if runtime.GOOS == "windows" && public {
				rootPath = filepath.Join(t.TempDir(), "checkout")
				junction(rootPath, destination)
				defer func() { require.NoError(t, os.Remove(rootPath)) }()
			}
			root, err := os.OpenRoot(rootPath)
			require.NoError(t, err)
			defer func() { _ = root.Close() }()
			dir := root.Name()
			stage, err := CreateRootStage(root)
			require.NoError(t, err)
			defer func() { require.NoError(t, stage.Cleanup()) }()
			_, err = stage.File.WriteString("verified")
			require.NoError(t, err)
			require.NoError(t, stage.File.Close())
			stage.File = nil
			relativePath := "photo.jpg"
			parent := root
			if !public {
				relativePath = filepath.Join("date", "asset", "photo.jpg")
				require.NoError(t, stage.MakeParentDirectories(relativePath))
				parent, err = root.OpenRoot(filepath.Dir(relativePath))
				require.NoError(t, err)
				defer func() { _ = parent.Close() }()
			}
			replacedPath := filepath.Join(dir, filepath.Dir(relativePath))
			moved := filepath.Join(t.TempDir(), "moved")
			if runtime.GOOS == "windows" && public {
				require.NoError(t, os.Remove(replacedPath))
				junction(replacedPath, t.TempDir())
				moved = destination
			} else {
				if err := os.Rename(replacedPath, moved); err != nil {
					if runtime.GOOS == "windows" {
						t.Skip("Windows pins an opened directory against rename")
					}
					require.NoError(t, err)
				}
				require.NoError(t, os.Mkdir(replacedPath, 0o755))
			}
			var installed bool
			if !public {
				// Model a rename between Publish opening the parent and calling publish.
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
		})
	}
}

func TestRootStageSyncsCreatedParentsInOrder(t *testing.T) {
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
	relativePath := filepath.Join("2024", "01", "02", "asset", "photo.jpg")
	err = stage.MakeParentDirectories(relativePath)
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
}

func TestRootStageSyncFailureStopsParentCreation(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	stage, err := CreateRootStage(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	failure := errors.New("directory sync failed")
	stage.syncDirectory = func(*os.Root, string) error { return failure }
	err = stage.MakeParentDirectories(filepath.Join("2024", "01", "02", "asset", "photo.jpg"))
	require.ErrorIs(t, err, failure)
	_, err = root.Stat(filepath.Join("2024", "01"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRootStageHonorsNoReplaceAndReportsPostInstallDurability(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	relativePath := "photo.jpg"
	require.NoError(t, root.WriteFile(relativePath, []byte("verified"), 0o600))
	failure := errors.New("directory sync failed")
	stage, err := CreateRootStage(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, stage.Cleanup()) }()
	_, err = stage.File.WriteString("replacement")
	require.NoError(t, err)
	require.NoError(t, stage.File.Close())
	stage.File = nil
	installed, err := stage.Publish(relativePath)
	if runtime.GOOS == "windows" {
		var status interface{ Errno() syscall.Errno }
		require.ErrorAs(t, err, &status)
		const errorAlreadyExists, errorFileExists syscall.Errno = 183, 80
		require.Contains(t, []syscall.Errno{errorAlreadyExists, errorFileExists}, status.Errno())
		err = status.Errno()
	}
	require.ErrorIs(t, err, os.ErrExist)
	require.False(t, installed)
	content, err := root.ReadFile(relativePath)
	require.NoError(t, err)
	require.Equal(t, "verified", string(content))
	stage.syncDirectory = func(*os.Root, string) error { return failure }
	durablePath := filepath.Join(filepath.Dir(relativePath), "durable.jpg")
	installed, err = stage.Publish(durablePath)
	require.ErrorIs(t, err, failure)
	require.True(t, installed)
	content, err = root.ReadFile(durablePath)
	require.NoError(t, err)
	require.Equal(t, "replacement", string(content))
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

func TestRootStageCleanupPreservesDecoysAndUserFiles(t *testing.T) {
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
}
