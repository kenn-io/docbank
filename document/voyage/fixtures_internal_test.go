package voyage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media"
)

func TestReadFixtureFileRejectsOversizeBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.fixture")
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(media.MaxBytes+1))
	require.NoError(t, file.Close())

	data, err := readFixtureFile(path, media.MaxBytes)
	require.ErrorIs(t, err, media.ErrTooLarge)
	require.Nil(t, data)
}

func TestReadFixtureFileBoundsGrowthWhileReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.fixture")
	require.NoError(t, os.WriteFile(path, []byte("12345"), 0o600))

	data, err := readFixtureFile(path, 4)
	require.ErrorIs(t, err, media.ErrTooLarge)
	require.Nil(t, data)

	_, err = readFixtureFile(filepath.Join(t.TempDir(), "missing"), 4)
	require.Error(t, err)
	require.NotErrorIs(t, err, media.ErrTooLarge)
}
