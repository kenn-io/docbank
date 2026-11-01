//go:build !windows

package backupapp_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
)

type ownershipLockContext struct {
	context.Context

	lockPlanted func()
}

func (ctx ownershipLockContext) Err() error {
	ctx.lockPlanted()
	return ctx.Context.Err()
}

// Windows directory permissions do not prevent lock renames through Chmod.
func TestClaimRepositoryReportsLockReleaseFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	fixture := newArchiveFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	locks := repo.Path("locks")
	t.Cleanup(func() { require.NoError(t, os.Chmod(locks, 0o700)) })
	// A cancellable context lets database/sql call Err from its asynchronous
	// rows cleanup, which can restore read-only permissions after our cleanup.
	// This hook exercises lock release, not cancellation.
	ctx := ownershipLockContext{Context: context.Background(), lockPlanted: func() {
		// Acquisition checks cancellation after planting the exclusive lock.
		// Keep the directory readable so acquisition can finish, but prevent
		// Release from renaming the lock to claim it for removal.
		if _, err := os.Stat(repo.Path("locks", "exclusive.json")); err == nil {
			require.NoError(t, os.Chmod(locks, 0o500))
		} else {
			require.ErrorIs(t, err, os.ErrNotExist)
		}
	}}

	preview, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, fixture.metadata)
	require.NoError(t, err)
	_, err = backupapp.ClaimRepository(ctx, repo, fixture.metadata, preview.Fingerprint)
	require.ErrorIs(t, err, os.ErrPermission)
	claim, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, fixture.metadata)
	require.NoError(t, err)
	require.True(t, claim.Owned, "the claim published its marker before the release failed")
	_, err = os.Stat(repo.Path("locks", "exclusive.json"))
	require.NoError(t, err)
}
