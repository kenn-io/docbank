//go:build windows

package packstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooseWindowsIdentitySnapshotSharesWithValidator(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "blob")
	require.NoError(os.WriteFile(path, []byte("content"), 0o600))
	f, err := openNoFollow(path, false)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(f.Close()) })

	info, err := snapshotPathIdentity(path)
	require.NoError(err)
	descriptorInfo, err := f.Stat()
	require.NoError(err)
	assert.True(t, os.SameFile(info, descriptorInfo))
}

func TestLooseWindowsRejectsReparseDestination(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(os.WriteFile(target, []byte("target"), 0o600))
	link := filepath.Join(dir, "link")
	require.NoError(os.Symlink(target, link))

	info, err := snapshotPathIdentity(link)
	require.NoError(err)
	assert.ErrorContains(t, validatePlatformFileInfo(info), "reparse point")
}

func TestLooseWindowsDurableHandleCanSync(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "blob")
	require.NoError(os.WriteFile(path, []byte("content"), 0o600))
	f, err := openNoFollow(path, true)
	require.NoError(err)
	require.NoError(f.Sync())
	require.NoError(f.Close())
}
