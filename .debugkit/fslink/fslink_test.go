//go:build unix || windows

package fslink_test

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
)

func TestClassifyIsLinkReadlink(t *testing.T) {
	notLinks := []struct {
		name string
		path string
	}{
		{name: "regular file", path: "file.txt"},
		{name: "dir", path: "sub"},
	}
	for _, tc := range notLinks {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			path := filepath.Join(newTree(t), tc.path)

			kind, err := fslink.Classify(path)
			require.NoError(t, err)
			assert.Equal(fslink.NotLink, kind)

			isLink, err := fslink.IsLink(path)
			require.NoError(t, err)
			assert.False(isLink)

			_, err = fslink.Readlink(path)
			assert.Error(err)
		})
	}
	for _, maker := range linkMakers() {
		t.Run(maker.name, func(t *testing.T) {
			assert := assert.New(t)
			dir := newTree(t)
			target := maker.create(t, dir, "link")
			path := filepath.Join(dir, "link")

			kind, err := fslink.Classify(path)
			require.NoError(t, err)
			assert.Equal(maker.kind, kind)

			isLink, err := fslink.IsLink(path)
			require.NoError(t, err)
			assert.True(isLink)

			got, err := fslink.Readlink(path)
			require.NoError(t, err)
			assert.Equal(target, got)
		})
	}
}

