package home

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoCreateLock(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	root, lock, registry, err := l.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Nil(t, root)
	require.Nil(t, lock)
	require.NotEmpty(t, registry)
	_, err = os.Lstat(filepath.Join(l.Root, "vault.lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestNoCreateRegistryInsideVault(t *testing.T) {
	for _, name := range []string{"root", "child", "missing descendant", "root alias", "alias descendant"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			alias := filepath.Join(t.TempDir(), "alias")
			if name == "root alias" || name == "alias descendant" {
				if err := os.Symlink(root, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			registry := map[string]string{
				"root": root, "child": filepath.Join(root, "locks"),
				"missing descendant": filepath.Join(root, "missing", "locks"),
				"root alias":         alias, "alias descendant": filepath.Join(alias, "locks"),
			}[name]
			t.Setenv("DOCBANK_LOCK_DIR", registry)
			before, err := os.Stat(root)
			require.NoError(t, err)
			opened, held, returnedRegistry, err := (Layout{Root: root}).OpenExistingAndLockExclusiveNoCreate()
			require.ErrorIs(t, err, ErrRegistryInsideVault)
			require.Nil(t, opened)
			require.Nil(t, held)
			require.Empty(t, returnedRegistry)
			after, err := os.Stat(root)
			require.NoError(t, err)
			require.Equal(t, before.Mode(), after.Mode())
			require.Equal(t, before.ModTime(), after.ModTime())
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Empty(t, entries)
			t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
			owner, err := (Layout{Root: root}).TryLockExclusive()
			require.NoError(t, err)
			require.NoError(t, owner.Release())
		})
	}
	t.Run("default registry", func(t *testing.T) {
		t.Setenv("DOCBANK_LOCK_DIR", "")
		root := t.TempDir()
		registry := filepath.Join(root, "missing", "locks")
		before, err := os.Stat(root)
		require.NoError(t, err)
		resolve := func() (string, error) {
			return targetLockRegistryPathWithBase(func() (string, error) { return registry, nil })
		}
		opened, lock, gotRegistry, err := (Layout{Root: root}).openExistingAndLockExclusiveNoCreate(resolve)
		require.ErrorIs(t, err, ErrRegistryInsideVault)
		require.Nil(t, opened)
		require.Nil(t, lock)
		require.Empty(t, gotRegistry)
		after, err := os.Stat(root)
		require.NoError(t, err)
		require.Equal(t, before.ModTime(), after.ModTime())
		entries, err := os.ReadDir(root)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
	t.Run("missing root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "vault")
		t.Setenv("DOCBANK_LOCK_DIR", filepath.Join(root, "locks"))
		opened, held, returnedRegistry, err := (Layout{Root: root}).OpenExistingAndLockExclusiveNoCreate()
		require.ErrorIs(t, err, ErrRegistryInsideVault)
		require.Nil(t, opened)
		require.Nil(t, held)
		require.Empty(t, returnedRegistry)
		_, err = os.Lstat(root)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
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

func TestNoCreateHierarchyConflicts(t *testing.T) {
	t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
	parent := filepath.Join(t.TempDir(), "parent")
	child := filepath.Join(parent, "child")
	require.NoError(t, os.MkdirAll(child, 0o700))
	for _, root := range []string{parent, child} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "vault.lock"), nil, 0o600))
	}
	for _, pair := range [][2]string{{parent, parent}, {parent, child}, {child, parent}, {parent, filepath.Join(t.TempDir(), "alias")}} {
		for _, readerFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_%s_readerFirst_%t", filepath.Base(pair[0]), filepath.Base(pair[1]), readerFirst), func(t *testing.T) {
				if filepath.Base(pair[1]) == "alias" {
					if err := os.Symlink(parent, pair[1]); err != nil && !os.IsExist(err) {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				owner := Layout{Root: pair[0]}
				reader := Layout{Root: pair[1]}
				if readerFirst {
					root, lock, _, err := reader.OpenExistingAndLockExclusiveNoCreate()
					require.NoError(t, err)
					_, err = owner.TryLockExclusive()
					require.ErrorIs(t, err, ErrVaultLocked)
					require.NoError(t, lock.Release())
					require.NoError(t, root.Close())
				} else {
					lock, err := owner.TryLockExclusive()
					require.NoError(t, err)
					_, _, _, err = reader.OpenExistingAndLockExclusiveNoCreate()
					require.ErrorIs(t, err, ErrVaultLocked)
					require.NoError(t, lock.Release())
				}
			})
		}
	}
}

func TestNoCreateOwnershipTransitionConflict(t *testing.T) {
	t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
	layout := Layout{Root: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(layout.Root, "vault.lock"), nil, 0o600))
	transition, err := layout.TryLockOwnershipTransition()
	require.NoError(t, err)
	root, lock, registry, err := layout.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.Nil(t, root)
	require.Nil(t, lock)
	require.NotEmpty(t, registry)
	require.NoError(t, transition.Release())
}
