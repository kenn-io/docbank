package api

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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
		_, err := d.Store.Mkdir(t.Context(), d.Store.RootID(), "retained")
		require.NoError(t, err)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		manual, err := createBackupSnapshot(t.Context(), repo, d, d.Gate, backupCreateRequest{Tag: "manual"}, nil)
		require.NoError(t, err)
		time.Sleep(time.Second)
		delay, err := scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		require.Equal(t, time.Hour, delay)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		time.Sleep(30 * time.Minute)
		delay, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		require.Equal(t, 30*time.Minute, delay)
		time.Sleep(49 * time.Hour)
		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))
		delay, err = scheduledBackup(t.Context(), d, logger, false)
		require.NoError(t, err)
		require.Equal(t, time.Hour, delay)
		require.Contains(t, logs.String(), "newest scheduled backup is stale")
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		_, err = repo.LoadManifest(manual.ID)
		require.NoError(t, err)
		// The new retained point remains independently readable after forgetting its ancestor.
		verified, err := backupapp.Verify(t.Context(), repo, "test", backup.VerifyOptions{All: true})
		require.NoError(t, err)
		require.Empty(t, verified.Problems)
		for _, point := range points {
			require.NotNil(t, point.Metadata)
			require.Equal(t, backupapp.MetadataFormat, point.Metadata.Format)
		}
		target := filepath.Join(t.TempDir(), "restored")
		_, err = backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target})
		require.NoError(t, err)
		restored, err := store.Open(filepath.Join(target, "docbank.db"))
		require.NoError(t, err)
		defer func() { require.NoError(t, restored.Close()) }()
		_, err = restored.NodeByPath(t.Context(), "/retained")
		require.NoError(t, err)
	})
}

func TestBackupScheduleRetryAndCancellation(t *testing.T) {
	t.Parallel()
	for _, interval := range []time.Duration{time.Minute, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := scheduledDeps(t, interval)
				supervisor := jobs.New(t.Context(), d.Logger)
				require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
				synctest.Wait()
				require.Contains(t, supervisor.Snapshot()[0].Error, "backup")
				// Initialization after the first failed attempt must be picked up at the retry deadline.
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				retry := min(interval, 5*time.Minute)
				time.Sleep(retry - time.Second)
				synctest.Wait()
				points, err := repo.ListSnapshots()
				require.NoError(t, err)
				require.Empty(t, points)
				time.Sleep(time.Second)
				synctest.Wait()
				points, err = repo.ListSnapshots()
				require.NoError(t, err)
				require.Len(t, points, 1)
				require.Equal(t, "docbank:scheduled:"+d.Store.VaultID(), points[0].Options.Tag)
				require.Empty(t, supervisor.Snapshot()[0].Error)
				require.NoError(t, supervisor.Shutdown(t.Context()))
				require.Equal(t, jobs.StatusCancelled, supervisor.Snapshot()[0].Status)
			})
		})
	}
}

func TestBackupScheduleClearsErrorWhenHealthyPointIsNotDue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		require.NotEmpty(t, supervisor.Snapshot()[0].Error)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		point, err := createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Equal(t, point.ID, points[0].SnapshotID)
		require.Empty(t, supervisor.Snapshot()[0].Error)
		require.NoError(t, supervisor.Shutdown(t.Context()))
	})
}

func TestBackupScheduleAfterForgettingFutureHistory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		manual, err := createBackupSnapshot(t.Context(), repo, d, d.Gate, backupCreateRequest{Tag: "manual"}, nil)
		require.NoError(t, err)
		future, err := repo.LoadManifest(manual.ID)
		require.NoError(t, err)
		future.CreatedAt = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
		_, err = repo.WriteManifest(future)
		require.NoError(t, err)
		// Initialize scheduling against future-dated history, then remove every point.
		_, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		history, err := repo.ListSnapshots()
		require.NoError(t, err)
		var ids []string
		for _, point := range history {
			ids = append(ids, point.SnapshotID)
		}
		_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: ids, AllowEmpty: true})
		require.NoError(t, err)
		_, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		first := points[0].SnapshotID
		// A restarted worker recognizes the new point and waits for its interval.
		_, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Equal(t, first, points[0].SnapshotID)
		time.Sleep(time.Hour)
		_, err = scheduledBackup(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, first, points[0].SnapshotID)
	})
}

