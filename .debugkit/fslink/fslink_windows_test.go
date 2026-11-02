//go:build windows

package fslink_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/fslink"
)

func TestCreateJunctionResolvesRelativeTargetFromWorkingDir(t *testing.T) {
	assert := assert.New(t)
	dir := newTree(t)
	t.Chdir(dir)

	require.NoError(t, fslink.CreateJunction("sub", "link"))
	target, err := fslink.Readlink(filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.Equal(filepath.Join(dir, "sub"), target)
	assertFileContent(t, filepath.Join(dir, "link", "inner.txt"), "inner")
}

func TestCreateJunctionRejectsNonDirectoryTargets(t *testing.T) {
	for _, target := range []string{"file.txt", "missing"} {
		t.Run(target, func(t *testing.T) {
			dir := newTree(t)
			link := filepath.Join(dir, "link")
			err := fslink.CreateJunction(filepath.Join(dir, target), link)
			require.Error(t, err)
			_, statErr := os.Lstat(link)
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

// os.Symlink would create a file symlink for a missing target, which Windows
// refuses to traverse once the directory exists.
func TestLinkDirCreatesDirectorySymlinkBeforeTargetExists(t *testing.T) {
	require := require.New(t)
	dir := newTree(t)
	symlinkOrSkip(t, "sub", filepath.Join(dir, "probe"))
	link := filepath.Join(dir, "link")

	kind, err := fslink.LinkDir("later", link)
	require.NoError(err)
	require.Equal(fslink.Symlink, kind)
	require.NoError(os.Mkdir(filepath.Join(dir, "later"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(dir, "later", "x.txt"), []byte("later"), 0o600))

	assertFileContent(t, filepath.Join(link, "x.txt"), "later")
}

const wofContent = "compressible content "

// newWOFFile writes a WOF-compressed file named compressed.txt in dir and
// returns its path and content. It skips when compact does not produce a
// reparse point.
func newWOFFile(t *testing.T, dir string) (string, string) {
	t.Helper()
	require := require.New(t)
	content := strings.Repeat(wofContent, 4096)
	path := filepath.Join(dir, "compressed.txt")
	require.NoError(os.WriteFile(path, []byte(content), 0o600))
	out, err := exec.CommandContext(t.Context(), "compact.exe", "/c", "/exe:xpress4k", path).CombinedOutput()
	require.NoError(err, "compact: %s", out)
	path16, err := windows.UTF16PtrFromString(path)
	require.NoError(err)
	attrs, err := windows.GetFileAttributes(path16)
	require.NoError(err)
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Skipf("compact did not make a WOF reparse point: %s", out)
	}
	kind, err := fslink.Classify(path)
	require.NoError(err)
	require.Equal(fslink.NotLink, kind)
	return path, content
}

// A WOF-compressed file is a reparse point that is not a link. Its content is
// served by a filter driver that FILE_FLAG_OPEN_REPARSE_POINT bypasses, so
// opens must reach the file itself, not the stub.
func TestOpenNonLinkReparsePointReadsFileContent(t *testing.T) {
	t.Run("ReadFile", func(t *testing.T) {
		path, content := newWOFFile(t, t.TempDir())
		data, err := fslink.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, content, string(data))
	})
	t.Run("OpenFile", func(t *testing.T) {
		path, content := newWOFFile(t, t.TempDir())
		file, err := fslink.OpenFile(path, os.O_RDONLY, 0)
		require.NoError(t, err)
		defer func() { _ = file.Close() }()
		assert.Equal(t, content, readAll(t, file))
	})
	t.Run("OpenFile truncates the file", func(t *testing.T) {
		require := require.New(t)
		path, _ := newWOFFile(t, t.TempDir())
		file, err := fslink.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		require.NoError(err)
		_, err = file.WriteString("new")
		require.NoError(err)
		require.NoError(file.Close())
		assertFileContent(t, path, "new")
	})
}

// os.Root refuses every reparse point on Windows, so the root-confined opens
// refuse a WOF file that OpenFile reads, and say why.
func TestOpenInRootRefusesNonLinkReparsePoint(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path, content := newWOFFile(t, dir)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	file, err := fslink.OpenInRoot(root, "compressed.txt", os.O_RDONLY, 0)
	if file != nil {
		_ = file.Close()
	}
	require.ErrorIs(t, err, errors.ErrUnsupported)
	require.ErrorContains(t, err, "reparse point refused under a root")
	require.ErrorContains(t, err, "compressed.txt")
	require.NotErrorIs(t, err, fslink.ErrIsLink)

	file, err = fslink.OpenFile(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	assert.Equal(content, readAll(t, file))
}

func TestCreateJunctionRefusesExistingEntry(t *testing.T) {
	existing := []struct {
		name   string
		create func(t *testing.T, dir string)
		check  func(t *testing.T, dir string)
	}{
		{
			name: "file",
			create: func(t *testing.T, dir string) {
				t.Helper()
				renameInTree(t, dir, "file.txt")
			},
			check: func(t *testing.T, dir string) {
				t.Helper()
				assertFileContent(t, filepath.Join(dir, "link"), "hello")
			},
		},
		{
			name: "dir",
			create: func(t *testing.T, dir string) {
				t.Helper()
				renameInTree(t, dir, "sub")
			},
			check: func(t *testing.T, dir string) {
				t.Helper()
				kind, err := fslink.Classify(filepath.Join(dir, "link"))
				require.NoError(t, err)
				assert.Equal(t, fslink.NotLink, kind)
				assertFileContent(t, filepath.Join(dir, "link", "inner.txt"), "inner")
			},
		},
		{
			name: "junction",
			create: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, fslink.CreateJunction(filepath.Join(dir, "sub"), filepath.Join(dir, "link")))
			},
			check: func(t *testing.T, dir string) {
				t.Helper()
				target, err := fslink.Readlink(filepath.Join(dir, "link"))
				require.NoError(t, err)
				assert.Equal(t, filepath.Join(dir, "sub"), target)
				assertFileContent(t, filepath.Join(dir, "link", "inner.txt"), "inner")
			},
		},
	}
	for _, tc := range existing {
		t.Run(tc.name, func(t *testing.T) {
			dir := newTree(t)
			other := filepath.Join(dir, "other")
			require.NoError(t, os.Mkdir(other, 0o755))
			tc.create(t, dir)

			err := fslink.CreateJunction(other, filepath.Join(dir, "link"))
			require.ErrorIs(t, err, fs.ErrExist)
			tc.check(t, dir)
		})
	}
}

// renameInTree moves name inside dir to dir/link.
func renameInTree(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.Rename(filepath.Join(dir, name), filepath.Join(dir, "link")))
}
