package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/home"
)

func TestBackupRestoreCoordinatorExcludesRestoreAndDaemon(t *testing.T) {
	target := filepath.Join(t.TempDir(), "restore-target")
	require.NoError(t, os.MkdirAll(target, 0o700))
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	coordinator := newRestoreTargetCoordinator(target, "", "", true)
	lease, err := coordinator.AcquireRestoreTarget(t.Context(), root)
	require.NoError(t, err)

	_, err = (home.Layout{Root: target}).TryLockExclusive()
	require.ErrorIs(t, err, home.ErrVaultLocked,
		"daemon startup must conflict with a restore-held target")
	competingRoot, err := os.OpenRoot(target)
	require.NoError(t, err)
	defer func() { _ = competingRoot.Close() }()
	_, err = coordinator.AcquireRestoreTarget(t.Context(), competingRoot)
	var problem *Error
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, "backup_restore_target_active", problem.Code)

	require.NoError(t, lease.Release())
	retryLock, err := (home.Layout{Root: target}).TryLockExclusive()
	require.NoError(t, err, "restore completion must release target ownership")
	require.NoError(t, retryLock.Release())
	lockInfo, err := os.Stat(filepath.Join(target, "vault.lock"))
	require.NoError(t, err)
	assert.True(t, lockInfo.Mode().IsRegular())
}

func TestBackupRestoreCoordinatorPinsTargetAcrossPathSwap(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	parked := filepath.Join(base, "parked")
	repository := filepath.Join(base, "repository")
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.MkdirAll(repository, 0o700))
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	coordinator := newRestoreTargetCoordinator(target, repository, "", true)
	lease, err := coordinator.AcquireRestoreTarget(t.Context(), root)
	require.NoError(t, err)
	defer func() { _ = lease.Release() }()

	require.NoError(t, os.Rename(target, parked))
	if err := os.Symlink(repository, target); err != nil {
		t.Skipf("creating a symlink requires additional platform permission: %v", err)
	}
	require.NoError(t, root.WriteFile("descriptor-write", []byte("pinned"), 0o600))
	content, err := os.ReadFile(filepath.Join(parked, "descriptor-write"))
	require.NoError(t, err)
	assert.Equal(t, "pinned", string(content))
	_, err = os.Stat(filepath.Join(repository, "descriptor-write"))
	require.ErrorIs(t, err, os.ErrNotExist,
		"writes through Kit's borrowed root must not follow the replaced pathname")
}

func TestBackupRestoreCoordinatorRejectsSwapBeforeAcquisition(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	parked := filepath.Join(base, "parked")
	repository := filepath.Join(base, "repository")
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.MkdirAll(repository, 0o700))
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()
	require.NoError(t, os.Rename(target, parked))
	if err := os.Symlink(repository, target); err != nil {
		t.Skipf("creating a symlink requires additional platform permission: %v", err)
	}

	coordinator := newRestoreTargetCoordinator(target, repository, "", true)
	_, err = coordinator.AcquireRestoreTarget(t.Context(), root)
	var problem *Error
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, "validation", problem.Code)
	_, err = os.Stat(filepath.Join(repository, "vault.lock"))
	require.ErrorIs(t, err, os.ErrNotExist,
		"rejected replacement must not receive lock or restore files")
}

func TestBackupRestoreCoordinatorRejectsTargetBelowAnotherActiveVault(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "other-vault")
	target := filepath.Join(vault, "nested-restore")
	require.NoError(t, os.MkdirAll(target, 0o700))
	daemonLock, err := (home.Layout{Root: vault}).TryLockExclusive()
	require.NoError(t, err)
	defer func() { _ = daemonLock.Release() }()
	root, err := os.OpenRoot(target)
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	coordinator := newRestoreTargetCoordinator(target, "", "", true)
	_, err = coordinator.AcquireRestoreTarget(t.Context(), root)
	var problem *Error
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, "backup_restore_target_active", problem.Code)
}

func TestBackupRestoreCoordinatorExcludesOverlappingTargets(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "restore")
	child := filepath.Join(parent, "nested")
	require.NoError(t, os.MkdirAll(child, 0o700))
	parentRoot, err := os.OpenRoot(parent)
	require.NoError(t, err)
	defer func() { _ = parentRoot.Close() }()
	childRoot, err := os.OpenRoot(child)
	require.NoError(t, err)
	defer func() { _ = childRoot.Close() }()
	parentCoordinator := newRestoreTargetCoordinator(parent, "", "", true)
	childCoordinator := newRestoreTargetCoordinator(child, "", "", true)

	parentLease, err := parentCoordinator.AcquireRestoreTarget(t.Context(), parentRoot)
	require.NoError(t, err)
	_, err = childCoordinator.AcquireRestoreTarget(t.Context(), childRoot)
	assertRestoreTargetActive(t, err)
	require.NoError(t, parentLease.Release())

	childLease, err := childCoordinator.AcquireRestoreTarget(t.Context(), childRoot)
	require.NoError(t, err)
	defer func() { _ = childLease.Release() }()
	_, err = parentCoordinator.AcquireRestoreTarget(t.Context(), parentRoot)
	assertRestoreTargetActive(t, err)
}

func TestBackupRestoreCoordinatorRejectsMissingDescendantBeforeCreation(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "active-restore")
	descendant := filepath.Join(parent, "docbank.db")
	require.NoError(t, os.Mkdir(parent, 0o700))
	parentRoot, err := os.OpenRoot(parent)
	require.NoError(t, err)
	defer func() { _ = parentRoot.Close() }()
	parentCoordinator := newRestoreTargetCoordinator(parent, "", "", true)
	parentLease, err := parentCoordinator.AcquireRestoreTarget(t.Context(), parentRoot)
	require.NoError(t, err)
	defer func() { _ = parentLease.Release() }()

	descendantCoordinator := newRestoreTargetCoordinator(descendant, "", "", true)
	err = descendantCoordinator.Prepare(t.Context())
	assertRestoreTargetActive(t, err)
	_, err = os.Lstat(descendant)
	require.ErrorIs(t, err, os.ErrNotExist,
		"coordination must reject a missing descendant before Kit can create it")
}

func assertRestoreTargetActive(t *testing.T, err error) {
	t.Helper()
	var problem *Error
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, "backup_restore_target_active", problem.Code)
}
