package api

import "go.kenn.io/kit/backup"

func newRestoreTargetCoordinator(
	target, repoRoot, vaultRoot string, overwrite bool,
) backup.RestoreTargetCoordinator {
	return platformRestoreTargetCoordinator{
		target: target, repoRoot: repoRoot, vaultRoot: vaultRoot, overwrite: overwrite,
	}
}
