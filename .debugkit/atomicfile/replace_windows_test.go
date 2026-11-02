//go:build windows

package atomicfile_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/internal/winpath"
)

// openShareAll opens path for reading with FILE_SHARE_READ, FILE_SHARE_WRITE,
// and FILE_SHARE_DELETE, the way a reader that tolerates renames would.
func openShareAll(t *testing.T, path string) *os.File {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	f := os.NewFile(uintptr(h), path)
	t.Cleanup(func() { f.Close() })
	return f
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	_, err := f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(data)
}

// A reader holding the target open with FILE_SHARE_DELETE does not block the
// POSIX-semantics rename, and keeps reading the content it opened.
func TestReplaceTargetOpenWithShareDelete(t *testing.T) {
	openers := map[string]func(t *testing.T, dir string) *os.File{
		"CreateFile": func(t *testing.T, dir string) *os.File {
			t.Helper()
			return openShareAll(t, filepath.Join(dir, "dst"))
		},
		"os.Root": func(t *testing.T, dir string) *os.File {
			t.Helper()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { root.Close() })
			f, err := root.Open("dst")
			require.NoError(t, err)
			t.Cleanup(func() { f.Close() })
			return f
		},
	}
	for name, open := range openers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			src := writeString(t, dir, "src", "new")
			dst := writeString(t, dir, "dst", "old")
			held := open(t, dir)

			require.NoError(t, atomicfile.Replace(src, dst))

			assert.Equal(t, "new", readString(t, dst))
			assert.Equal(t, "old", readAll(t, held))
			assert.Equal(t, []string{"dst"}, entryNames(t, dir))
		})
	}
}

// os.Open shares read and write but not delete, so a reader holding the
// target that way still blocks Replace, with a sharing violation.
func TestReplaceTargetOpenWithoutShareDelete(t *testing.T) {
	dir := t.TempDir()
	src := writeString(t, dir, "src", "new")
	dst := writeString(t, dir, "dst", "old")
	held, err := os.Open(dst)
	require.NoError(t, err)
	defer held.Close()

	err = atomicfile.Replace(src, dst)

	require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
	assert.Equal(t, "new", readString(t, src))
	assert.Equal(t, "old", readString(t, dst))
}

func TestReplaceSourceOpen(t *testing.T) {
	t.Run("with FILE_SHARE_DELETE", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "new")
		dst := writeString(t, dir, "dst", "old")
		held := openShareAll(t, src)

		require.NoError(t, atomicfile.Replace(src, dst))

		assert.Equal(t, "new", readString(t, dst))
		assert.Equal(t, "new", readAll(t, held))
		assert.Equal(t, []string{"dst"}, entryNames(t, dir))
	})
	t.Run("without FILE_SHARE_DELETE", func(t *testing.T) {
		dir := t.TempDir()
		src := writeString(t, dir, "src", "new")
		dst := writeString(t, dir, "dst", "old")
		held, err := os.Open(src)
		require.NoError(t, err)
		defer held.Close()

		err = atomicfile.Replace(src, dst)

		require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		assert.Equal(t, "new", readString(t, src))
		assert.Equal(t, "old", readString(t, dst))
	})
}

// A junction at the target is replaced as an entry, like a symlink; the
// directory it names is never written through.
func TestReplaceJunctionTarget(t *testing.T) {
	dir, link, target := newJunction(t)
	src := writeString(t, t.TempDir(), "src", "new")

	require.NoError(t, atomicfile.Replace(src, link))

	kind, err := fslink.Classify(link)
	require.NoError(t, err)
	assert.Equal(t, fslink.NotLink, kind)
	assert.Equal(t, "new", readString(t, link))
	assert.Equal(t, []string{"inner.txt"}, entryNames(t, target))
	assert.Equal(t, "inner", readString(t, filepath.Join(target, "inner.txt")))
	assert.ElementsMatch(t, []string{"link", "target"}, entryNames(t, dir))
}

// A relative target reaches FileRenameInfoEx as given. The held reader
// proves the POSIX-semantics rename took it: the MoveFileEx fallback would
// fail.
func TestReplaceRelativePaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	writeString(t, dir, "src", "new")
	dst := writeString(t, filepath.Join(dir, "sub"), "dst", "old")
	held := openShareAll(t, dst)
	t.Chdir(dir)
	rel := filepath.Join("sub", "dst")
	require.Equal(t, rel, winpath.Long(rel), "the target must stay relative")

	require.NoError(t, atomicfile.Replace("src", rel))

	assert.Equal(t, "new", readString(t, dst))
	assert.Equal(t, "old", readAll(t, held))
	assert.Equal(t, []string{"sub"}, entryNames(t, dir))
}

// Windows names are case-insensitive, so a case-only rename of a directory
// finds the directory itself at newpath and must still go ahead.
func TestReplaceCaseOnlyDirectoryRename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Data")
	require.NoError(t, os.Mkdir(src, 0o700))
	writeString(t, src, "inner", "inner")

	require.NoError(t, atomicfile.Replace(src, filepath.Join(dir, "data")))

	assert.Equal(t, []string{"data"}, entryNames(t, dir))
	assert.Equal(t, "inner", readString(t, filepath.Join(dir, "data", "inner")))
}
