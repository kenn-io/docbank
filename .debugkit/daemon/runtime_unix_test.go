//go:build !windows

package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestRuntimeStoreRepairsPublicDir(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-public-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.Chmod(dir, 0o777))

	store := daemon.RuntimeStore{Dir: dir}
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	}
	_, err := store.Write(rec)
	require.NoError(err)
	_, err = store.List()
	require.NoError(err)
	_, err = store.CleanupDead()
	require.NoError(err)
	_, err = store.LockPath()
	require.NoError(err)
	_, err = store.Read(filepath.Join(dir, "daemon.1.json"))
	require.Error(err)
	info, err := os.Stat(dir)
	require.NoError(err)
	require.Zero(info.Mode().Perm() & 0o077)
}

func TestRuntimeStoreRepairsPrivateUnusableDir(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-unusable-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.Chmod(dir, 0o500))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.LockPath()
	require.NoError(err)
	info, err := os.Stat(dir)
	require.NoError(err)
	require.Equal(os.FileMode(0o700), info.Mode().Perm())
}

func TestRuntimeStoreCheckWritablePreservesPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through directory mode restrictions")
	}
	require := require.New(t)

	parent := t.TempDir()
	require.NoError(os.Chmod(parent, 0o500))
	t.Cleanup(func() { require.NoError(os.Chmod(parent, 0o700)) })

	err := (daemon.RuntimeStore{Dir: filepath.Join(parent, "runtime")}).CheckWritable()

	require.ErrorIs(err, os.ErrPermission)
}

func TestRuntimeStoreRejectsSymlinkDir(t *testing.T) {
	require := require.New(t)

	base := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-symlink-%d", os.Getpid()))
	target := base + "-target"
	t.Cleanup(func() {
		_ = os.Remove(base)
		_ = os.RemoveAll(target)
	})
	require.NoError(os.RemoveAll(base))
	require.NoError(os.RemoveAll(target))
	require.NoError(os.MkdirAll(target, 0o700))
	require.NoError(os.Symlink(target, base))

	store := daemon.RuntimeStore{Dir: base}
	_, err := store.LockPath()
	require.Error(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(err)
	_, err = store.List()
	require.Error(err)
	_, err = store.CleanupDead()
	require.Error(err)
}

func TestRuntimeStoreRejectsSymlinkRecord(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-record-symlink-%d", os.Getpid()))
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "daemon.1.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.WriteFile(target, []byte(`{"pid":1,"address":"127.0.0.1:7474"}`), 0o644))
	require.NoError(os.Symlink(target, link))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.Read(link)
	require.Error(err)
}

func TestRuntimeStoreRejectsNonRegularRecord(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-fifo-%d", os.Getpid()))
	record := filepath.Join(dir, "daemon.1.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(syscall.Mkfifo(record, 0o600))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.Read(record)
	require.Error(err)
}
