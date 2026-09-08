package qmdexport

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanupCompleteOrphanAndIncompleteOrphan(t *testing.T) {
	// Catches refusing complete crash residue or deleting unproved partial work.
	target := publicationTarget(t)
	old := publishOne(t, target)
	orphan := filepath.Join(target, ".staging", "11111111111111111111111111111111")
	require.NoError(t, os.Rename(filepath.Dir(old.CollectionPath), orphan))
	incomplete := filepath.Join(target, ".staging", "22222222222222222222222222222222")
	require.NoError(t, os.Mkdir(incomplete, 0o700))
	sentinel := filepath.Join(incomplete, "synthetic-sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("preserve me"), 0o600))
	selected, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
	var post *PostPublicationError
	require.ErrorAs(t, err, &post)
	require.NotEmpty(t, selected.GenerationID)
	require.Equal(t, 1, post.Cleanup.RemovedGenerations)
	require.Equal(t, 1, post.Cleanup.PreservedCandidates)
	require.NoDirExists(t, orphan)
	value, readErr := os.ReadFile(sentinel)
	require.NoError(t, readErr)
	require.Equal(t, "preserve me", string(value))
}

func TestCleanupRechecksBodyIdentityBeforeRemoval(t *testing.T) {
	// Catches deleting a replacement at a previously verified body path.
	target := publicationTarget(t)
	old := publishOne(t, target)
	bodyPath := filepath.Join(old.CollectionPath, old.Manifest.Entries[0].RelativePath)
	receipt, err := publishWithHooks(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{
		beforeCleanupRemove: func() {
			require.NoError(t, os.Rename(bodyPath, bodyPath+".preserved"))
			require.NoError(t, os.WriteFile(bodyPath, []byte("synthetic substitution"), 0o600))
		},
	})
	var post *PostPublicationError
	require.ErrorAs(t, err, &post)
	require.ErrorIs(t, err, errIdentity)
	require.NotEmpty(t, receipt.GenerationID)
	value, readErr := os.ReadFile(bodyPath)
	require.NoError(t, readErr)
	require.Equal(t, "synthetic substitution", string(value))
}

func TestCleanupCandidateScanBoundsPreserveUnseenEntries(t *testing.T) {
	// Catches unbounded candidate scans and invented successful removal counts.
	target := publicationTarget(t)
	selected, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
	for _, name := range []string{"synthetic-a", "synthetic-b"} {
		require.NoError(t, os.Mkdir(filepath.Join(target, ".staging", name), 0o700))
	}
	root, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	bounds, err := normalizeOptions(Options{})
	require.NoError(t, err)
	status, err := cleanupWithBudget(t.Context(), root, selected.GenerationID, bounds, newCleanupBudget(), 1, nil)
	require.Error(t, err)
	require.True(t, status.ScanLimitReached)
	require.Zero(t, status.RemovedGenerations)
	for _, name := range []string{"synthetic-a", "synthetic-b"} {
		require.DirExists(t, filepath.Join(target, ".staging", name))
	}
	for _, limit := range []int{8, 9} {
		budget := &cleanupBudget{entries: limit, bytes: 1 << 20}
		status, err := cleanupWithBudget(t.Context(), root, selected.GenerationID, bounds, budget, 2, nil)
		require.Error(t, err) // Both incomplete stages remain unproved.
		if limit == 8 {
			require.True(t, status.ScanLimitReached)
			require.Zero(t, status.PreservedCandidates) // No exact unseen count.
			require.Zero(t, budget.entries)
		} else {
			require.False(t, status.ScanLimitReached)
			require.Equal(t, 2, status.PreservedCandidates)
			require.Equal(t, 1, budget.entries) // Eight actual entries, probe refunded.
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = cleanupWithBudget(ctx, root, selected.GenerationID, bounds, newCleanupBudget(), 2, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCleanupPreservesUnverifiedCandidates(t *testing.T) {
	// Catches stamp-only deletion and silent success when stale text remains.
	for _, change := range []string{"wrong-body", "extra-file", "extra-directory", "missing-body", "manifest", "root-stamp", "link"} {
		t.Run(change, func(t *testing.T) {
			target := publicationTarget(t)
			old := publishOne(t, target)
			generationPath := filepath.Dir(old.CollectionPath)
			bodyPath := filepath.Join(old.CollectionPath, old.Manifest.Entries[0].RelativePath)
			sentinel := filepath.Join(target, "unknown-synthetic")
			require.NoError(t, os.WriteFile(sentinel, []byte("preserve me"), 0o600))
			switch change {
			case "wrong-body":
				require.NoError(t, os.WriteFile(bodyPath, []byte("Synthetic retained body\n"), 0o600))
			case "extra-file":
				require.NoError(t, os.WriteFile(filepath.Join(generationPath, "unknown"), []byte("preserve me"), 0o600))
			case "extra-directory":
				require.NoError(t, os.Mkdir(filepath.Join(old.CollectionPath, "unknown"), 0o700))
			case "missing-body":
				require.NoError(t, os.Remove(bodyPath))
			case "manifest":
				require.NoError(t, os.WriteFile(filepath.Join(generationPath, "manifest.json"), []byte("{}\n"), 0o600))
			case "root-stamp":
				otherTarget := publicationTarget(t)
				other := publishOne(t, otherTarget)
				stamp, err := os.ReadFile(filepath.Join(filepath.Dir(other.CollectionPath), ".docbank-qmd-generation.json"))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(generationPath, ".docbank-qmd-generation.json"), stamp, 0o600))
			case "link":
				require.NoError(t, os.Remove(bodyPath))
				if err := os.Symlink(sentinel, bodyPath); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			selected, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
			var post *PostPublicationError
			require.ErrorAs(t, err, &post)
			require.Equal(t, "cleanup_incomplete", post.Code)
			require.Equal(t, 1, post.Cleanup.PreservedCandidates)
			require.Equal(t, 1, post.Cleanup.UnknownEntries)
			require.NotEmpty(t, selected.GenerationID)
			require.DirExists(t, generationPath)
			value, readErr := os.ReadFile(sentinel)
			require.NoError(t, readErr)
			require.Equal(t, "preserve me", string(value))
			require.NotContains(t, err.Error(), "unknown-synthetic")
		})
	}
}
