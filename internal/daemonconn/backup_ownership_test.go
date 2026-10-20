package daemonconn_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/kit/backup"
)

func TestBackupOwnershipProblemsRemainTyped(t *testing.T) {
	first, _ := newClient(t, serverKey)
	second, _ := newClient(t, serverKey)
	path := filepath.Join(t.TempDir(), "repo")
	_, err := first.API().InitBackupRepository(t.Context(), &apiclient.InitBackupRepositoryRequestOptions{
		Body: &apiclient.InitBackupRepositoryBody{Repo: new(path)},
	})
	require.NoError(t, err)
	create := &apiclient.CreateBackupSnapshotRequestOptions{Body: &apiclient.CreateBackupSnapshotBody{Repo: new(path)}}
	_, err = second.API().CreateBackupSnapshot(t.Context(), create)
	require.ErrorIs(t, err, backupapp.ErrRepositoryOwnerConflict)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "backup_repository_owner_conflict", code)
	_, err = first.API().CreateBackupSnapshot(t.Context(), create)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(path, "docbank-owner")))
	_, err = first.API().CreateBackupSnapshot(t.Context(), create)
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	code, ok = daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "backup_repository_claim_required", code)
	_, err = first.BackupCreateStream(t.Context(), daemonconn.BackupCreateOptions{Repo: path}, nil)
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
	code, ok = daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "backup_repository_claim_required", code)
	preview, err := first.API().ClaimBackupRepository(t.Context(), &apiclient.ClaimBackupRepositoryRequestOptions{
		Body: &apiclient.ClaimBackupRepositoryBody{Repo: new(path)},
	})
	require.NoError(t, err)
	repo, err := backup.Open(path)
	require.NoError(t, err)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	points[0].Options.Tag = "changed-history"
	_, err = repo.WriteManifest(points[0])
	require.NoError(t, err)
	_, err = first.API().ClaimBackupRepository(t.Context(), &apiclient.ClaimBackupRepositoryRequestOptions{
		Body: &apiclient.ClaimBackupRepositoryBody{Repo: new(path), Confirm: new(true), Preview: new(preview.Preview)},
	})
	require.ErrorIs(t, err, backupapp.ErrRepositoryClaimChanged)
	code, ok = daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "backup_repository_claim_changed", code)
}
