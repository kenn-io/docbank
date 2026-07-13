package api

import (
	"context"

	"go.kenn.io/kit/backup"
)

type restoreTargetCoordinator interface {
	backup.RestoreTargetCoordinator
	Prepare(ctx context.Context) error
	ReleasePreparation() error
}

func newRestoreTargetCoordinator(
	target, repoRoot, vaultRoot string, overwrite bool,
) restoreTargetCoordinator {
	return &platformRestoreTargetCoordinator{
		target: target, repoRoot: repoRoot, vaultRoot: vaultRoot, overwrite: overwrite,
	}
}
