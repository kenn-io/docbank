//go:build windows

package atomicfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
)

// longDir returns a new directory whose path is past MAX_PATH. os creates it
// through its own long-path handling.
func longDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), strings.Repeat("x", 100), strings.Repeat("y", 100), strings.Repeat("z", 100))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.Greater(t, len(dir), 260)
	return dir
}

// Paths that os accepts past MAX_PATH work in atomicfile too, even where the
// system long-path setting is off.
func TestLongPaths(t *testing.T) {
	t.Run("WriteFile", func(t *testing.T) {
		path := filepath.Join(longDir(t), "file")
		require.NoError(t, atomicfile.WriteFile(path, []byte("one")))
		require.NoError(t, atomicfile.WriteFile(path, []byte("two")))
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "two", string(got))
	})
	t.Run("WriteNew", func(t *testing.T) {
		path := filepath.Join(longDir(t), "file")
		require.NoError(t, atomicfile.WriteNew(path, []byte("one")))
		require.ErrorIs(t, atomicfile.WriteNew(path, []byte("two")), fs.ErrExist)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "one", string(got))
	})
	t.Run("RenameNoReplace", func(t *testing.T) {
		dir := longDir(t)
		from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
		require.NoError(t, os.WriteFile(from, []byte("data"), 0o600))
		require.NoError(t, atomicfile.RenameNoReplace(from, to))
		got, err := os.ReadFile(to)
		require.NoError(t, err)
		assert.Equal(t, "data", string(got))
	})
	t.Run("Replace", func(t *testing.T) {
		dir := longDir(t)
		from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
		require.NoError(t, os.WriteFile(from, []byte("new"), 0o600))
		require.NoError(t, os.WriteFile(to, []byte("old"), 0o600))
		require.NoError(t, atomicfile.Replace(from, to))
		got, err := os.ReadFile(to)
		require.NoError(t, err)
		assert.Equal(t, "new", string(got))
	})
}
