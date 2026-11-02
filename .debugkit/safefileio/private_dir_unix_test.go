//go:build unix

package safefileio_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestEnsurePrivateDirRepairsPublicDir(t *testing.T) {
	tests := []struct {
		name   string
		ensure func(string) error
	}{
		{name: "ensure", ensure: safefileio.EnsurePrivateDir},
		{name: "shallow", ensure: safefileio.EnsurePrivateDirShallow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			dir := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-public-%s-%d", tt.name, os.Getpid()))
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			require.NoError(os.RemoveAll(dir))
			require.NoError(os.MkdirAll(dir, 0o700))
			require.NoError(os.Chmod(dir, 0o777))

			require.NoError(tt.ensure(dir))

			info, err := os.Stat(dir)
			require.NoError(err)
			require.Equal(os.FileMode(0o700), info.Mode().Perm())
		})
	}
}

func TestValidatePrivateDirRejectsWithoutRepairingPublicDir(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-validate-public-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.Chmod(dir, 0o777))

	require.Error(safefileio.ValidatePrivateDir(dir))

	info, err := os.Stat(dir)
	require.NoError(err)
	require.Equal(os.FileMode(0o777), info.Mode().Perm())
}

func TestValidatePrivateDirAcceptsPrivateDir(t *testing.T) {
	dir := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-validate-private-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(dir, 0o700))

	require.NoError(t, safefileio.ValidatePrivateDir(dir))
}

func TestEnsurePrivateDirRejectsEmptyPath(t *testing.T) {
	require.Error(t, safefileio.EnsurePrivateDir(""))
}

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	require := require.New(t)

	base := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-symlink-%d", os.Getpid()))
	target := base + "-target"
	t.Cleanup(func() {
		_ = os.Remove(base)
		_ = os.RemoveAll(target)
	})
	require.NoError(os.RemoveAll(base))
	require.NoError(os.RemoveAll(target))
	require.NoError(os.MkdirAll(target, 0o700))
	require.NoError(os.Symlink(target, base))

	require.Error(safefileio.EnsurePrivateDir(base))
}

func TestOpenCurrentUserFileRejectsEmptyPath(t *testing.T) {
	file, err := safefileio.OpenCurrentUserFile("")
	require.Error(t, err)
	require.Nil(t, file)
}

func TestOpenCurrentUserFileRejectsNonRegularFile(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-safefileio-fifo-%d", os.Getpid()))
	path := filepath.Join(dir, "record.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(syscall.Mkfifo(path, 0o600))

	file, err := safefileio.OpenCurrentUserFile(path)
	require.Error(err)
	require.Nil(file)
}