func TestBackupSchedulePreservesLegacyTaggedManualHistory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Hour)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		manual, err := createBackupSnapshot(t.Context(), repo, d, d.Gate, backupCreateRequest{Tag: "manual"}, nil)
		require.NoError(t, err)
		legacy, err := repo.LoadManifest(manual.ID)
		require.NoError(t, err)
		// Before scheduling existed, operators could use this tag for manual captures.
		legacy.Options.Tag = "docbank:scheduled"
		legacyID, err := repo.WriteManifest(legacy)
		require.NoError(t, err)
		legacy.Options.Tag = "docbank:scheduled:another-vault"
		legacy.CreatedAt = time.Now().Add(time.Second).Format(time.RFC3339Nano)
		foreignID, err := repo.WriteManifest(legacy)
		require.NoError(t, err)
		_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: []string{manual.ID}})
		require.NoError(t, err)
		require.NoError(t, os.Remove(repo.Path("docbank-owner")))
		preview, err := backupapp.ClaimRepository(t.Context(), repo, d.Store.VaultID(), false, "")
		require.NoError(t, err)
		_, err = backupapp.ClaimRepository(t.Context(), repo, d.Store.VaultID(), true, preview.Preview)
		require.NoError(t, err)
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 3)
		require.Equal(t, legacyID, points[0].SnapshotID)
		require.Equal(t, foreignID, points[1].SnapshotID)
		previous := points[2].SnapshotID
		require.NoError(t, supervisor.Shutdown(t.Context()))
		supervisor = jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		for range 2 {
			time.Sleep(time.Hour)
			synctest.Wait()
			points, err = repo.ListSnapshots()
			require.NoError(t, err)
			require.Len(t, points, 3)
			require.Equal(t, legacyID, points[0].SnapshotID)
			require.Equal(t, foreignID, points[1].SnapshotID)
			require.NotEqual(t, previous, points[2].SnapshotID)
			previous = points[2].SnapshotID
		}
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
		// Restarts before the interval elapses must not create extra points.
		for range 2 {
			supervisor := jobs.New(t.Context(), d.Logger)
			require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
			synctest.Wait()
			time.Sleep(30 * time.Minute)
			synctest.Wait()
			points, err = repo.ListSnapshots()
			require.NoError(t, err)
			require.Len(t, points, 2)
			require.Equal(t, future, points[1].SnapshotID)
			require.NoError(t, supervisor.Shutdown(t.Context()))
		}
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		require.Equal(t, future, points[1].SnapshotID)
		lock, err := repo.AcquireExclusiveLock("test-recovery-conflict", false)
		require.NoError(t, err)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Contains(t, supervisor.Snapshot()[0].Error, "locked")
		require.NoError(t, lock.Release())
		// A failed due capture retries in five minutes, preserving the consumed deadline.
		time.Sleep(5*time.Minute - time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		time.Sleep(time.Second)
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

func TestBackupScheduleCaptureFailureRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t, time.Minute)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		lock, err := repo.AcquireExclusiveLock("test-capture-conflict", false)
		require.NoError(t, err)
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		require.Contains(t, supervisor.Snapshot()[0].Error, "locked")
		require.NoError(t, lock.Release())
		time.Sleep(time.Minute)
		synctest.Wait()
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Empty(t, supervisor.Snapshot()[0].Error)
		first := points[0].SnapshotID
		time.Sleep(time.Minute)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, first, points[0].SnapshotID)
		require.NoError(t, supervisor.Shutdown(t.Context()))
	})
}

func TestBackupScheduleCleanupFailurePreservesCapture(t *testing.T) {
	t.Parallel()
	d := scheduledDeps(t, time.Hour)
	repo, err := backup.Init(d.Cfg.Backup.Repo)
	require.NoError(t, err)
	created, err := createScheduledBackupSnapshot(t.Context(), repo, d)
	require.NoError(t, err)
	lock, err := repo.AcquireExclusiveLock("test-cleanup-conflict", false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	err = retainScheduledBackups(t.Context(), repo, "", 1, created.ID)
	require.ErrorIs(t, err, backup.ErrRepoLocked)
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

func TestBackupScheduleRejectsDifferentVault(t *testing.T) {
	t.Parallel()
	first := scheduledDeps(t, time.Hour)
	repo, err := backup.Init(first.Cfg.Backup.Repo)
	require.NoError(t, err)
	_, err = scheduledBackup(t.Context(), first, first.Logger, false)
	require.NoError(t, err)
	before, err := repo.ListSnapshots()
	require.NoError(t, err)
	second := scheduledDeps(t, time.Hour)
	second.Cfg.Backup.Repo = first.Cfg.Backup.Repo
	_, err = scheduledBackup(t.Context(), second, second.Logger, false)
	require.ErrorContains(t, err, "another vault")
	after, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Equal(t, before, after)
}
