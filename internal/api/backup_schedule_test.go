package api

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func scheduledDeps(t *testing.T, interval time.Duration) Deps {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	dir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Backup = config.BackupConfig{Repo: filepath.Join(t.TempDir(), "repo"), Interval: config.Duration(interval), KeepLast: 1}
	return Deps{Store: metadata, Blobs: blobs, VaultRoot: root, Cfg: cfg, Gate: NewOperationGate(), Logger: slog.New(slog.DiscardHandler)}
}

func TestBackupScheduleDeadlineAfterResume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		suspend  time.Duration
		wait     time.Duration
	}{
		{name: "overdue capture", deadline: 24 * time.Hour, suspend: 25 * time.Hour, wait: time.Minute},
		{name: "future retry", deadline: 5 * time.Minute, suspend: 2 * time.Minute, wait: 3 * time.Minute},
		{name: "backward clock", deadline: 5 * time.Minute, suspend: -time.Hour, wait: 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				deadline := time.Now().Add(tc.deadline)
				var suspended atomic.Int64
				finished := make(chan error, 1)
				go func() {
					finished <- waitBackupDeadline(t.Context(), deadline, func() time.Time {
						// Adjust wall time independently of timer time for sleep or clock correction.
						return time.Now().Add(time.Duration(suspended.Load()))
					})
				}()
				synctest.Wait()
				suspended.Store(int64(tc.suspend))
				time.Sleep(tc.wait - time.Second)
				synctest.Wait()
				select {
				case err := <-finished:
					t.Fatalf("deadline returned early: %v", err)
				default:
				}
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case err := <-finished:
					require.NoError(t, err)
				default:
					t.Fatal("deadline was not rechecked after resume")
				}
			})
		})
	}
}

func TestBackupScheduleCaptureRetentionAndRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		_, err = createBackupSnapshot(t.Context(), repo, d, d.Gate, backupCreateRequest{Tag: "manual"}, nil)
		require.NoError(t, err)
		time.Sleep(time.Second)
		_, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		time.Sleep(time.Hour)
		_, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
	})
}

func TestBackupScheduleRetryAndCancellation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		interval     time.Duration
		healthyPoint bool
	}{
		{name: "minute retry", interval: time.Minute},
		{name: "hour retry", interval: time.Hour},
		{name: "healthy point is not due", interval: time.Hour, healthyPoint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := scheduledDeps(t, tc.interval)
				supervisor := jobs.New(t.Context(), d.Logger)
				require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
				synctest.Wait()
				require.Contains(t, supervisor.Snapshot()[0].Error, "backup")
				// Initialization after the first failed attempt must be picked up at the retry deadline.
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				var existing BackupSnapshot
				if tc.healthyPoint {
					existing, err = createScheduledBackupSnapshot(t.Context(), repo, d)
					require.NoError(t, err)
				}
				retry := min(tc.interval, 5*time.Minute)
				time.Sleep(retry - time.Second)
				synctest.Wait()
				points, err := repo.ListSnapshots()
				require.NoError(t, err)
				if tc.healthyPoint {
					require.Len(t, points, 1)
				} else {
					require.Empty(t, points)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				points, err = repo.ListSnapshots()
				require.NoError(t, err)
				require.Len(t, points, 1)
				require.Equal(t, "docbank:scheduled:"+d.Store.VaultID(), points[0].Options.Tag)
				if tc.healthyPoint {
					require.Equal(t, existing.ID, points[0].SnapshotID)
				}
				require.Empty(t, supervisor.Snapshot()[0].Error)
				require.NoError(t, supervisor.Shutdown(t.Context()))
				require.Equal(t, jobs.StatusCancelled, supervisor.Snapshot()[0].Status)
			})
		})
	}
}

func TestBackupScheduleStaleWarningPersistsUntilHealthyCheck(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		old, err := createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		time.Sleep(49 * time.Hour)
		checks := make(chan struct{})
		checked := make(chan error)
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-checks:
					_, err := scheduledBackup(ctx, d, d.Logger, false)
					checked <- err
				}
			}
		}))
		checks <- struct{}{}
		require.NoError(t, <-checked)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, old.ID, points[0].SnapshotID, "stale capture and cleanup completed")
		assert.Contains(t, supervisor.Snapshot()[0].Error, "newest scheduled backup is stale")
		// A later healthy check clears the warning without another capture.
		checks <- struct{}{}
		require.NoError(t, <-checked)
		assert.Empty(t, supervisor.Snapshot()[0].Error)
		after, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, after, 1)
		require.Equal(t, points[0].SnapshotID, after[0].SnapshotID)
		require.NoError(t, supervisor.Shutdown(t.Context()))
	})
}

func TestBackupScheduleFutureTimestampCapsWaitAndKeepsCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		points[0].CreatedAt = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
		future, err := repo.WriteManifest(points[0])
		require.NoError(t, err)
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		lock, err := repo.AcquireExclusiveLock("test-recovery-conflict", false)
		require.NoError(t, err)
		time.Sleep(time.Hour)
		synctest.Wait()
		require.Contains(t, supervisor.Snapshot()[0].Error, "locked")
		require.NoError(t, lock.Release())
		// The failed due capture must retry even though the future timestamp is not due.
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, future, points[0].SnapshotID)
		// Kit keeps advancing the future timestamp, but captures stay one interval apart.
		firstRecovery := points[0].SnapshotID
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Equal(t, firstRecovery, points[0].SnapshotID)
		time.Sleep(time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, firstRecovery, points[0].SnapshotID)
		require.NoError(t, supervisor.Shutdown(t.Context()))
	})
}

func TestBackupScheduleCleanupFailurePreservesCapture(t *testing.T) {
	t.Parallel()
	d := scheduledDeps(t, time.Hour)
	repo, err := backup.Init(d.Cfg.Backup.Repo)
	require.NoError(t, err)
	_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
	require.NoError(t, err)
	created, err := createScheduledBackupSnapshot(t.Context(), repo, d)
	require.NoError(t, err)
	lock, err := repo.AcquireExclusiveLock("test-cleanup-conflict", false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	err = retainScheduledBackups(t.Context(), repo, backupapp.ScheduledTag(d.Store.VaultID()), 1, created.ID)
	require.ErrorIs(t, err, backup.ErrRepoLocked)
	require.ErrorContains(t, err, "forgetting scheduled backups")
	_, err = repo.LoadManifest(created.ID)
	require.NoError(t, err)
}

func TestBackupScheduleCancellationWhileMaintenanceHoldsGate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		_, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		held, release := make(chan struct{}), make(chan struct{})
		maintenance := make(chan error, 1)
		go func() {
			maintenance <- d.Gate.MaintainContext(t.Context(), func() error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		require.NoError(t, supervisor.Shutdown(t.Context()))
		close(release)
		require.NoError(t, <-maintenance)
	})
}
