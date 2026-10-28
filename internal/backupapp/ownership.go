package backupapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"go.kenn.io/docbank/internal/store"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/backup"
)

const repositoryOwnerFile = "docbank-owner"

var (
	ErrRepositoryOwnerConflict = errors.New("backup repository belongs to another vault; select this vault's repository or initialize a new one")
	ErrRepositoryClaimRequired = errors.New("backup repository has history this vault installation has not claimed, as after upgrading or restoring; run docbank backup claim before capturing")
	ErrRepositoryClaimChanged  = errors.New("backup repository changed after the claim preview; review the repository again before claiming")
)

// RepositoryClaim previews the history an operator would assign to this vault
// installation. Fingerprint binds a confirmation to exactly this preview.
type RepositoryClaim struct {
	ID            string `json:"id"`
	Path          string `json:"path"`
	SnapshotCount int    `json:"snapshot_count"`
	NewestAt      string `json:"newest_at,omitzero"`
	Owned         bool   `json:"owned"`
	// Takeover means another installation of this vault, such as the original
	// of a restored copy, owns the repository and loses capture access on claim.
	Takeover    bool   `json:"takeover,omitzero"`
	Fingerprint string `json:"fingerprint"`
}

// repositoryOwner is the docbank-owner marker. The processing incarnation
// identifies one installation of a vault: restores start a new incarnation
// and upgrades keep it, so a restored copy cannot capture into the original's
// repository until an operator claims it.
type repositoryOwner struct {
	VaultID       string `json:"vault_id"`
	IncarnationID string `json:"incarnation_id"`
}

type ownership int

const (
	unmarked ownership = iota
	ownedHere
	ownedByOtherInstallation
)

// EnsureRepositoryOwner lets this vault installation capture into repo. It
// binds an empty unmarked repository and refuses history it has not claimed.
// Captures repeat the check under Kit's repository lock; this call is for
// callers that need the answer before they start.
func EnsureRepositoryOwner(ctx context.Context, repo *backup.Repo, metadata *store.Store, forceUnlock bool) error {
	self, err := installationOwner(ctx, metadata)
	if err != nil {
		return err
	}
	if owned, err := checkOwner(repo, self); owned || err != nil {
		return err
	}
	return withOwnerLock(ctx, repo, forceUnlock, func() error { return bindOwnerLocked(repo, self) })
}

// RequireRepositoryOwner refuses repo unless this installation owns it. It
// never binds, so automatic cleanup can call it before deleting history.
func RequireRepositoryOwner(ctx context.Context, repo *backup.Repo, metadata *store.Store) error {
	self, err := installationOwner(ctx, metadata)
	if err != nil {
		return err
	}
	owned, err := checkOwner(repo, self)
	if err != nil {
		return err
	}
	if !owned {
		return ErrRepositoryClaimRequired
	}
	return nil
}

// checkOwner reports whether self owns repo. Unmarked repositories report
// false; history owned by another installation needs a claim.
func checkOwner(repo *backup.Repo, self repositoryOwner) (bool, error) {
	state, _, err := readOwnership(repo, self)
	switch {
	case err != nil:
		return false, err
	case state == ownedByOtherInstallation:
		return false, ErrRepositoryClaimRequired
	}
	return state == ownedHere, nil
}

// bindOwnerLocked confirms or binds ownership. The caller holds Kit's
// exclusive repository lock, so no claim can change the marker meanwhile.
func bindOwnerLocked(repo *backup.Repo, self repositoryOwner) error {
	if owned, err := checkOwner(repo, self); owned || err != nil {
		return err
	}
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return fmt.Errorf("reading backup repository history: %w", err)
	}
	if len(manifests) > 0 {
		return ErrRepositoryClaimRequired
	}
	return writeOwner(repo, self, false)
}

// ownerFence checks ownership when Kit starts its metadata freeze. Kit holds
// the exclusive repository lock by then, so a claim cannot move ownership
// between this check and publication of the snapshot.
type ownerFence struct {
	backup.FreezeCoordinator

	repo *backup.Repo
	self repositoryOwner
}

func (f ownerFence) Begin(ctx context.Context) error {
	if err := bindOwnerLocked(f.repo, f.self); err != nil {
		return err
	}
	if err := f.FreezeCoordinator.Begin(ctx); err != nil {
		return fmt.Errorf("beginning capture freeze: %w", err)
	}
	return nil
}

