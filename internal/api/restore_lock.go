package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"time"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/home"
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

type platformRestoreTargetCoordinator struct {
	target    string
	repoRoot  string
	vaultRoot string
	overwrite bool
	launch    *home.Lock
	ancestors *home.Lock
}

func (c *platformRestoreTargetCoordinator) Prepare(ctx context.Context) error {
	for {
		launch, err := (home.Layout{Root: c.target}).TryLockLaunch()
		if err == nil {
			c.launch = launch
			break
		}
		if !errors.Is(err, home.ErrVaultLocked) {
			return c.lockError("serializing backup restore target creation", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	ancestors, err := (home.Layout{Root: c.target}).TryLockExistingAncestors()
	if err != nil {
		_ = c.ReleasePreparation()
		return c.lockError("locking backup restore target ancestors", err)
	}
	c.ancestors = ancestors
	return nil
}

func (c *platformRestoreTargetCoordinator) ReleasePreparation() error {
	err := c.releaseAncestors()
	err = errors.Join(err, c.releaseLaunch())
	return err
}

func (c *platformRestoreTargetCoordinator) releaseAncestors() error {
	var err error
	if c.ancestors != nil {
		err = c.ancestors.Release()
		c.ancestors = nil
	}
	return err
}

func (c *platformRestoreTargetCoordinator) releaseLaunch() error {
	var err error
	if c.launch != nil {
		err = c.launch.Release()
		c.launch = nil
	}
	return err
}

func (c *platformRestoreTargetCoordinator) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (backup.RestoreTargetLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.validate(root); err != nil {
		return nil, err
	}
	// Preparation holds shared ancestry while Kit opens or creates the pinned
	// target. Retain the global creation mutex while replacing those shared
	// locks with exclusive target ownership so no coordinated starter can enter
	// the transition gap.
	if err := c.releaseAncestors(); err != nil {
		return nil, NewError(http.StatusInternalServerError, "backup_failed",
			fmt.Sprintf("releasing backup restore ancestor preparation: %v", err))
	}
	lock, err := (home.Layout{Root: c.target}).TryLockExclusiveRoot(root)
	if err != nil {
		return nil, c.lockError("locking backup restore target", err)
	}
	if err := c.validate(root); err != nil {
		_ = lock.Release()
		return nil, err
	}
	if err := c.releaseLaunch(); err != nil {
		_ = lock.Release()
		return nil, NewError(http.StatusInternalServerError, "backup_failed",
			fmt.Sprintf("releasing backup restore target preparation: %v", err))
	}
	return lock, nil
}

func (c *platformRestoreTargetCoordinator) lockError(action string, err error) error {
	if errors.Is(err, home.ErrVaultLocked) {
		return NewError(http.StatusConflict, "backup_restore_target_active",
			"backup restore target overlaps another restore or running docbank daemon")
	}
	return NewError(http.StatusInternalServerError, "backup_failed",
		fmt.Sprintf("%s: %v", action, err))
}

func (c *platformRestoreTargetCoordinator) validate(root *os.Root) error {
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
