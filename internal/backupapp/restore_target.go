package backupapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/home"
)

var (
	ErrRestoreTargetActive   = errors.New("backup restore target is active")
	ErrRestoreTargetChanged  = errors.New("backup restore target changed")
	ErrRestoreTargetNotEmpty = errors.New("backup restore target is not empty")
	ErrRestoreTargetOverlap  = errors.New("backup restore target overlaps protected storage")
)

// RestoreTargetCoordinator confines a restore to a stable target disjoint
// from the live vault and repository while coordinating with other vault and
// restore owners through the shared hierarchy locks.
type RestoreTargetCoordinator struct {
	target         string
	repositoryRoot string
	vaultRoot      string
	overwrite      bool
	launch         *home.Lock
	ancestors      *home.Lock
}

func NewRestoreTargetCoordinator(
	target, repositoryRoot, vaultRoot string, overwrite bool,
) *RestoreTargetCoordinator {
	return &RestoreTargetCoordinator{
		target: target, repositoryRoot: repositoryRoot, vaultRoot: vaultRoot,
		overwrite: overwrite,
	}
}

func (c *RestoreTargetCoordinator) Prepare(ctx context.Context) error {
	for {
		launch, err := (home.Layout{Root: c.target}).TryLockLaunch()
		if err == nil {
			c.launch = launch
			break
		}
		if !errors.Is(err, home.ErrVaultLocked) {
			return fmt.Errorf("serializing backup restore target creation: %w", err)
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
		if errors.Is(err, home.ErrVaultLocked) {
			return fmt.Errorf("locking backup restore target ancestors: %w", ErrRestoreTargetActive)
		}
		return fmt.Errorf("locking backup restore target ancestors: %w", err)
	}
	c.ancestors = ancestors
	return nil
}

func (c *RestoreTargetCoordinator) ReleasePreparation() error {
	var result error
	if c.ancestors != nil {
		result = c.ancestors.Release()
		c.ancestors = nil
	}
	if c.launch != nil {
		result = errors.Join(result, c.launch.Release())
		c.launch = nil
	}
	return result
}

func (c *RestoreTargetCoordinator) AcquireRestoreTarget(
	ctx context.Context, root *os.Root,
) (backup.RestoreTargetLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.validate(root); err != nil {
		return nil, err
	}
	if c.ancestors != nil {
		if err := c.ancestors.Release(); err != nil {
			return nil, fmt.Errorf("releasing backup restore ancestor preparation: %w", err)
		}
		c.ancestors = nil
	}
	lock, err := (home.Layout{Root: c.target}).TryLockExclusiveRoot(root)
	if err != nil {
		if errors.Is(err, home.ErrVaultLocked) {
			return nil, fmt.Errorf("locking backup restore target: %w", ErrRestoreTargetActive)
		}
		return nil, fmt.Errorf("locking backup restore target: %w", err)
	}
	if err := c.validate(root); err != nil {
		return nil, errors.Join(err, lock.Release())
	}
	if c.launch != nil {
		if err := c.launch.Release(); err != nil {
			return nil, errors.Join(fmt.Errorf(
				"releasing backup restore target preparation: %w", err), lock.Release())
		}
		c.launch = nil
	}
	return lock, nil
}

func (c *RestoreTargetCoordinator) validate(root *os.Root) error {
	leaf, err := os.Lstat(c.target)
	if err != nil {
		return fmt.Errorf("checking backup restore target: %w", err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup restore target was replaced with a symlink: %w", ErrRestoreTargetChanged)
	}
	held, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("checking held backup restore target: %w", err)
	}
	if !os.SameFile(leaf, held) {
		return fmt.Errorf("backup restore target was replaced while acquiring coordination: %w",
			ErrRestoreTargetChanged)
	}
	if err := ValidateDisjointRoots(c.target, c.repositoryRoot, c.vaultRoot); err != nil {
		return err
	}
	if !c.overwrite {
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return fmt.Errorf("reading held backup restore target: %w", err)
		}
		for _, entry := range entries {
			if entry.Name() != "vault.lock" {
				return ErrRestoreTargetNotEmpty
			}
		}
	}
	return nil
}

// ValidateDisjointRoots rejects lexical, symlink, case-equivalent, and
// filesystem-identity aliases between root and protected storage.
func ValidateDisjointRoots(root string, protected ...string) error {
	canonicalRoot, err := home.CanonicalRoot(root)
	if err != nil {
		return err
	}
	for _, candidate := range protected {
		if candidate == "" {
			continue
		}
		canonicalProtected, err := home.CanonicalRoot(candidate)
		if err != nil {
			return err
		}
		overlaps, err := restorePathsOverlap(canonicalRoot, canonicalProtected)
		if err != nil {
			return err
		}
		if overlaps {
			return fmt.Errorf("backup roots %q and %q overlap: %w",
				canonicalRoot, canonicalProtected, ErrRestoreTargetOverlap)
		}
	}
	return nil
}

func restorePathsOverlap(a, b string) (bool, error) {
	if restorePathContains(a, b) || restorePathContains(b, a) {
		return true, nil
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		matched, err := restoreExistingAncestorMatches(pair[0], pair[1])
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func restoreExistingAncestorMatches(path, protected string) (bool, error) {
	protectedInfo, err := os.Stat(protected)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil && os.SameFile(info, protectedInfo) {
			return true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
	}
}

func restorePathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
