//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package loadfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolverRefusesFIFOReplacementWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	path := filepath.Join(root, "VOL001", "a.pdf")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))
	resolver, err := NewResolver(root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	require.NoError(t, os.Remove(path))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	_, err = resolver.Resolve(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
}
