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

func TestReadFixtureFileBoundsOversizeAndMissingFiles(t *testing.T) {
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
