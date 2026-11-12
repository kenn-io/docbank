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
	owner, err := l.TryLockExclusive()
	require.NoError(t, err)
	root, lock, registry, err = l.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.Nil(t, root)
	require.Nil(t, lock)
	require.NotEmpty(t, registry)
	require.NoError(t, owner.Release())
	root, lock, registry, err = l.OpenExistingAndLockExclusiveNoCreate()
	require.NoError(t, err)
	require.NotEmpty(t, registry)
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
	for _, registry := range []string{root, filepath.Join(root, "locks"), filepath.Join(root, "missing", "locks"), alias, filepath.Join(alias, "locks")} {
		t.Run(registry, func(t *testing.T) {
			if registry == alias || registry == filepath.Join(alias, "locks") {
				if err := os.Symlink(root, alias); err != nil && !os.IsExist(err) {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
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

func TestNoCreateDefaultRegistryInsideVault(t *testing.T) {
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
}

func TestNoCreateRegistryInsidePreparedOrMissingVault(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_%t", missing), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "vault")
			if !missing {
				require.NoError(t, os.Mkdir(root, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "vault.lock"), []byte("leaf"), 0o600))
			}
			t.Setenv("DOCBANK_LOCK_DIR", filepath.Join(root, "locks"))
			_, _, _, err := (Layout{Root: root}).OpenExistingAndLockExclusiveNoCreate()
			require.ErrorIs(t, err, ErrRegistryInsideVault)
			if missing {
				_, err = os.Lstat(root)
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			data, err := os.ReadFile(filepath.Join(root, "vault.lock"))
			require.NoError(t, err)
			require.Equal(t, "leaf", string(data))
			t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
			lock, err := (Layout{Root: root}).TryLockExclusive()
			require.NoError(t, err)
			require.NoError(t, lock.Release())
		})
	}
}

func TestNoCreateOwnershipTransitionConflict(t *testing.T) {
	t.Setenv("DOCBANK_LOCK_DIR", t.TempDir())
	layout := Layout{Root: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(layout.Root, "vault.lock"), nil, 0o600))
	transition, err := layout.TryLockOwnershipTransition()
	require.NoError(t, err)
	_, _, _, err = layout.OpenExistingAndLockExclusiveNoCreate()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, transition.Release())
	root, lock, _, err := layout.OpenExistingAndLockExclusiveNoCreate()
	require.NoError(t, err)
	transition, err = layout.TryLockOwnershipTransition()
	require.NoError(t, err)
	_, _, err = transition.OpenExistingAndLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, transition.Release())
	require.NoError(t, lock.Release())
	require.NoError(t, root.Close())
	transition, err = layout.TryLockOwnershipTransition()
	require.NoError(t, err)
	require.NoError(t, transition.Release())
}