func TestClassifyMissingPath(t *testing.T) {
	_, err := fslink.Classify(filepath.Join(t.TempDir(), "missing"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestOpenFileRefusesFinalLink(t *testing.T) {
	flags := []struct {
		name string
		flag int
	}{
		{name: "read", flag: os.O_RDONLY},
		{name: "truncate", flag: os.O_WRONLY | os.O_TRUNC},
		{name: "create", flag: os.O_WRONLY | os.O_CREATE},
	}
	for _, maker := range linkMakers() {
		for _, f := range flags {
			t.Run(maker.name+"/"+f.name, func(t *testing.T) {
				dir := newTree(t)
				maker.create(t, dir, "link")

				file, err := fslink.OpenFile(filepath.Join(dir, "link"), f.flag, 0o600)
				if file != nil {
					_ = file.Close()
				}
				require.ErrorIs(t, err, fslink.ErrIsLink)
				assertTreeUntouched(t, dir)
			})
		}
	}
}

// assertTreeUntouched checks that no open through a link truncated or
// created a link destination.
func assertTreeUntouched(t *testing.T, dir string) {
	t.Helper()
	assert := assert.New(t)
	data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal("hello", string(data))
	data, err = os.ReadFile(filepath.Join(dir, "sub", "inner.txt"))
	require.NoError(t, err)
	assert.Equal("inner", string(data))
	assert.NoFileExists(filepath.Join(dir, "missing"))
}

func TestOpenFileFollowsIntermediateLink(t *testing.T) {
	for _, maker := range dirLinkMakers() {
		t.Run(maker.name, func(t *testing.T) {
			dir := newTree(t)
			maker.create(t, dir, "link")

			file, err := fslink.OpenFile(filepath.Join(dir, "link", "inner.txt"), os.O_RDONLY, 0)
			require.NoError(t, err)
			t.Cleanup(func() { _ = file.Close() })
			assert.Equal(t, "inner", readAll(t, file))
		})
	}
}

func TestOpenFilePlainPaths(t *testing.T) {
	t.Run("truncate", func(t *testing.T) {
		path := filepath.Join(newTree(t), "file.txt")
		file, err := fslink.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0)
		require.NoError(t, err)
		_, err = file.WriteString("new")
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assertFileContent(t, path, "new")
	})
	t.Run("append", func(t *testing.T) {
		path := filepath.Join(newTree(t), "file.txt")
		file, err := fslink.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		require.NoError(t, err)
		_, err = file.WriteString(" world")
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assertFileContent(t, path, "hello world")
	})
	t.Run("exclusive create", func(t *testing.T) {
		dir := newTree(t)
		path := filepath.Join(dir, "new.txt")
		file, err := fslink.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		require.NoError(t, err)
		_, err = file.WriteString("created")
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assertFileContent(t, path, "created")

		_, err = fslink.OpenFile(filepath.Join(dir, "file.txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		assert.ErrorIs(t, err, fs.ErrExist)
	})
	t.Run("directory for writing", func(t *testing.T) {
		file, err := fslink.OpenFile(filepath.Join(newTree(t), "sub"), os.O_WRONLY, 0)
		if file != nil {
			_ = file.Close()
		}
		assert.Error(t, err)
	})
}

// Every write to an O_APPEND file lands at the end, even after a Seek and
// even when O_TRUNC emptied the file first.
func TestOpenFileAppendIgnoresSeek(t *testing.T) {
	cases := []struct {
		name string
		flag int
		want string
	}{
		{name: "append", flag: os.O_WRONLY | os.O_APPEND, want: "helloab"},
		{name: "append truncate", flag: os.O_WRONLY | os.O_APPEND | os.O_TRUNC, want: "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			path := filepath.Join(newTree(t), "file.txt")
			file, err := fslink.OpenFile(path, tc.flag, 0)
			require.NoError(err)
			_, err = file.WriteString("a")
			require.NoError(err)
			_, err = file.Seek(0, io.SeekStart)
			require.NoError(err)
			_, err = file.WriteString("b")
			require.NoError(err)
			require.NoError(file.Close())
			assertFileContent(t, path, tc.want)
		})
	}
}

func TestOpenRegular(t *testing.T) {
	t.Run("regular file", func(t *testing.T) {
		file, err := fslink.OpenRegular(filepath.Join(newTree(t), "file.txt"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		assert.Equal(t, "hello", readAll(t, file))
	})
	t.Run("directory", func(t *testing.T) {
		_, err := fslink.OpenRegular(filepath.Join(newTree(t), "sub"))
		assert.ErrorContains(t, err, "not a regular file")
	})
	for _, maker := range linkMakers() {
		t.Run(maker.name, func(t *testing.T) {
			dir := newTree(t)
			maker.create(t, dir, "link")
			_, err := fslink.OpenRegular(filepath.Join(dir, "link"))
			assert.ErrorIs(t, err, fslink.ErrIsLink)
		})
	}
}

func TestReadFile(t *testing.T) {
	dir := newTree(t)
	data, err := fslink.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	symlinkOrSkip(t, "file.txt", filepath.Join(dir, "link"))
	_, err = fslink.ReadFile(filepath.Join(dir, "link"))
	assert.ErrorIs(t, err, fslink.ErrIsLink)
}

func openTreeRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := newTree(t)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func TestOpenInRootPlainPaths(t *testing.T) {
	t.Run("nested read", func(t *testing.T) {
		_, root := openTreeRoot(t)
		file, err := fslink.OpenInRoot(root, filepath.Join("sub", "inner.txt"), os.O_RDONLY, 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		assert.Equal(t, "inner", readAll(t, file))
	})
	t.Run("nested create", func(t *testing.T) {
		dir, root := openTreeRoot(t)
		file, err := fslink.OpenInRoot(root, filepath.Join("sub", "new.txt"), os.O_WRONLY|os.O_CREATE, 0o600)
		require.NoError(t, err)
		_, err = file.WriteString("created")
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assertFileContent(t, filepath.Join(dir, "sub", "new.txt"), "created")
	})
	t.Run("truncate", func(t *testing.T) {
		dir, root := openTreeRoot(t)
		file, err := fslink.OpenInRoot(root, "file.txt", os.O_WRONLY|os.O_TRUNC, 0)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assertFileContent(t, filepath.Join(dir, "file.txt"), "")
	})
	t.Run("create existing without truncate", func(t *testing.T) {
		dir, root := openTreeRoot(t)
		file, err := fslink.OpenInRoot(root, "file.txt", os.O_WRONLY|os.O_CREATE, 0o600)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		assertFileContent(t, filepath.Join(dir, "file.txt"), "hello")
	})
	t.Run("exclusive create of existing", func(t *testing.T) {
		_, root := openTreeRoot(t)
		_, err := fslink.OpenInRoot(root, "file.txt", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		assert.ErrorIs(t, err, fs.ErrExist)
	})
	t.Run("missing intermediate", func(t *testing.T) {
		_, root := openTreeRoot(t)
		_, err := fslink.OpenInRoot(root, filepath.Join("missing", "x.txt"), os.O_WRONLY|os.O_CREATE, 0o600)
		assert.ErrorIs(t, err, fs.ErrNotExist)
	})
	for _, name := range []string{"", filepath.Join("..", "x"), filepath.Join(t.TempDir(), "x")} {
		t.Run("non-local "+name, func(t *testing.T) {
			_, root := openTreeRoot(t)
			_, err := fslink.OpenInRoot(root, name, os.O_RDONLY, 0)
			assert.ErrorContains(t, err, "not local")
		})
	}
}

func TestOpenInRootRefusesLinks(t *testing.T) {
	for _, maker := range dirLinkMakers() {
		t.Run("intermediate "+maker.name, func(t *testing.T) {
			dir, root := openTreeRoot(t)
			maker.create(t, dir, "link")
			_, err := fslink.OpenInRoot(root, filepath.Join("link", "inner.txt"), os.O_RDONLY, 0)
			assert.ErrorIs(t, err, fslink.ErrIsLink)
		})
	}
	for _, maker := range linkMakers() {
		for _, flag := range []int{os.O_RDONLY, os.O_WRONLY | os.O_TRUNC, os.O_WRONLY | os.O_CREATE} {
			t.Run("final "+maker.name, func(t *testing.T) {
				dir, root := openTreeRoot(t)
				maker.create(t, dir, "link")
				file, err := fslink.OpenInRoot(root, "link", flag, 0o600)
				if file != nil {
					_ = file.Close()
				}
				require.ErrorIs(t, err, fslink.ErrIsLink)
				assertTreeUntouched(t, dir)
			})
		}
	}
}

// An exclusive create never follows a link and reports it the way it reports
// any existing entry: fs.ErrExist, not ErrIsLink.
func TestExclusiveCreateOnLinkReportsExist(t *testing.T) {
	const flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	opens := []struct {
		name string
		open func(t *testing.T, dir string) (*os.File, error)
	}{
		{name: "OpenFile", open: func(t *testing.T, dir string) (*os.File, error) {
			t.Helper()
			return fslink.OpenFile(filepath.Join(dir, "link"), flag, 0o600)
		}},
		{name: "OpenInRoot", open: func(t *testing.T, dir string) (*os.File, error) {
			t.Helper()
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = root.Close() })
			return fslink.OpenInRoot(root, "link", flag, 0o600)
		}},
	}
	for _, maker := range linkMakers() {
		for _, op := range opens {
			t.Run(op.name+"/"+maker.name, func(t *testing.T) {
				assert := assert.New(t)
				dir := newTree(t)
				target := maker.create(t, dir, "link")

				file, err := op.open(t, dir)
				if file != nil {
					_ = file.Close()
				}
				require.ErrorIs(t, err, fs.ErrExist)
				require.NotErrorIs(t, err, fslink.ErrIsLink)
				assertTreeUntouched(t, dir)
				kind, err := fslink.Classify(filepath.Join(dir, "link"))
				require.NoError(t, err)
				assert.Equal(maker.kind, kind)
				got, err := fslink.Readlink(filepath.Join(dir, "link"))
				require.NoError(t, err)
				assert.Equal(target, got)
			})
		}
	}
}

// A create that fails for another reason must not claim the name is taken:
// callers retry under a new name on fs.ErrExist.
func TestExclusiveCreateFailureWithoutEntryIsNotExist(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, root.Close())

	file, err := fslink.OpenInRoot(root, "new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)

	require.Error(t, err)
	assert.Nil(t, file)
	assert.NotErrorIs(t, err, fs.ErrExist)
}

func TestOpenRootNoFollow(t *testing.T) {
	t.Run("plain dir", func(t *testing.T) {
		_, root := openTreeRoot(t)
		sub, err := fslink.OpenRootNoFollow(root, "sub")
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Close() })
		data, err := sub.ReadFile("inner.txt")
		require.NoError(t, err)
		assert.Equal(t, "inner", string(data))
	})
	t.Run("regular file", func(t *testing.T) {
		_, root := openTreeRoot(t)
		_, err := fslink.OpenRootNoFollow(root, "file.txt")
		assert.Error(t, err)
	})
	for _, maker := range dirLinkMakers() {
		t.Run("final "+maker.name, func(t *testing.T) {
			dir, root := openTreeRoot(t)
			maker.create(t, dir, "link")
			_, err := fslink.OpenRootNoFollow(root, "link")
			assert.ErrorIs(t, err, fslink.ErrIsLink)
		})
		t.Run("intermediate "+maker.name, func(t *testing.T) {
			dir, root := openTreeRoot(t)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "sub", "nested"), 0o755))
			maker.create(t, dir, "link")
			_, err := fslink.OpenRootNoFollow(root, filepath.Join("link", "nested"))
			assert.ErrorIs(t, err, fslink.ErrIsLink)
		})
	}
}

