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
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	require.NoError(t, os.Remove(path))
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	_, err = resolver.Open(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "a.pdf")
	require.ErrorIs(t, err, ErrUnsafeReference)
}

func TestResolverCountsLargeSparseFileExtents(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	file, err := os.Create(filepath.Join(root, "VOL001", "source.bin"))
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	require.NoError(t, file.Truncate((4<<30)+1))
	resolver, err := NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	resolver.MaxBytes = 4 << 30
	_, err = resolver.Open(Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, "source.bin")
	require.ErrorIs(t, err, ErrLoadfileLimit)
	require.NoError(t, resolver.Close())
	require.NoError(t, file.Truncate(defaultMaxPackageBytes+1))
	_, err = NewResolver(t.Context(), root, nil)
	require.ErrorIs(t, err, ErrLoadfileLimit)
}
