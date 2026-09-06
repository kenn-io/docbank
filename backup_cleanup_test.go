package docbank_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	docbank "go.kenn.io/docbank"
)

func TestRepositoryCleanupWithoutSourcePreservesRetainedRestore(t *testing.T) {
	r := require.New(t)
	source := filepath.Join(t.TempDir(), "source")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: source})
	r.NoError(err)
	t.Cleanup(func() { r.NoError(vault.Close()) })
	body := []byte("retained document\n")
	createRangeFixture(t, vault, "/document.txt", body)
	repositoryPath := filepath.Join(t.TempDir(), "repository")
	repository, err := docbank.InitBackupRepository(repositoryPath)
	r.NoError(err)
	retained, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	r.NoError(err)
	createRangeFixture(t, vault, "/later.txt", []byte("only in the removed recovery point\n"))
	removed, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{})
	r.NoError(err)
	r.NoError(vault.Close())
	r.NoError(os.RemoveAll(source))
	repository, err = docbank.OpenBackupRepository(repositoryPath)
	r.NoError(err)

	selection := docbank.BackupForgetOptions{SnapshotIDs: []string{removed.ID}, DryRun: true}
	preview, err := repository.Forget(t.Context(), selection)
	r.NoError(err)
	r.Equal([]string{removed.ID}, preview.Selected)
	r.Empty(preview.Forgotten)
	snapshots, err := repository.Snapshots()
	r.NoError(err)
	r.Len(snapshots, 2)
	selection.DryRun = false
	forgotten, err := repository.Forget(t.Context(), selection)
	r.NoError(err)
	r.Equal([]string{removed.ID}, forgotten.Forgotten)

	planned, err := repository.Prune(t.Context(), docbank.BackupPruneOptions{DryRun: true})
	r.NoError(err)
	r.NotEmpty(planned.PacksToRemove)
	r.Positive(planned.BytesToRemove)
	r.Empty(planned.RemovedPacks)
	r.Zero(planned.BytesRemoved)

	// Reports must survive an error after publication so the host can explain
	// the partial operation and retry it without the original vault.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	partial, err := repository.Prune(ctx, docbank.BackupPruneOptions{
		Progress: func(event docbank.BackupProgress) {
			if event.Stage == "seal" && event.Final {
				cancel()
			}
		},
	})
	r.ErrorIs(err, context.Canceled)
	r.ElementsMatch(planned.PacksToRemove, partial.PacksToRemove)
	r.Empty(partial.RemovedPacks)
	pruned, err := repository.Prune(t.Context(), docbank.BackupPruneOptions{})
	r.NoError(err)
	r.ElementsMatch(planned.PacksToRemove, pruned.RemovedPacks)
	r.Equal(planned.BytesToRemove, pruned.BytesRemoved)
	verified, err := repository.Verify(t.Context(), docbank.BackupVerifyOptions{All: true})
	r.NoError(err)
	r.Empty(verified.Problems)
	r.Equal([]string{retained.ID}, verified.Snapshots)
	restored, err := repository.Restore(t.Context(), docbank.BackupRestoreOptions{
		SnapshotID: retained.ID, Target: filepath.Join(t.TempDir(), "restored"),
	})
	r.NoError(err)
	recovered, err := docbank.New(t.Context(), docbank.Config{Root: restored.Target})
	r.NoError(err)
	t.Cleanup(func() { r.NoError(recovered.Close()) })
	reader, err := recovered.OpenContent(t.Context(), "/document.txt")
	r.NoError(err)
	got, err := io.ReadAll(reader.Reader)
	r.NoError(err)
	r.NoError(reader.Reader.Verify())
	r.NoError(reader.Reader.Close())
	r.Equal(body, got)

	_, err = repository.Forget(t.Context(), docbank.BackupForgetOptions{SnapshotIDs: []string{retained.ID}})
	r.ErrorIs(err, docbank.ErrBackupLastSnapshot)
	last, err := repository.Forget(t.Context(), docbank.BackupForgetOptions{
		SnapshotIDs: []string{retained.ID}, AllowEmpty: true,
	})
	r.NoError(err)
	r.Equal([]string{retained.ID}, last.Forgotten)
}

func TestRepositoryCleanupRequiresInitializedRepository(t *testing.T) {
	for _, repository := range []*docbank.BackupRepository{nil, {}} {
		_, err := repository.Forget(t.Context(), docbank.BackupForgetOptions{})
		require.ErrorContains(t, err, "repository is required")
		_, err = repository.Prune(t.Context(), docbank.BackupPruneOptions{})
		require.ErrorContains(t, err, "repository is required")
	}
}
