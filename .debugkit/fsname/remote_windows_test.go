package fsname

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestFinalPathDropsLongPathPrefix(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(path, []byte("x"), 0o600))
	f, err := os.Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = f.Close() })

	final, err := finalPath(windows.Handle(f.Fd()))
	require.NoError(err)
	assert.False(t, strings.HasPrefix(final, `\\?\`), "final path %q", final)

	want, err := os.Stat(path)
	require.NoError(err)
	got, err := os.Stat(final)
	require.NoError(err)
	assert.True(t, os.SameFile(want, got), "final path %q names another file", final)
}
