//go:build !unix

package api

import (
	"context"
	"os"

	"go.kenn.io/kit/backup"
)

type platformRestoreTargetCoordinator struct {
	target    string
	repoRoot  string
	vaultRoot string
	overwrite bool
}

type noopRestoreTargetLease struct{}

func (noopRestoreTargetLease) Release() error { return nil }

func (platformRestoreTargetCoordinator) AcquireRestoreTarget(
	context.Context, *os.Root,
) (backup.RestoreTargetLease, error) {
	return noopRestoreTargetLease{}, nil
}
