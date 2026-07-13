//go:build unix

package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/home"
)

type platformRestoreTargetCoordinator struct {
	target    string
	repoRoot  string
	vaultRoot string
	overwrite bool
}

func (c platformRestoreTargetCoordinator) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (backup.RestoreTargetLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.validate(root); err != nil {
		return nil, err
	}
	lock, err := (home.Layout{Root: c.target}).TryLockRestoreRoot(root)
	if err != nil {
		if errors.Is(err, home.ErrVaultLocked) {
			return nil, NewError(http.StatusConflict, "backup_restore_target_active",
				"backup restore target overlaps another restore or running docbank daemon")
		}
		return nil, NewError(http.StatusInternalServerError, "backup_failed",
			fmt.Sprintf("locking backup restore target: %v", err))
	}
	if err := c.validate(root); err != nil {
		_ = lock.Release()
		return nil, err
	}
	return lock, nil
}

func (c platformRestoreTargetCoordinator) validate(root *os.Root) error {
	leaf, err := os.Lstat(c.target)
	if err != nil {
		return NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("checking backup restore target: %v", err))
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		return NewError(http.StatusUnprocessableEntity, "validation",
			"backup restore target was replaced with a symlink")
	}
	held, err := root.Stat(".")
	if err != nil {
		return NewError(http.StatusInternalServerError, "backup_failed",
			fmt.Sprintf("checking held backup restore target: %v", err))
	}
	if !os.SameFile(leaf, held) {
		return NewError(http.StatusUnprocessableEntity, "validation",
			"backup restore target was replaced while acquiring coordination")
	}
	for _, protected := range []struct {
		path    string
		detail  string
		failure string
	}{
		{c.repoRoot, "backup restore target must be disjoint from the backup repository",
			"checking backup repository overlap"},
		{c.vaultRoot, "backup restore target must be disjoint from the running vault",
			"checking live vault overlap"},
	} {
		if protected.path == "" {
			continue
		}
		overlaps, overlapErr := pathsOverlap(c.target, protected.path)
		if overlapErr != nil {
			return NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("%s: %v", protected.failure, overlapErr))
		}
		if overlaps {
			return NewError(http.StatusUnprocessableEntity, "validation", protected.detail)
		}
	}
	if !c.overwrite {
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("reading held backup restore target: %v", err))
		}
		if restoreTargetHasPayload(entries) {
			return NewError(http.StatusConflict, "backup_restore_target_not_empty",
				"backup restore target is not empty; set overwrite to merge into it")
		}
	}
	return nil
}
