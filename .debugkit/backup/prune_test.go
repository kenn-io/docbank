package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pack"
)

// Put existing indexed blobs alongside unused bytes. Older packs remain on
// disk, exercising both sparse-pack rewriting and removal of duplicate packs.
func consolidatePruneFixture(t *testing.T, r *Repo, padding int) string {
	t.Helper()
	require := require.New(t)
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	a := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	t.Cleanup(a.Abort)
	for id := range known {
		raw, readErr := r.ReadBlob(known, id, nil, testPackExt)
		require.NoError(readErr)
		_, _, err = a.Add(raw)
		require.NoError(err)
	}
	unused := make([]byte, padding)
	_, err = rand.Read(unused)
	require.NoError(err)
	_, _, err = a.Add(unused)
	require.NoError(err)
	packs, entries, err := a.Finish()
	require.NoError(err)
	require.Len(packs, 1)
	_, err = r.WriteIndex(entries)
	require.NoError(err)
	return packs[0]
}

func assertPrunedRestore(t *testing.T, r *Repo, id string) {
	t.Helper()
	require := require.New(t)
	verified, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{All: true})
	require.NoError(err)
	require.Empty(verified.Problems)
	_, err = Restore(t.Context(), r, newTestApp(), RestoreOptions{
		SnapshotID: id, TargetDir: filepath.Join(t.TempDir(), "restored"),
	})
	require.NoError(err)
}

func TestPruneReclaimsSparseAndDeadPacksWithRetainedChain(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, contentDir, dataDir, writer := seedBackupFixture(t)
	opts := createOpts(dbPath, contentDir, dataDir, t.TempDir())
	_, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-02-01T00:00:00Z')`)
	require.NoError(err)
	retained, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(err)
	require.Positive(retained.DB.MapChainDepth)
	_, err = writer.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	forgotten, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(err)
	_, err = Forget(t.Context(), r, ForgetOptions{SnapshotIDs: []string{forgotten.SnapshotID}})
	require.NoError(err)
	sparse := consolidatePruneFixture(t, r, 256<<10)

	preview, err := Prune(t.Context(), r, newTestApp(), PruneOptions{DryRun: true})
	require.NoError(err)
	assert.Equal([]string{sparse}, preview.PacksToRepack)
	assert.NotEmpty(preview.PacksToRemove)
	assert.Positive(preview.BytesToRemove)
	assert.Positive(preview.LiveBytesToRewrite)
	assert.Empty(preview.RemovedPacks)
	assertPrunedRestore(t, r, retained.SnapshotID)

	result, err := Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(err)
	assert.ElementsMatch(preview.PacksToRemove, result.RemovedPacks)
	assert.Equal(preview.BytesToRemove, result.BytesRemoved)
	assert.Greater(result.BytesRemoved, result.BytesWritten)
	for _, id := range result.RemovedPacks {
		_, err = os.Stat(r.packPath(id, testPackExt))
		require.ErrorIs(err, os.ErrNotExist)
	}
	assertPrunedRestore(t, r, retained.SnapshotID)
	index, err := r.LoadBlobIndex()
	require.NoError(err)
	for _, entry := range index {
		_, err = os.Stat(r.packPath(entry.PackID, testPackExt))
		require.NoError(err, "no published index may reference a retired pack")
	}
	// Capture must deduplicate correctly against the compacted index too.
	_, err = Create(t.Context(), r, newTestApp(), opts)
	require.NoError(err)
	assertPrunedRestore(t, r, retained.SnapshotID)
}

func TestPruneKeepsMostlyLivePacks(t *testing.T) {
	require := require.New(t)
	r, manifest := buildVerifyFixture(t)
	dense := consolidatePruneFixture(t, r, 1)
	result, err := Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(err)
	require.Empty(result.PacksToRepack)
	require.NotContains(result.RemovedPacks, dense)
	assertPrunedRestore(t, r, manifest.SnapshotID)
	second, err := Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(err)
	require.Empty(second.RemovedPacks)
	require.Zero(second.BytesWritten)
}

func TestPruneCancellationLeavesRetainedSnapshotsRestorable(t *testing.T) {
	for _, stage := range []ProgressStage{ProgressStageSeal, ProgressStagePruneIndexes, ProgressStagePrunePacks} {
		t.Run(string(stage), func(t *testing.T) {
			require := require.New(t)
			r, manifest := buildVerifyFixture(t)
			consolidatePruneFixture(t, r, 256<<10)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, err := Prune(ctx, r, newTestApp(), PruneOptions{Progress: func(event ProgressEvent) {
				if event.Stage == stage && event.Done > 0 {
					cancel()
				}
			}})
			require.ErrorIs(err, context.Canceled)
			assertPrunedRestore(t, r, manifest.SnapshotID)
			_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
			require.NoError(err)
			assertPrunedRestore(t, r, manifest.SnapshotID)
		})
	}
}

func TestPruneRejectsBrokenRetainedReferencesBeforeRemoval(t *testing.T) {
	require := require.New(t)
	r, manifest := buildVerifyFixture(t)
	packID := consolidatePruneFixture(t, r, 256<<10)
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	id, err := pack.ParseBlobID(manifest.DB.PageMap)
	require.NoError(err)
	corruptStoredBlob(t, r, known, id)
	_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.Error(err)
	_, err = os.Stat(r.packPath(packID, testPackExt))
	require.NoError(err)
}

func TestPruneEmptyRepositoryReclaimsOrphans(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	id := consolidatePruneFixture(t, r, 1024)
	result, err := Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(err)
	require.Equal([]string{id}, result.RemovedPacks)
	known, err := r.LoadBlobIndex()
	require.NoError(err)
	require.Empty(known)
}

