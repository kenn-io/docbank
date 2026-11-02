//go:build unix || windows

package atomicfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
)

func TestReplace(t *testing.T) {
	t.Run("replaces an existing file", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "new")
		dst := writeString(t, dir, "dst", "old")

		require.NoError(t, atomicfile.Replace(src, dst))

		assert.Equal(t, "new", readString(t, dst))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("renames to a free name", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "data")
		dst := filepath.Join(dir, "dst")

		require.NoError(t, atomicfile.Replace(src, dst))

		assert.Equal(t, "data", readString(t, dst))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("renames a read-only file", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		require.NoError(t, os.WriteFile(src, []byte("new"), 0o400))
		dst := writeString(t, dir, "dst", "old")

		require.NoError(t, atomicfile.Replace(src, dst))

		assert.Equal(t, "new", readString(t, dst))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("fails for a missing source", func(t *testing.T) {
		dir := t.TempDir()
		dst := writeString(t, dir, "dst", "old")

		err := atomicfile.Replace(filepath.Join(dir, "missing"), dst)

		require.ErrorIs(t, err, fs.ErrNotExist)
		var linkErr *os.LinkError
		require.ErrorAs(t, err, &linkErr)
		assert.Equal(t, "old", readString(t, dst))
	})
	t.Run("refuses to replace a directory with a file", func(t *testing.T) {
		for _, name := range []string{"empty", "full"} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				src := writeString(t, dir, "src", "new")
				dst := filepath.Join(dir, "dst")
				require.NoError(t, os.Mkdir(dst, 0o700))
				if name == "full" {
					writeString(t, dst, "inner", "inner")
				}

				require.ErrorIs(t, atomicfile.Replace(src, dst), fs.ErrExist)

				assert.Equal(t, "new", readString(t, src))
				info, err := os.Lstat(dst)
				require.NoError(t, err)
				assert.True(t, info.IsDir())
			})
		}
	})
	t.Run("renames a directory to a free name", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src")
		require.NoError(t, os.Mkdir(src, 0o700))
		writeString(t, src, "inner", "inner")
		dst := filepath.Join(dir, "dst")

		require.NoError(t, atomicfile.Replace(src, dst))

		assert.Equal(t, "inner", readString(t, filepath.Join(dst, "inner")))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("refuses to replace a directory with a directory", func(t *testing.T) {
		for _, name := range []string{"empty", "full"} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				src := filepath.Join(dir, "src")
				require.NoError(t, os.Mkdir(src, 0o700))
				writeString(t, src, "inner", "new")
				dst := filepath.Join(dir, "dst")
				require.NoError(t, os.Mkdir(dst, 0o700))
				want := []string{}
				if name == "full" {
					writeString(t, dst, "other", "old")
					want = []string{"other"}
				}

				require.ErrorIs(t, atomicfile.Replace(src, dst), fs.ErrExist)

				assert.Equal(t, "new", readString(t, filepath.Join(src, "inner")))
				assert.Equal(t, want, entryNames(t, dst))
			})
		}
	})
	t.Run("replaces a symlink at the target without following it", func(t *testing.T) {
		dir := t.TempDir()
		realFile := writeString(t, dir, "real", "real")
		src := writeString(t, dir, "src", "new")
		dst := filepath.Join(dir, "dst")
		symlinkOrSkip(t, "real", dst)

		require.NoError(t, atomicfile.Replace(src, dst))

		info, err := os.Lstat(dst)
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular())
		assert.Equal(t, "new", readString(t, dst))
		assert.Equal(t, "real", readString(t, realFile))
		assert.ElementsMatch(t, []string{"dst", "real"}, entryNames(t, dir))
	})
	t.Run("renames a symlink at the source without following it", func(t *testing.T) {
		dir := t.TempDir()
		realFile := writeString(t, dir, "real", "real")
		src := filepath.Join(dir, "src")
		symlinkOrSkip(t, "real", src)
		dst := writeString(t, dir, "dst", "old")

		require.NoError(t, atomicfile.Replace(src, dst))

		got, err := os.Readlink(dst)
		require.NoError(t, err)
		assert.Equal(t, "real", got)
		assert.Equal(t, "real", readString(t, realFile))
		assert.ElementsMatch(t, []string{"dst", "real"}, entryNames(t, dir))
	})
}
