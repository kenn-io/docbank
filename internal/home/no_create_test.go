package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoCreateLock(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	_, _, err := l.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Lstat(filepath.Join(l.Root, "vault.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	owner, err := l.TryLockExclusive()
	require.NoError(t, err)
	_, _, err = l.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, owner.Release())
	root, lock, err := l.OpenExistingAndLockExclusiveNoCreate()
	require.NoError(t, err)
	_, err = l.TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	child := Layout{Root: filepath.Join(l.Root, "child")}
	require.NoError(t, os.Mkdir(child.Root, 0o700))
	_, err = child.TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, lock.Release())
	require.NoError(t, root.Close())
}
