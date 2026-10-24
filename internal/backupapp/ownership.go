package backupapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"go.kenn.io/docbank/internal/store"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/backup"
)

const repositoryOwnerFile = "docbank-owner"

var (
	ErrRepositoryOwnerConflict = errors.New("backup repository owner marker does not name this vault; select this vault's repository or initialize a new one")
	ErrRepositoryClaimRequired = errors.New("backup repository has unclaimed history; run docbank backup claim before capturing")
)

// RepositoryClaim previews the history an operator would assign to this vault.
type RepositoryClaim struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	SnapshotCount int    `json:"snapshot_count"`
	NewestAt      string `json:"newest_at,omitzero"`
	Owned         bool   `json:"owned"`
}

// EnsureCaptureAllowed checks the restored vault's claim and repository owner
// before asking Kit to write a snapshot.
func EnsureCaptureAllowed(ctx context.Context, repo *backup.Repo, metadata *store.Store, forceUnlock bool) error {
	required, err := metadata.BackupClaimRequired(ctx)
	if err != nil {
		return err
	}
	if required {
		return ErrRepositoryClaimRequired
	}
	return EnsureRepositoryOwner(ctx, repo, metadata.VaultID(), forceUnlock)
}

// EnsureRepositoryOwner binds empty repositories and refuses unclaimed history.
func EnsureRepositoryOwner(ctx context.Context, repo *backup.Repo, vaultID string, forceUnlock bool) error {
	if owned, err := repositoryOwned(repo, vaultID); owned || err != nil {
		return err
	}
	_, err := repositoryOwnership(ctx, repo, vaultID, true, false, forceUnlock)
	return err
}

// ClaimRepository previews ownership, or adopts existing history when confirmed.
// An existing owner cannot be replaced.
func ClaimRepository(ctx context.Context, repo *backup.Repo, metadata *store.Store, confirm bool) (RepositoryClaim, error) {
	required, err := metadata.BackupClaimRequired(ctx)
	if err != nil {
		return RepositoryClaim{}, err
	}
	preview, err := repositoryOwnership(ctx, repo, metadata.VaultID(), false, confirm, false)
	if err != nil {
		return preview, err
	}
	if required {
		preview.Owned = confirm
		if confirm {
			if err := metadata.ConfirmBackupClaim(ctx); err != nil {
				return preview, err
			}
		}
	}
	return preview, nil
}

func repositoryOwned(repo *backup.Repo, vaultID string) (bool, error) {
	owner, err := os.ReadFile(repo.Path(repositoryOwnerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading backup repository owner: %w", err)
	}
	identity := strings.TrimSpace(string(owner))
	if identity != vaultID {
		return false, ErrRepositoryOwnerConflict
	}
	return true, nil
}

func repositoryOwnership(ctx context.Context, repo *backup.Repo, vaultID string, capture, confirm, forceUnlock bool) (preview RepositoryClaim, err error) {
	if capture || confirm {
		lock, err := repo.AcquireExclusiveLockContext(ctx, "docbank-owner", forceUnlock)
		if err != nil {
			return preview, fmt.Errorf("locking backup repository ownership: %w", err)
		}
		defer func() { err = errors.Join(err, lock.Release()) }()
	}
	owned, err := repositoryOwned(repo, vaultID)
	if err != nil {
		return preview, err
	}
	if owned && capture {
		return preview, nil
	}
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return preview, fmt.Errorf("reading backup repository history: %w", err)
	}
	preview = RepositoryClaim{ID: repo.Config().RepoID, Path: repo.Root(), SnapshotCount: len(manifests), Owned: owned}
	if len(manifests) > 0 {
		preview.NewestAt = manifests[len(manifests)-1].CreatedAt
	}
	if owned {
		return preview, nil
	}
	if capture && len(manifests) > 0 {
		return preview, ErrRepositoryClaimRequired
	}
	if capture || confirm {
		if err := atomicfile.WriteNew(repo.Path(repositoryOwnerFile), []byte(vaultID+"\n")); err != nil {
			return preview, fmt.Errorf("writing backup repository owner: %w", err)
		}
		preview.Owned = true
	}
	return preview, nil
}
