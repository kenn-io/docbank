//go:build unix

package home

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTryLockExclusive(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())

	lk, err := l.TryLockExclusive()
	require.NoError(t, err)
	_, err = l.TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, lk.Release())

	lk2, err := l.TryLockExclusive()
	require.NoError(t, err)
	require.NoError(t, lk2.Release())
}

func TestTryLockExclusiveRejectsOverlappingTrees(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "restore")
	child := filepath.Join(parent, "blobs")
	sibling := filepath.Join(base, "sibling")
	for _, dir := range []string{child, sibling} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	parentLock, err := (Layout{Root: parent}).TryLockExclusive()
	require.NoError(t, err)
	_, err = (Layout{Root: child}).TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked,
		"a descendant must share-lock the exclusively owned parent identity")
	siblingLock, err := (Layout{Root: sibling}).TryLockExclusive()
	require.NoError(t, err, "disjoint sibling trees may be owned concurrently")
	require.NoError(t, siblingLock.Release())
	require.NoError(t, parentLock.Release())

	childLock, err := (Layout{Root: child}).TryLockExclusive()
	require.NoError(t, err)
	_, err = (Layout{Root: parent}).TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked,
		"a parent must exclusive-lock an identity already shared by its descendant")
	require.NoError(t, childLock.Release())
}

func TestTryLockExclusiveResolvesIntermediateSymlinkAncestry(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	resolvedParent := filepath.Join(realParent, "deep")
	vault := filepath.Join(resolvedParent, "vault")
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.MkdirAll(vault, 0o700))
	require.NoError(t, os.Symlink(resolvedParent, alias))

	aliasLock, err := (Layout{Root: filepath.Join(alias, "vault")}).TryLockExclusive()
	require.NoError(t, err)
	defer func() { _ = aliasLock.Release() }()

	_, err = (Layout{Root: realParent}).TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked,
		"an alias must lock every ancestor of its resolved destination")
}

func TestOpenAndLockExclusiveCoordinatesBeforeCreatingMissingRoot(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "restore")
	require.NoError(t, os.Mkdir(parent, 0o700))
	parentLock, err := (Layout{Root: parent}).TryLockExclusive()
	require.NoError(t, err)
	defer func() { _ = parentLock.Release() }()

	missing := filepath.Join(parent, "docbank.db")
	root, lock, err := (Layout{Root: missing}).OpenAndLockExclusive()
	if root != nil {
		_ = root.Close()
	}
	if lock != nil {
		_ = lock.Release()
	}
	require.ErrorIs(t, err, ErrVaultLocked)
	_, err = os.Lstat(missing)
	require.ErrorIs(t, err, os.ErrNotExist,
		"startup must coordinate with the existing ancestor before creating the root")
}

func TestOpenAndLockExclusiveLocksEachCreatedComponentBeforeDescending(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(first, "second")
	target := filepath.Join(second, "vault")
	var contender *Lock

	root, lock, err := (Layout{Root: target}).createAndLockExclusiveWith(
		func(index int, _ *os.Root) error {
			if index != 0 {
				return nil
			}
			var lockErr error
			contender, lockErr = (Layout{Root: first}).TryLockExclusive()
			return lockErr
		})
	if root != nil {
		_ = root.Close()
	}
	if lock != nil {
		_ = lock.Release()
	}
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NotNil(t, contender)
	defer func() { _ = contender.Release() }()
	_, err = os.Lstat(second)
	require.ErrorIs(t, err, os.ErrNotExist,
		"a newly claimed component must stop startup before deeper creation")
}

func TestTryLockLaunchDoesNotCreateVaultRoot(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing", "vault")
	lock, err := (Layout{Root: target}).TryLockLaunch()
	require.NoError(t, err)
	_, err = (Layout{Root: target}).TryLockLaunch()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, lock.Release())
	_, err = os.Lstat(filepath.Dir(target))
	require.ErrorIs(t, err, os.ErrNotExist,
		"launch coordination must live outside the unowned vault tree")
}

func TestTryLockLaunchUsesFilesystemIdentityForAliases(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Mkdir(realParent, 0o700))
	require.NoError(t, os.Symlink(realParent, alias))

	lock, err := (Layout{Root: filepath.Join(realParent, "vault")}).TryLockLaunch()
	require.NoError(t, err)
	defer func() { _ = lock.Release() }()
	_, err = (Layout{Root: filepath.Join(alias, "vault")}).TryLockLaunch()
	require.ErrorIs(t, err, ErrVaultLocked,
		"aliases of one missing vault must share the ancestor-identity launch lock")
}

func TestTryLockLaunchRemainsStableWhenVaultAppears(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "new", "vault")
	lock, err := (Layout{Root: target}).TryLockLaunch()
	require.NoError(t, err)
	defer func() { _ = lock.Release() }()
	require.NoError(t, os.MkdirAll(target, 0o700))

	_, err = (Layout{Root: target}).TryLockLaunch()
	require.ErrorIs(t, err, ErrVaultLocked,
		"post-creation launch keys must retain the pre-creation ancestor keys")
}

func TestTryLockLaunchSerializesDifferentVaults(t *testing.T) {
	base := t.TempDir()
	first, err := (Layout{Root: filepath.Join(base, "one")}).TryLockLaunch()
	require.NoError(t, err)
	defer func() { _ = first.Release() }()
	_, err = (Layout{Root: filepath.Join(base, "two")}).TryLockLaunch()
	require.ErrorIs(t, err, ErrVaultLocked,
		"the short-lived per-user launch lock must serialize all daemon starters")
}

func TestOpenLaunchOutputStaysOutsideVault(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing", "vault")
	f, path, err := (Layout{Root: target}).OpenLaunchOutput()
	require.NoError(t, err)
	defer func() { _ = os.Remove(path) }()
	require.NoError(t, f.Close())
	require.NotEqual(t, target, filepath.Dir(path))
	_, err = os.Lstat(filepath.Dir(target))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTargetLockRegistryIgnoresProcessHomeEnvironment(t *testing.T) {
	before, err := targetLockRegistryDir()
	require.NoError(t, err)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	after, err := targetLockRegistryDir()
	require.NoError(t, err)
	require.Equal(t, before, after)
}
