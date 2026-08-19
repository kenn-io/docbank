package voyage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
	"go.kenn.io/kit/safefileio"
)

func privateFixtureTestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(directory))
	return directory
}

func TestReadFixtureFileRejectsOversizeBeforeReading(t *testing.T) {
	directory := privateFixtureTestDir(t)
	path := filepath.Join(directory, "oversized.fixture")
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(media.MaxBytes+1))
	require.NoError(t, file.Close())

	root, err := openPrivateFixtureRoot(directory, "test directory")
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	data, err := readFixtureFile(root, "oversized.fixture", media.MaxBytes)
	require.ErrorIs(t, err, media.ErrTooLarge)
	require.Nil(t, data)
}

func TestReadFixtureFileBoundsGrowthWhileReading(t *testing.T) {
	directory := privateFixtureTestDir(t)
	path := filepath.Join(directory, "growing.fixture")
	require.NoError(t, os.WriteFile(path, []byte("12345"), 0o600))
	root, err := openPrivateFixtureRoot(directory, "test directory")
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()

	data, err := readFixtureFile(root, "growing.fixture", 4)
	require.ErrorIs(t, err, media.ErrTooLarge)
	require.Nil(t, data)

	_, err = readFixtureFile(root, "missing", 4)
	require.Error(t, err)
	require.NotErrorIs(t, err, media.ErrTooLarge)
}

func TestPublishFixtureDirectoryDoesNotReplaceDestination(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Mkdir(path, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(path, "keep"), []byte("directory"), 0o600))
			},
		},
		{
			name: "file",
			setup: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("file"), 0o600))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := privateFixtureTestDir(t)
			root, err := openPrivateFixtureRoot(parent, "test parent")
			require.NoError(t, err)
			defer func() { require.NoError(t, root.Close()) }()
			parentIdentity, err := root.Lstat(".")
			require.NoError(t, err)
			stagingName, stagingRoot, identity, err := createPrivateFixtureStagingRoot(root)
			require.NoError(t, err)
			require.NoError(t, stagingRoot.Close())

			destination := filepath.Join(parent, "fixtures")
			tt.setup(t, destination)
			err = publishFixtureDirectory(root, stagingName, "fixtures", parentIdentity, identity)
			require.ErrorContains(t, err, "destination already exists")
			_, err = os.Lstat(destination)
			require.NoError(t, err)
			require.NoError(t, removeFixtureDirectory(root, stagingName, identity))
		})
	}
}

func TestFixtureRenameAtomicallyRefusesExistingDestination(t *testing.T) {
	parent := privateFixtureTestDir(t)
	root, err := openPrivateFixtureRoot(parent, "test parent")
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	parentIdentity, err := root.Lstat(".")
	require.NoError(t, err)
	stagingName, stagingRoot, stagingIdentity, err := createPrivateFixtureStagingRoot(root)
	require.NoError(t, err)
	require.NoError(t, stagingRoot.Close())
	require.NoError(t, root.Mkdir("fixtures", 0o700))

	err = renameFixtureDirectoryNoReplace(parent, stagingName, "fixtures", parentIdentity, stagingIdentity)
	require.Error(t, err)
	currentIdentity, statErr := root.Lstat(stagingName)
	require.NoError(t, statErr)
	require.True(t, os.SameFile(stagingIdentity, currentIdentity))
	require.NoError(t, removeFixtureDirectory(root, stagingName, stagingIdentity))
}

func TestRemoveFixtureDirectoryRefusesReplacement(t *testing.T) {
	parent := privateFixtureTestDir(t)
	root, err := openPrivateFixtureRoot(parent, "test parent")
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()
	stagingName, stagingRoot, identity, err := createPrivateFixtureStagingRoot(root)
	require.NoError(t, err)
	require.NoError(t, stagingRoot.Close())
	require.NoError(t, root.RemoveAll(stagingName))
	require.NoError(t, root.Mkdir(stagingName, 0o700))

	err = removeFixtureDirectory(root, stagingName, identity)
	require.ErrorContains(t, err, "refuse to remove a replaced")
	_, statErr := root.Lstat(stagingName)
	require.NoError(t, statErr)
}

func TestVerifyFixtureRootPathRejectsReplacedParent(t *testing.T) {
	outer := privateFixtureTestDir(t)
	parent := filepath.Join(outer, "parent")
	moved := filepath.Join(outer, "moved")
	require.NoError(t, os.Mkdir(parent, 0o700))
	root, err := openPrivateFixtureRoot(parent, "test parent")
	require.NoError(t, err)
	identity, err := root.Lstat(".")
	require.NoError(t, err)
	require.NoError(t, os.Rename(parent, moved))
	require.NoError(t, os.Mkdir(parent, 0o700))

	err = verifyFixtureRootPath(root, identity, "test parent")
	require.ErrorContains(t, err, "changed during fixture generation")
	require.NoError(t, root.Close())
	require.NoError(t, os.Remove(parent))
	require.NoError(t, os.Rename(moved, parent))

	_, err = os.Lstat(parent)
	require.NoError(t, err)
}
