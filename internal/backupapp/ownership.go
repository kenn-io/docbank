package backupapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/backup"
)

const repositoryOwnerFile = "docbank-owner"

var (
	ErrRepositoryOwnerConflict = errors.New("backup repository belongs to another vault")
	ErrRepositoryClaimRequired = errors.New("backup repository has unclaimed history; run docbank backup claim before capturing")
	ErrRepositoryClaimChanged  = errors.New("backup repository history changed; preview it again before confirming")
)

// RepositoryClaim previews the history an operator would assign to this vault.
type RepositoryClaim struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	SnapshotCount int    `json:"snapshot_count"`
	NewestAt      string `json:"newest_at,omitzero"`
	Owned         bool   `json:"owned"`
	// Preview identifies this exact history and must accompany confirmation.
	Preview string `json:"preview"`
}

// EnsureRepositoryOwner binds empty repositories and refuses unclaimed history.
// Every capture must check this before asking Kit to write a snapshot.
func EnsureRepositoryOwner(ctx context.Context, repo *backup.Repo, vaultID string, forceUnlock bool) error {
	if owned, err := repositoryOwned(repo, vaultID); owned || err != nil {
		return err
	}
	_, err := repositoryOwnership(ctx, repo, vaultID, true, false, forceUnlock, "")
	return err
}

// ClaimRepository previews ownership, or adopts existing history when confirmed.
// Confirmation must carry the preceding Preview; an existing owner cannot be replaced.
func ClaimRepository(ctx context.Context, repo *backup.Repo, vaultID string, confirm bool, preview string) (RepositoryClaim, error) {
	return repositoryOwnership(ctx, repo, vaultID, false, confirm, false, preview)
}

func repositoryOwned(repo *backup.Repo, vaultID string) (bool, error) {
	if vaultID == "" {
		return false, errors.New("backup repository requires a vault identity")
	}
	owner, err := os.ReadFile(repo.Path(repositoryOwnerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading backup repository owner: %w", err)
	}
	if strings.TrimSpace(string(owner)) != vaultID {
		return false, ErrRepositoryOwnerConflict
	}
	return true, nil
}

func repositoryOwnership(ctx context.Context, repo *backup.Repo, vaultID string, capture, confirm, forceUnlock bool, expected string) (preview RepositoryClaim, err error) {
	lock, err := repo.AcquireExclusiveLockContext(ctx, "docbank-owner", forceUnlock)
	if err != nil {
		return preview, fmt.Errorf("locking backup repository ownership: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
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
	history := sha256.New()
	_, _ = fmt.Fprintln(history, preview.ID)
	for _, manifest := range manifests {
		_, _ = fmt.Fprintln(history, manifest.SnapshotID)
	}
	preview.Preview = hex.EncodeToString(history.Sum(nil))
	if len(manifests) > 0 {
		preview.NewestAt = manifests[len(manifests)-1].CreatedAt
	}
	if owned {
		return preview, nil
	}
	if capture && len(manifests) > 0 {
		return preview, ErrRepositoryClaimRequired
	}
	if confirm && expected != preview.Preview {
		return preview, ErrRepositoryClaimChanged
	}
	if capture || confirm {
		if err := atomicfile.WriteNew(repo.Path(repositoryOwnerFile), []byte(vaultID+"\n")); err != nil {
			return preview, fmt.Errorf("writing backup repository owner: %w", err)
		}
		preview.Owned = true
	}
	return preview, nil
}