// PreviewRepositoryClaim reports the history a claim would assign to this
// vault installation. Another vault's repository is refused.
func PreviewRepositoryClaim(ctx context.Context, repo *backup.Repo, metadata *store.Store) (RepositoryClaim, error) {
	self, err := installationOwner(ctx, metadata)
	if err != nil {
		return RepositoryClaim{}, err
	}
	claim, _, err := inspectClaim(repo, self)
	return claim, err
}

// ClaimRepository assigns repo to this vault installation if it still matches
// the preview that produced fingerprint. It cannot take another vault's
// repository; claiming one owned by another installation of this vault moves
// capture access here.
func ClaimRepository(ctx context.Context, repo *backup.Repo, metadata *store.Store, fingerprint string) (RepositoryClaim, error) {
	self, err := installationOwner(ctx, metadata)
	if err != nil {
		return RepositoryClaim{}, err
	}
	var claim RepositoryClaim
	err = withOwnerLock(ctx, repo, false, func() error {
		current, state, err := inspectClaim(repo, self)
		if err != nil {
			return err
		}
		if current.Fingerprint != fingerprint {
			return ErrRepositoryClaimChanged
		}
		if state != ownedHere {
			if err := writeOwner(repo, self, state == ownedByOtherInstallation); err != nil {
				return err
			}
		}
		claim, _, err = inspectClaim(repo, self)
		return err
	})
	return claim, err
}

func installationOwner(ctx context.Context, metadata *store.Store) (repositoryOwner, error) {
	incarnation, err := metadata.CurrentProcessingIncarnation(ctx)
	if err != nil {
		return repositoryOwner{}, err
	}
	return repositoryOwner{VaultID: metadata.VaultID(), IncarnationID: incarnation.ID}, nil
}

func readOwnership(repo *backup.Repo, self repositoryOwner) (ownership, []byte, error) {
	raw, err := os.ReadFile(repo.Path(repositoryOwnerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return unmarked, nil, nil
	}
	if err != nil {
		return unmarked, nil, fmt.Errorf("reading backup repository owner: %w", err)
	}
	var owner repositoryOwner
	if err := json.Unmarshal(raw, &owner); err != nil {
		return unmarked, nil, fmt.Errorf("reading backup repository owner: %w", err)
	}
	switch {
	case owner.VaultID != self.VaultID:
		return unmarked, nil, ErrRepositoryOwnerConflict
	case owner.IncarnationID != self.IncarnationID:
		return ownedByOtherInstallation, raw, nil
	}
	return ownedHere, raw, nil
}

func inspectClaim(repo *backup.Repo, self repositoryOwner) (RepositoryClaim, ownership, error) {
	state, owner, err := readOwnership(repo, self)
	if err != nil {
		return RepositoryClaim{}, state, err
	}
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return RepositoryClaim{}, state, fmt.Errorf("reading backup repository history: %w", err)
	}
	claim := RepositoryClaim{ID: repo.Config().RepoID, Path: repo.Root(), SnapshotCount: len(manifests),
		Owned: state == ownedHere, Takeover: state == ownedByOtherInstallation}
	if len(manifests) > 0 {
		claim.NewestAt = manifests[len(manifests)-1].CreatedAt
	}
	snapshots := make([]string, len(manifests))
	for i, manifest := range manifests {
		snapshots[i] = manifest.SnapshotID
	}
	encoded, err := json.Marshal(struct {
		Repository string   `json:"repository"`
		Owner      string   `json:"owner"`
		Snapshots  []string `json:"snapshots"`
	}{claim.ID, string(owner), snapshots}, json.Deterministic(true))
	if err != nil {
		return RepositoryClaim{}, state, fmt.Errorf("fingerprinting backup repository claim: %w", err)
	}
	digest := sha256.Sum256(encoded)
	claim.Fingerprint = hex.EncodeToString(digest[:])
	return claim, state, nil
}

func withOwnerLock(ctx context.Context, repo *backup.Repo, forceUnlock bool, fn func() error) (err error) {
	lock, err := repo.AcquireExclusiveLockContext(ctx, "docbank-owner", forceUnlock)
	if err != nil {
		return fmt.Errorf("locking backup repository ownership: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	return fn()
}

// writeOwner publishes the marker; replace is only for a confirmed takeover
// from another installation of this vault.
func writeOwner(repo *backup.Repo, owner repositoryOwner, replace bool) error {
	encoded, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("encoding backup repository owner: %w", err)
	}
	encoded = append(encoded, '\n')
	path := repo.Path(repositoryOwnerFile)
	if replace {
		err = atomicfile.WriteFile(path, encoded)
	} else {
		err = atomicfile.WriteNew(path, encoded)
	}
	if err != nil {
		return fmt.Errorf("writing backup repository owner: %w", err)
	}
	return nil
}