func TestOpenRoot(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := newTree(t)
		root, err := fslink.OpenRoot(filepath.Join(dir, "sub"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = root.Close() })
		data, err := root.ReadFile("inner.txt")
		require.NoError(t, err)
		assert.Equal(t, "inner", string(data))
	})
	t.Run("link in an earlier component is followed", func(t *testing.T) {
		for _, maker := range dirLinkMakers() {
			t.Run(maker.name, func(t *testing.T) {
				dir := newTree(t)
				require.NoError(t, os.Mkdir(filepath.Join(dir, "sub", "deeper"), 0o755))
				maker.create(t, dir, "link")
				root, err := fslink.OpenRoot(filepath.Join(dir, "link", "deeper"))
				require.NoError(t, err)
				assert.NoError(t, root.Close())
			})
		}
	})
	t.Run("final link is refused", func(t *testing.T) {
		for _, maker := range dirLinkMakers() {
			t.Run(maker.name, func(t *testing.T) {
				dir := newTree(t)
				maker.create(t, dir, "link")
				root, err := fslink.OpenRoot(filepath.Join(dir, "link"))
				assert.Nil(t, root)
				require.ErrorIs(t, err, fslink.ErrIsLink)
			})
		}
	})
	t.Run("final link with a trailing separator is refused", func(t *testing.T) {
		for _, maker := range dirLinkMakers() {
			t.Run(maker.name, func(t *testing.T) {
				dir := newTree(t)
				maker.create(t, dir, "link")
				root, err := fslink.OpenRoot(filepath.Join(dir, "link") + string(filepath.Separator))
				assert.Nil(t, root)
				require.ErrorIs(t, err, fslink.ErrIsLink)
			})
		}
	})
	t.Run("regular file", func(t *testing.T) {
		root, err := fslink.OpenRoot(filepath.Join(newTree(t), "file.txt"))
		assert.Nil(t, root)
		require.Error(t, err)
	})
	t.Run("empty path", func(t *testing.T) {
		root, err := fslink.OpenRoot("")
		assert.Nil(t, root)
		require.Error(t, err)
	})
	t.Run("missing", func(t *testing.T) {
		root, err := fslink.OpenRoot(filepath.Join(newTree(t), "missing"))
		assert.Nil(t, root)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
}

func TestLinkDirResolvesRelativeTarget(t *testing.T) {
	assert := assert.New(t)
	dir := newTree(t)
	link := filepath.Join(dir, "link")

	kind, err := fslink.LinkDir("sub", link)
	require.NoError(t, err)
	classified, err := fslink.Classify(link)
	require.NoError(t, err)
	assert.Equal(kind, classified)
	assertFileContent(t, filepath.Join(link, "inner.txt"), "inner")
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(data))
}