func TestPruneInterruptedProcessCanRetry(t *testing.T) {
	if root := os.Getenv("KIT_TEST_PRUNE_REPO"); root != "" {
		r, err := Open(root)
		require.NoError(t, err)
		_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{Progress: func(event ProgressEvent) {
			if string(event.Stage) == os.Getenv("KIT_TEST_PRUNE_STAGE") && event.Done > 0 {
				os.Exit(37) // simulate termination without deferred cleanup
			}
		}})
		require.NoError(t, err)
		return
	}
	for _, stage := range []ProgressStage{ProgressStageSeal, ProgressStagePruneIndexes, ProgressStagePrunePacks} {
		t.Run(string(stage), func(t *testing.T) {
			require := require.New(t)
			r, manifest := buildVerifyFixture(t)
			consolidatePruneFixture(t, r, 256<<10)
			// A future-dated old index sorts AFTER newly published indexes.
			// Crash recovery must not depend on a new index winning that union.
			indexes, err := os.ReadDir(r.Path(indexesDirName))
			require.NoError(err)
			require.NoError(os.Rename(r.Path(indexesDirName, indexes[len(indexes)-1].Name()), r.Path(indexesDirName, "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"+indexExt)))
			executable, err := os.Executable()
			require.NoError(err)
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestPruneInterruptedProcessCanRetry$")
			cmd.Env = append(os.Environ(), "KIT_TEST_PRUNE_REPO="+r.Root(), "KIT_TEST_PRUNE_STAGE="+string(stage))
			out, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			require.ErrorAs(err, &exited, string(out))
			require.Equal(37, exited.ExitCode(), string(out))
			verified, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{All: true, ForceUnlock: true})
			require.NoError(err)
			require.Empty(verified.Problems)
			assertPrunedRestore(t, r, manifest.SnapshotID)
			_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
			require.NoError(err)
			assertPrunedRestore(t, r, manifest.SnapshotID)
		})
	}
}

func TestPruneFailedIndexSyncDoesNotRetireOldPacks(t *testing.T) {
	require := require.New(t)
	r, manifest := buildVerifyFixture(t)
	consolidatePruneFixture(t, r, 256<<10)
	preview, err := Prune(t.Context(), r, newTestApp(), PruneOptions{DryRun: true})
	require.NoError(err)
	// The merged index reaches disk but its publication reports failure,
	// as when the directory sync after the rename fails.
	originalPublish := publishFile
	t.Cleanup(func() { publishFile = originalPublish })
	wantErr := errors.New("injected index directory sync failure")
	publishFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		if err := originalPublish(path, data, opts...); err != nil {
			return err
		}
		if filepath.Dir(path) == r.Path(indexesDirName) {
			return wantErr
		}
		return nil
	}
	_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.ErrorIs(err, wantErr)
	publishFile = originalPublish
	for _, id := range preview.PacksToRemove {
		_, err = os.Stat(r.packPath(id, testPackExt))
		require.NoError(err)
	}
	assertPrunedRestore(t, r, manifest.SnapshotID)
	_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(err)
	assertPrunedRestore(t, r, manifest.SnapshotID)
}

func TestPruneRefusesSymlinkedPackDirectory(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	other := initTestRepo(t)
	id := consolidatePruneFixture(t, other, 1024)
	require.NoError(os.Remove(r.Path(packsDirName)))
	err := os.Symlink(other.Path(packsDirName), r.Path(packsDirName))
	if err != nil && runtime.GOOS == "windows" {
		t.Skipf("symlink unavailable: %v", err)
	}
	require.NoError(err)
	_, err = Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.Error(err)
	_, err = os.Stat(other.packPath(id, testPackExt))
	require.NoError(err)
}

func TestPruneOnlyRewritesBelowHalfLivePayload(t *testing.T) {
	for _, liveSize := range []int{49, 50, 51} {
		t.Run(strconv.Itoa(liveSize)+"_percent_live", func(t *testing.T) {
			require := require.New(t)
			r := initTestRepo(t)
			known := map[pack.BlobID]IndexEntry{}
			a := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
			defer a.Abort()
			live := make([]byte, liveSize)
			dead := make([]byte, 100-liveSize)
			for i := range live {
				live[i] = byte(i)
			}
			for i := range dead {
				dead[i] = byte(i + 128)
			}
			id, _, err := a.Add(live)
			require.NoError(err)
			_, _, err = a.Add(dead)
			require.NoError(err)
			_, entries, err := a.Finish()
			require.NoError(err)
			_, err = r.WriteIndex(entries)
			require.NoError(err)
			dirs, err := openPruneDirs(r)
			require.NoError(err)
			defer func() { require.NoError(dirs.close()) }()
			plan, err := planPrune(t.Context(), dirs, testPackExt, map[pack.BlobID]IndexEntry{id: known[id]})
			require.NoError(err)
			if liveSize < 50 {
				require.NotEmpty(plan.result.PacksToRepack)
			} else {
				require.Empty(plan.result.PacksToRepack)
			}
		})
	}
}

func TestPruneCancelsWhileRepositoryReaderHoldsLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	lock, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	defer func() { require.NoError(lock.Release()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the held reader lock keeps Prune blocked until it fires
	defer cancel()
	_, err = Prune(ctx, r, newTestApp(), PruneOptions{})
	require.ErrorIs(err, context.DeadlineExceeded)
	// A canceled prune must release its exclusive claim.
	other, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	require.NoError(other.Release())
}
