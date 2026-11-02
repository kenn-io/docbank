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
	"go.kenn.io/kit/fslink"
)

// newJunction creates dir/target (holding inner.txt) and a junction
// dir/link pointing at it.
func newJunction(t *testing.T) (dir, link, target string) {
	t.Helper()
	require := require.New(t)
	dir = t.TempDir()
	target = filepath.Join(dir, "target")
	require.NoError(os.Mkdir(target, 0o700))
	writeString(t, target, "inner.txt", "inner")
	link = filepath.Join(dir, "link")
	require.NoError(fslink.CreateJunction(target, link))
	return dir, link, target
}

func assertJunctionUntouched(t *testing.T, dir, link, target string) {
	t.Helper()
	kind, err := fslink.Classify(link)
	require.NoError(t, err)
	assert.Equal(t, fslink.Junction, kind)
	assert.Equal(t, "inner", readString(t, filepath.Join(target, "inner.txt")))
	assert.ElementsMatch(t, []string{"link", "target"}, entryNames(t, dir))
}

func TestWriteFileRefusesJunctionByDefault(t *testing.T) {
	dir, link, target := newJunction(t)

	err := atomicfile.WriteFile(link, []byte("new"))

	require.ErrorIs(t, err, fslink.ErrIsLink)
	assertJunctionUntouched(t, dir, link, target)
}

func TestWriteNewRefusesJunction(t *testing.T) {
	dir, link, target := newJunction(t)

	err := atomicfile.WriteNew(link, []byte("new"))

	require.ErrorIs(t, err, fs.ErrExist)
	assertJunctionUntouched(t, dir, link, target)
}

func TestWriteFileWithFollowLinkRefusesJunctionToDirectory(t *testing.T) {
	dir, link, target := newJunction(t)

	err := atomicfile.WriteFile(link, []byte("new"), atomicfile.WithFollowLink())

	require.Error(t, err)
	assertJunctionUntouched(t, dir, link, target)
}

// A destination rooted without a volume (`\dir\file`) names a path on the
// link's volume, not one below the link's directory.
func TestWriteFileWithFollowLinkResolvesRootedTargetOnLinkVolume(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.Mkdir(filepath.Join(dir, "data"), 0o700))
	realFile := writeString(t, filepath.Join(dir, "data"), "cfg.json", "old")
	require.NoError(os.Mkdir(filepath.Join(dir, "app"), 0o700))
	link := filepath.Join(dir, "app", "cfg.json")
	symlinkOrSkip(t, strings.TrimPrefix(realFile, filepath.VolumeName(realFile)), link)

	require.NoError(atomicfile.WriteFile(link, []byte("new"), atomicfile.WithFollowLink()))

	assert.Equal(t, "new", readString(t, realFile))
	assert.Equal(t, []string{"cfg.json"}, entryNames(t, filepath.Join(dir, "app")))
}
