package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoCreateLock(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	_, _, _, err := l.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Lstat(filepath.Join(l.Root, "vault.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	owner, err := l.TryLockExclusive()
	require.NoError(t, err)
	_, _, _, err = l.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, owner.Release())
	root, lock, _, err := l.OpenExistingAndLockExclusiveNoCreate()
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

func TestNoCreateRegistryInsideVault(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, registry := range []string{root, filepath.Join(root, "locks"), filepath.Join(root, "missing", "locks"), alias, filepath.Join(alias, "locks")} {
		t.Run(registry, func(t *testing.T) {
			t.Setenv("DOCBANK_LOCK_DIR", registry)
			before, err := os.Stat(root)
			require.NoError(t, err)
			_, _, _, err = (Layout{Root: root}).OpenExistingAndLockExclusiveNoCreate()
			require.ErrorIs(t, err, ErrRegistryInsideVault)
			after, err := os.Stat(root)
			require.NoError(t, err)
			require.Equal(t, before.ModTime(), after.ModTime())
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestNoCreateRegistryParent(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "vault")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "vault.lock"), nil, 0o600))
	t.Setenv("DOCBANK_LOCK_DIR", parent)
	opened, lock, registry, err := (Layout{Root: root}).OpenExistingAndLockExclusiveNoCreate()
	require.NoError(t, err)
	canonical, err := CanonicalRoot(parent)
	require.NoError(t, err)
	require.Equal(t, canonical, registry)
	require.NoError(t, lock.Release())
	require.NoError(t, opened.Close())
}
