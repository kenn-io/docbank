package docbank

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/home"
)

// ResetOptions defines the explicit move-aside boundary for ResetVault.
// ReleaseCurrent, when set, is called exactly once after reset excludes every
// overlapping hierarchy-owner acquisition and validates both paths, but before
// it takes ordinary ownership of the source directory. Embedded applications
// use it to hand an already-open vault and any surrounding lifecycle state to
// reset without an ownership gap.
type ResetOptions struct {
	DiagnosticRoot string
	ReleaseCurrent func() error
}

// ResetVault atomically moves an existing vault to an absent diagnostic
// sibling and creates a fresh vault at the original canonical path. It never
// opens the source catalog and never deletes the source, diagnostic, or a
// partially initialized fresh vault. The returned fresh vault retains ordinary
// exclusive hierarchy ownership until Close.
func ResetVault(
	ctx context.Context, config Config, opts ResetOptions,
) (fresh *Vault, retErr error) {
	requestedRoot := filepath.Clean(config.Root)
	config, blobOptions, err := normalizeVaultConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	err = validateResetSourcePath(requestedRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("checking requested docbank reset source: %w", err)
	}
	if opts.DiagnosticRoot == "" {
		return nil, errors.New("docbank reset diagnostic root is required")
	}
	diagnosticRoot, err := home.CanonicalRoot(opts.DiagnosticRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving docbank reset diagnostic root: %w", err)
	}
	sourceRoot := config.Root
	if sourceRoot == diagnosticRoot || filepath.Dir(sourceRoot) != filepath.Dir(diagnosticRoot) {
		return nil, errors.New("docbank reset diagnostic root must be an absent sibling of the vault root")
	}
	if err := validateResetPaths(sourceRoot, diagnosticRoot); err != nil {
		return nil, err
	}

	sourceLayout := home.Layout{Root: sourceRoot}
	transition, err := sourceLayout.TryLockOwnershipTransition()
	if err != nil {
		return nil, fmt.Errorf("excluding docbank vault ownership during reset: %w", err)
	}
	moved := false
	defer func() {
		releaseErr := transition.Release()
		if releaseErr == nil {
			return
		}
		if fresh != nil {
			releaseErr = errors.Join(releaseErr, fresh.Close())
			fresh = nil
		}
		if moved {
			releaseErr = fmt.Errorf(
				"releasing reset coordination for fresh docbank vault at %s after moving the original to %s: %w",
				sourceRoot, diagnosticRoot, releaseErr,
			)
		}
		retErr = errors.Join(retErr, releaseErr)
	}()
	if err := validateResetPaths(sourceRoot, diagnosticRoot); err != nil {
		return nil, err
	}
	if opts.ReleaseCurrent != nil {
		if err := opts.ReleaseCurrent(); err != nil {
			return nil, fmt.Errorf("releasing current docbank vault before reset: %w", err)
		}
	}

	root, err := transition.OpenExistingForReplacement()
	if err != nil {
		return nil, fmt.Errorf("locking existing docbank vault for reset: %w", err)
	}
	heldIdentity, identityErr := root.Stat(".")
	sourceErr := validateResetSourcePath(sourceRoot)
	closeErr := errors.Join(root.Close(), transition.ReleaseSourceForReplacement())
	if err := errors.Join(identityErr, sourceErr, closeErr); err != nil {
		return nil, fmt.Errorf("validating existing docbank vault before reset rename: %w", err)
	}
	currentIdentity, err := os.Stat(sourceRoot)
	if err != nil || !os.SameFile(heldIdentity, currentIdentity) {
		if err != nil {
			return nil, fmt.Errorf("rechecking docbank reset source: %w", err)
		}
		return nil, errors.New("docbank reset source changed before rename")
	}
	if err := renameVaultNoReplace(sourceRoot, diagnosticRoot); err != nil {
		return nil, fmt.Errorf("moving docbank vault aside: %w", err)
	}
	moved = true

	fresh, err = openVaultWithRootOpener(config, blobOptions, sourceLayout, func() (
		*os.Root, *home.Lock, error,
	) {
		return transition.OpenAndLockReplacement()
	})
	if err != nil {
		return nil, fmt.Errorf(
			"opening fresh docbank vault at %s after moving the original to %s: %w",
			sourceRoot, diagnosticRoot, err,
		)
	}
	return fresh, nil
}

func validateResetPaths(sourceRoot, diagnosticRoot string) error {
	if err := validateResetSourcePath(sourceRoot); err != nil {
		return fmt.Errorf("checking docbank reset source: %w", err)
	}
	_, err := os.Lstat(diagnosticRoot)
	if err == nil {
		return fmt.Errorf("docbank reset diagnostic destination already exists: %s", diagnosticRoot)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking docbank reset diagnostic destination: %w", err)
	}
	return nil
}
