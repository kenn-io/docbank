//go:build windows

package fslink_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
)

// Paths that os accepts past MAX_PATH work in fslink too, even where the
// system long-path setting is off.
func TestLongPaths(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), strings.Repeat("x", 100), strings.Repeat("y", 100), strings.Repeat("z", 100))
	require.NoError(os.MkdirAll(filepath.Join(dir, "target"), 0o700))
	require.Greater(len(dir), 260)
	file := filepath.Join(dir, "file")
	require.NoError(os.WriteFile(file, []byte("data"), 0o600))

	kind, err := fslink.Classify(file)
	require.NoError(err)
	assert.Equal(fslink.NotLink, kind)

	data, err := fslink.ReadFile(file)
	require.NoError(err)
	assert.Equal("data", string(data))

	f, err := fslink.OpenFile(filepath.Join(dir, "new"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(err)
	require.NoError(f.Close())

	link := filepath.Join(dir, "link")
	require.NoError(fslink.CreateJunction(filepath.Join(dir, "target"), link))
	kind, err = fslink.Classify(link)
	require.NoError(err)
	assert.Equal(fslink.Junction, kind)

	// An absolute long target, which CreateSymbolicLink needs prefixed.
	kind, err = fslink.LinkDir(filepath.Join(dir, "target"), filepath.Join(dir, "dirlink"))
	require.NoError(err)
	assert.Contains([]fslink.Kind{fslink.Symlink, fslink.Junction}, kind)
}
