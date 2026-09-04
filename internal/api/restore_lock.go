package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
)

type restoreTargetCoordinator interface {
	backup.RestoreTargetCoordinator
	Prepare(ctx context.Context) error
	ReleasePreparation() error
}

type retainedRestoreTargetCoordinator struct {
	restoreTargetCoordinator

	targetLease backup.RestoreTargetLease
}

func (c *retainedRestoreTargetCoordinator) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (backup.RestoreTargetLease, error) {
	lease, err := c.restoreTargetCoordinator.AcquireRestoreTarget(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("acquiring retained restore target: %w", err)
	}
	c.targetLease = lease
	return retainedRestoreTargetLease{}, nil
}

func (c *retainedRestoreTargetCoordinator) Release() error {
	var err error
	if c.targetLease != nil {
		err = c.targetLease.Release()
		c.targetLease = nil
	}
	return errors.Join(err, c.ReleasePreparation())
}

type retainedRestoreTargetLease struct{}

func (retainedRestoreTargetLease) Release() error { return nil }

func newRestoreTargetCoordinator(
	target, repoRoot, vaultRoot string, overwrite bool,
) restoreTargetCoordinator {
	return &platformRestoreTargetCoordinator{
		inner: backupapp.NewRestoreTargetCoordinator(target, repoRoot, vaultRoot, overwrite),
	}
}

type platformRestoreTargetCoordinator struct {
	inner *backupapp.RestoreTargetCoordinator
}

func (c *platformRestoreTargetCoordinator) Prepare(ctx context.Context) error {
	return restoreTargetProblem("preparing backup restore target", c.inner.Prepare(ctx))
}

func (c *platformRestoreTargetCoordinator) ReleasePreparation() error {
	return restoreTargetProblem("releasing backup restore target preparation", c.inner.ReleasePreparation())
}

func (c *platformRestoreTargetCoordinator) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (backup.RestoreTargetLease, error) {
	lease, err := c.inner.AcquireRestoreTarget(ctx, root)
	return lease, restoreTargetProblem("acquiring backup restore target", err)
}

func restoreTargetProblem(action string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, backupapp.ErrRestoreTargetActive) {
		return NewError(http.StatusConflict, "backup_restore_target_active",
			"backup restore target overlaps another restore or running docbank daemon")
	}
	if errors.Is(err, backupapp.ErrRestoreTargetNotEmpty) {
		return NewError(http.StatusConflict, "backup_restore_target_not_empty",
			"backup restore target is not empty; set overwrite to merge into it")
	}
	if errors.Is(err, backupapp.ErrRestoreTargetChanged) ||
		errors.Is(err, backupapp.ErrRestoreTargetOverlap) {
		return NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	return NewError(http.StatusInternalServerError, "backup_failed", fmt.Sprintf("%s: %v", action, err))
}
