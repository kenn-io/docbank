//go:build unix

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/home"
)

func TestBackupRestoreLockExcludesRestoreAndDaemonStart(t *testing.T) {
	target := filepath.Join(t.TempDir(), "restore-target")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	forcedErr := errors.New("forced restore failure")

	go func() {
		_, err := restoreBackupSnapshotWith(t.Context(), nil, target,
			backupRestoreRequest{Overwrite: true}, nil,
			func(context.Context, *backup.Repo, string, backup.RestoreOptions) (*backup.RestoreResult, error) {
				close(entered)
				<-release
				return nil, forcedErr
			})
		firstDone <- err
	}()
	<-entered

	// Daemon startup uses this exact lock before it initializes the vault.
	_, err := (home.Layout{Root: target}).TryLockExclusive()
	require.ErrorIs(t, err, home.ErrVaultLocked)

	_, err = restoreBackupSnapshotWith(t.Context(), nil, target,
		backupRestoreRequest{Overwrite: true}, nil,
		func(context.Context, *backup.Repo, string, backup.RestoreOptions) (*backup.RestoreResult, error) {
			t.Fatal("competing restore ran after failing to acquire the target lock")
			return nil, errors.New("unreachable competing restore")
		})
	var problem *Error
	require.ErrorAs(t, err, &problem)
	assert.Equal(t, "backup_restore_target_active", problem.Code)

	close(release)
	err = <-firstDone
	require.Error(t, err)
	assert.Contains(t, err.Error(), forcedErr.Error())
	_, err = os.Stat(filepath.Join(target, "vault.lock"))
	require.ErrorIs(t, err, os.ErrNotExist,
		"a failed restore must remove the lock file it created")
}

func TestBackupRestoreLeavesVaultLockAfterSuccess(t *testing.T) {
	target := filepath.Join(t.TempDir(), "restore-target")
	_, err := restoreBackupSnapshotWith(t.Context(), nil, target,
		backupRestoreRequest{}, nil,
		func(context.Context, *backup.Repo, string, backup.RestoreOptions) (*backup.RestoreResult, error) {
			return &backup.RestoreResult{SnapshotID: "snapshot"}, nil
		})
	require.NoError(t, err)
	lockInfo, err := os.Stat(filepath.Join(target, "vault.lock"))
	require.NoError(t, err)
	assert.True(t, lockInfo.Mode().IsRegular())
}
