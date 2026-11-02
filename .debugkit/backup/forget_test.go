package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForgetPreservesRetainedIncrementalRestore(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	r := initTestRepo(t)
	dbPath, contentDir, dataDir, writer := seedBackupFixture(t)
	opts := createOpts(dbPath, contentDir, dataDir, t.TempDir())
	first, err := Create(ctx, r, newTestApp(), opts)
	require.NoError(err)
	_, err = writer.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-02-01T00:00:00Z')`)
	require.NoError(err)
	second, err := Create(ctx, r, newTestApp(), opts)
	require.NoError(err)
	require.Positive(second.DB.MapChainDepth)
	_, err = writer.ExecContext(t.Context(), `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	third, err := Create(ctx, r, newTestApp(), opts)
	require.NoError(err)

	_, err = Forget(ctx, r, ForgetOptions{SnapshotIDs: []string{first.SnapshotID}})
	require.ErrorIs(err, ErrSnapshotRequired)
	snapshots, err := r.ListSnapshots()
	require.NoError(err)
	assert.Len(snapshots, 3)

	preview, err := Forget(ctx, r, ForgetOptions{SnapshotIDs: []string{third.SnapshotID}, DryRun: true})
	require.NoError(err)
	assert.Equal([]string{third.SnapshotID}, preview.Selected)
	assert.Empty(preview.Forgotten)
	snapshots, err = r.ListSnapshots()
	require.NoError(err)
	assert.Len(snapshots, 3)

	result, err := Forget(ctx, r, ForgetOptions{SnapshotIDs: []string{third.SnapshotID, third.SnapshotID}})
	require.NoError(err)
	assert.Equal([]string{third.SnapshotID}, result.Forgotten)
	verified, err := Verify(ctx, r, newTestApp(), VerifyOptions{All: true})
	require.NoError(err)
	assert.Empty(verified.Problems)
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		SnapshotID: second.SnapshotID, TargetDir: filepath.Join(t.TempDir(), "restored"),
	})
	require.NoError(err)

	// Removal of the last recovery point requires a separate, explicit opt-in.
	selection := []string{first.SnapshotID, second.SnapshotID}
	_, err = Forget(ctx, r, ForgetOptions{SnapshotIDs: selection})
	require.ErrorIs(err, ErrLastSnapshot)
	result, err = Forget(ctx, r, ForgetOptions{SnapshotIDs: selection, AllowEmpty: true})
	require.NoError(err)
	assert.Equal([]string{second.SnapshotID, first.SnapshotID}, result.Forgotten)
	snapshots, err = r.ListSnapshots()
	require.NoError(err)
	assert.Empty(snapshots)
}

func TestForgetRejectsInvalidSelectionBeforeDeletion(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	id, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
	require.NoError(err)
	for _, selection := range [][]string{nil, {id, "unknown"}} {
		_, err = Forget(t.Context(), r, ForgetOptions{SnapshotIDs: selection, AllowEmpty: true})
		require.Error(err)
		_, err = r.LoadManifest(id)
		require.NoError(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Forget(canceled, r, ForgetOptions{SnapshotIDs: []string{id}, AllowEmpty: true})
	require.ErrorIs(err, context.Canceled)
	_, err = r.LoadManifest(id)
	require.NoError(err)
}

func TestForgetOrdersDependenciesNotTimestamps(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	parent, err := r.WriteManifest(testManifest("2026-07-03T00:00:00Z", "", 0))
	require.NoError(err)
	child, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", parent, 1))
	require.NoError(err)
	sibling, err := r.WriteManifest(testManifest("2026-07-02T00:00:00Z", parent, 1))
	require.NoError(err)
	result, err := Forget(t.Context(), r, ForgetOptions{
		SnapshotIDs: []string{parent, child, sibling}, AllowEmpty: true, DryRun: true,
	})
	require.NoError(err)
	require.Len(result.Selected, 3)
	require.Equal(parent, result.Selected[2], "both children must be removed durably before their parent")
}

func TestForgetUsesRepositoryLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	id, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
	require.NoError(err)
	lock, err := r.AcquireExclusiveLock("create", false)
	require.NoError(err)
	defer func() { require.NoError(lock.Release()) }()
	_, err = Forget(t.Context(), r, ForgetOptions{SnapshotIDs: []string{id}, AllowEmpty: true})
	require.ErrorIs(err, ErrRepoLocked)
	_, err = r.LoadManifest(id)
	require.NoError(err)
}

func TestForgetPreconditionRunsUnderLockBeforeDeletion(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	id, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
	require.NoError(err)
	refused := errors.New("repository changed hands")
	calls := 0
	_, err = Forget(t.Context(), r, ForgetOptions{SnapshotIDs: []string{id}, AllowEmpty: true, Precondition: func() error {
		calls++
		// Another writer cannot take the repository while the check runs.
		_, lockErr := r.AcquireExclusiveLock("claim", false)
		require.ErrorIs(lockErr, ErrRepoLocked)
		return refused
	}})
	require.ErrorIs(err, refused)
	require.Equal(1, calls)
	_, err = r.LoadManifest(id)
	require.NoError(err)

	result, err := Forget(t.Context(), r, ForgetOptions{SnapshotIDs: []string{id}, AllowEmpty: true, Precondition: func() error { return nil }})
	require.NoError(err)
	require.Equal([]string{id}, result.Forgotten)
}

func TestForgetRejectsSymlinkedSnapshots(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		t.Run(map[bool]string{true: "preview", false: "delete"}[dryRun], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			r := initTestRepo(t)
			other := initTestRepo(t)
			id, err := other.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
			require.NoError(err)
			require.NoError(os.Remove(r.Path(snapshotsDirName)))
			err = os.Symlink(other.Path(snapshotsDirName), r.Path(snapshotsDirName))
			if err != nil && runtime.GOOS == "windows" {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			require.NoError(err)
			_, err = Forget(t.Context(), r, ForgetOptions{
				SnapshotIDs: []string{id}, AllowEmpty: true, DryRun: dryRun,
			})
			require.Error(err)
			_, err = other.LoadManifest(id)
			assert.NoError(err, "another repository's manifest must remain untouched")
		})
	}
}

func TestForgetCancelsWhileWaitingForSharedLock(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	id, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
	require.NoError(err)
	shared, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := Forget(ctx, r, ForgetOptions{SnapshotIDs: []string{id}, AllowEmpty: true})
		done <- err
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(shared.Release())
		<-done
	})
	require.Eventually(func() bool {
		_, err := os.Stat(r.Path(locksDirName, exclusiveLockName))
		return err == nil
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(2 * time.Second):
		require.FailNow("forget did not cancel while the shared lock remained held")
	}
	_, err = r.LoadManifest(id)
	require.NoError(err)
	// The canceled waiter must release its claim, even while the reader lives.
	reader, err := r.AcquireSharedLock("verify", false)
	require.NoError(err)
	require.NoError(reader.Release())
}
