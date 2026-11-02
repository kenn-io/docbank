//go:build unix || windows

package fslink_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
)

// newTree creates dir/file.txt ("hello") and dir/sub/inner.txt ("inner").
func newTree(t *testing.T) string {
	t.Helper()
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o600))
	require.NoError(os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(dir, "sub", "inner.txt"), []byte("inner"), 0o600))
	return dir
}

// symlinkOrSkip creates a symlink, skipping the test when the platform
// refuses symlink creation to an unprivileged process.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if symlinkPrivilegeMissing(err) {
		t.Skipf("symlink creation needs a privilege this process lacks: %v", err)
	}
	require.NoError(t, err)
}

// linkMaker creates a link named name inside a newTree directory and returns
// the destination Readlink should report.
type linkMaker struct {
	name   string
	kind   fslink.Kind
	toDir  bool
	create func(t *testing.T, dir, name string) string
}

func symlinkTo(target string) func(*testing.T, string, string) string {
	return func(t *testing.T, dir, name string) string {
		t.Helper()
		symlinkOrSkip(t, target, filepath.Join(dir, name))
		return target
	}
}

// linkMakers returns every link kind the platform can create. Symlink
// targets are relative so the links stay inside the tree and an os.Root would
// follow them.
func linkMakers() []linkMaker {
	return append([]linkMaker{
		{name: "symlink to file", kind: fslink.Symlink, create: symlinkTo("file.txt")},
		{name: "symlink to dir", kind: fslink.Symlink, toDir: true, create: symlinkTo("sub")},
		{name: "dangling symlink", kind: fslink.Symlink, create: symlinkTo("missing")},
	}, platformLinkMakers()...)
}

func dirLinkMakers() []linkMaker {
	var makers []linkMaker
	for _, maker := range linkMakers() {
		if maker.toDir {
			makers = append(makers, maker)
		}
	}
	return makers
}
