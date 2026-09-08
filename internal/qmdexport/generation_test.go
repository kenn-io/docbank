package qmdexport

import (
	"context"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGenerationSpecialFilePreservedWithoutBlocking(t *testing.T) {
	// Catches following/reading a FIFO instead of rejecting its opened type.
	if runtime.GOOS == "windows" {
		t.Skip("FIFO is a Unix filesystem control")
	}
	target := publicationTarget(t)
	old := publishOne(t, target)
	bodyPath := filepath.Join(old.CollectionPath, old.Manifest.Entries[0].RelativePath)
	require.NoError(t, os.Remove(bodyPath))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "mkfifo", "-m", "600", bodyPath)
	require.NoError(t, command.Run())
	type result struct {
		receipt Receipt
		err     error
	}
	done := make(chan result, 1)
	go func() {
		receipt, err := Publish(ctx, target, "synthetic", nil, syntheticReader{}, Options{})
		done <- result{receipt, err}
	}()
	select {
	case got := <-done:
		var post *PostPublicationError
		require.ErrorAs(t, got.err, &post)
		require.NotEmpty(t, got.receipt.GenerationID)
		require.Equal(t, 1, post.Cleanup.PreservedCandidates)
	case <-ctx.Done():
		t.Fatal("special-file verification blocked")
	}
	info, err := os.Lstat(bodyPath)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeNamedPipe)
}

func TestGenerationPublishedArtifactContainsOnlyExactManagedEntries(t *testing.T) {
	// Catches extra stage files, stamp leakage into collection, and mismatched
	// pointer/manifest/stamp identity in the actual written synthetic artifact.
	target := publicationTarget(t)
	receipt := publishOne(t, target)
	rootNames, err := os.ReadDir(target)
	require.NoError(t, err)
	names := make([]string, 0, len(rootNames))
	for _, entry := range rootNames {
		names = append(names, entry.Name())
	}
	require.ElementsMatch(t, []string{".docbank-qmd-export.json", ".publish.lock", ".staging", "CURRENT", "generations"}, names)
	gen := filepath.Dir(receipt.CollectionPath)
	entries, err := os.ReadDir(gen)
	require.NoError(t, err)
	names = names[:0]
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.ElementsMatch(t, []string{".docbank-qmd-generation.json", "manifest.json", "collection"}, names)
	stampBytes, err := os.ReadFile(filepath.Join(gen, generationStampName))
	require.NoError(t, err)
	require.LessOrEqual(t, len(stampBytes), 4096)
	var stamp generationStamp
	require.NoError(t, json.Unmarshal(stampBytes, &stamp, json.RejectUnknownMembers(true)))
	require.Equal(t, "docbank-qmd-generation", stamp.Format)
	require.Equal(t, 1, stamp.Version)
	require.Len(t, stamp.RootID, 32)
	require.Equal(t, receipt.GenerationID, stamp.GenerationID)
	require.Equal(t, receipt.GenerationID, stamp.ManifestChecksum)
	manifestBytes, err := os.ReadFile(filepath.Join(gen, "manifest.json"))
	require.NoError(t, err)
	require.Equal(t, byte('\n'), manifestBytes[len(manifestBytes)-1])
	var manifest Manifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	require.Equal(t, receipt.Manifest, manifest)
	pointer, err := os.ReadFile(filepath.Join(target, "CURRENT"))
	require.NoError(t, err)
	require.Equal(t, receipt.GenerationID+"\n", string(pointer))
	t.Logf("actual synthetic artifact: 5 root entries, 3 generation entries, 1 body, 65-byte pointer, canonical manifest %d bytes, root-bound stamp %d bytes", len(manifestBytes), len(stampBytes))
}

func TestGenerationReservesEveryGrowthProbeBeforeReading(t *testing.T) {
	// Catches charging advertised bytes while reading unreserved probe bytes.
	target := publicationTarget(t)
	receipt := publishOne(t, target)
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(receipt.CollectionPath), "manifest.json"))
	require.NoError(t, err)
	root, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	bounds, err := normalizeOptions(Options{})
	require.NoError(t, err)
	reserved := int64(len(manifest) + 1 + len("synthetic retained body\n") + 1)
	for _, allowance := range []int64{int64(len(manifest)), reserved - 1, reserved} {
		budget := &cleanupBudget{entries: 100, bytes: allowance}
		verified, err := verifyOwnedGeneration(t.Context(), root, root.generations, receipt.GenerationID, receipt.GenerationID, bounds, budget)
		if allowance < reserved {
			require.ErrorIs(t, err, errCleanupBound)
			require.Nil(t, verified)
		} else {
			require.NoError(t, err)
			require.Zero(t, budget.bytes)
			require.NoError(t, verified.ledger.close())
		}
		require.GreaterOrEqual(t, budget.bytes, int64(0))
	}
}

func TestGenerationEntryBudgetIncludesEmptyLayout(t *testing.T) {
	// Catches omitted collection/documents checks and entry-budget overruns.
	target := publicationTarget(t)
	receipt, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
	root, err := acquireOwnedRoot(t.Context(), target, ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	bounds, err := normalizeOptions(Options{})
	require.NoError(t, err)
	for _, limit := range []int{3, 4, 5} {
		budget := &cleanupBudget{entries: limit, bytes: 1 << 20}
		verified, err := verifyOwnedGeneration(t.Context(), root, root.generations, receipt.GenerationID, receipt.GenerationID, bounds, budget)
		if limit < 5 {
			require.ErrorIs(t, err, errCleanupBound)
			require.Nil(t, verified)
		} else {
			require.NoError(t, err)
			require.Equal(t, 1, budget.entries)
			require.NoError(t, verified.ledger.close())
		}
	}
}
