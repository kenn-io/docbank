package api

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func backupTestDeps(t *testing.T, drivers ...sqlite.Driver) Deps {
	t.Helper()
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"), drivers...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	dir := filepath.Join(root, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return Deps{Store: metadata, Blobs: blobs, VaultRoot: root, Gate: NewOperationGate(), Logger: slog.New(slog.DiscardHandler)}
}

// warmPackEncoders creates Kit's shared zstd encoders outside a synctest
// bubble. A bubble that created them would own their channels, and the next
// test to encode outside it would crash. Parallel tests call it, so the
// sequential memory-limit test does not count the encoders' memory.
var warmPackEncoders = sync.OnceFunc(func() { pack.EncodeFrame(make([]byte, 1024), pack.DefaultZstdLevel) })

func scheduledDeps(t *testing.T, drivers ...sqlite.Driver) Deps {
	t.Helper()
	d := backupTestDeps(t, drivers...)
	cfg := config.Default()
	cfg.Backup = config.BackupConfig{Repo: filepath.Join(t.TempDir(), "repo"), Interval: config.Duration(time.Hour), KeepLast: 1}
	d.Cfg = cfg
	d.BackupScheduleWake = make(chan struct{}, 1)
	return d
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
			warmPackEncoders()
			synctest.Test(t, func(t *testing.T) {
				deadline := time.Now().Add(tc.deadline)
				var suspended atomic.Int64
				finished := make(chan error, 1)
				go func() {
					finished <- waitBackupDeadline(t.Context(), deadline, func() time.Time {
						// Adjust wall time independently of timer time for sleep or clock correction.
						return time.Now().Add(time.Duration(suspended.Load()))
					}, nil, nil)
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

func TestBackupScheduleCaptureAndRetention(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		appender := backup.NewPackAppender(repo, map[pack.BlobID]backup.IndexEntry{}, pack.DefaultZstdLevel, nil, packstore.PackExt)
		t.Cleanup(appender.Abort)
		_, _, err = appender.Add([]byte("unused backup content"))
		require.NoError(t, err)
		packs, entries, err := appender.Finish()
		require.NoError(t, err)
		require.Len(t, packs, 1)
		_, err = repo.WriteIndex(entries)
		require.NoError(t, err)
		unusedPack := repo.Path("packs", packs[0][:2], packs[0]+packstore.PackExt)
		_, err = os.Stat(unusedPack)
		require.NoError(t, err)
		time.Sleep(time.Second)
		schedule := backupSchedule{}
		_, _, err = schedule.capture(t.Context(), d, d.Logger, d.Cfg.Backup, false)
		require.NoError(t, err)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		_, err = os.Stat(unusedPack)
		require.ErrorIs(t, err, os.ErrNotExist)
		firstScheduled := points[0].SnapshotID
		time.Sleep(time.Hour)
		_, _, err = schedule.capture(t.Context(), d, d.Logger, d.Cfg.Backup, false)
		require.NoError(t, err)
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, firstScheduled, points[0].SnapshotID)
	})
}

func TestBackupSchedulePrunesOnlyAfterForgetting(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Cfg.Backup.KeepLast = 2
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		schedule := backupSchedule{}
		_, _, err = schedule.capture(t.Context(), d, d.Logger, d.Cfg.Backup, false)
		require.NoError(t, err)
		// Storage that becomes unreferenced mid-run waits for the next forget.
		appender := backup.NewPackAppender(repo, map[pack.BlobID]backup.IndexEntry{}, pack.DefaultZstdLevel, nil, packstore.PackExt)
		t.Cleanup(appender.Abort)
		_, _, err = appender.Add([]byte("unused backup content"))
		require.NoError(t, err)
		packs, entries, err := appender.Finish()
		require.NoError(t, err)
		_, err = repo.WriteIndex(entries)
		require.NoError(t, err)
		unusedPack := repo.Path("packs", packs[0][:2], packs[0]+packstore.PackExt)
		time.Sleep(time.Hour)
		_, _, err = schedule.capture(t.Context(), d, d.Logger, d.Cfg.Backup, true)
		require.NoError(t, err)
		_, err = os.Stat(unusedPack)
		require.NoError(t, err, "a capture that forgets nothing does not prune")
		time.Sleep(time.Hour)
		_, _, err = schedule.capture(t.Context(), d, d.Logger, d.Cfg.Backup, true)
		require.NoError(t, err)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		_, err = os.Stat(unusedPack)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

// A restored copy can claim the repository after the original captures. The
// original's retention policy must then leave the claimed history alone, even
// when the claim lands after retention has selected points to forget.
func TestBackupScheduleRetentionStopsAfterTakeover(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		beforeForget bool
	}{
		{name: "before retention"},
		{name: "before the forget lock", beforeForget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warmPackEncoders()
			synctest.Test(t, func(t *testing.T) {
				d := scheduledDeps(t)
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
				require.NoError(t, err)
				time.Sleep(time.Hour)
				_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
				require.NoError(t, err)
				restored := restoredDeps(t, repo, d)
				takeover := func() {
					preview, err := backupapp.PreviewRepositoryClaim(t.Context(), repo, restored.Store)
					require.NoError(t, err)
					_, err = backupapp.ClaimRepository(t.Context(), repo, restored.Store, preview.Fingerprint)
					require.NoError(t, err)
				}
				schedule := backupSchedule{}
				if tc.beforeForget {
					schedule.beforeForget = takeover
				} else {
					takeover()
				}
				err = schedule.retain(t.Context(), repo, d.Store, backupapp.ScheduledTag(d.Store.VaultID()), d.Cfg.Backup.KeepLast)
				require.ErrorIs(t, err, backupapp.ErrRepositoryClaimRequired)
				points, err := repo.ListSnapshots()
				require.NoError(t, err)
				require.Len(t, points, 2)
			})
		})
	}
}

// Jobs must show the schedule idle between captures, not as a backup that
// never finishes.
func TestBackupScheduleIsIdleBetweenCaptures(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		first, err := createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		supervisor := jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, supervisor.Shutdown(t.Context())) }()
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		job := supervisor.Snapshot()[0]
		require.Equal(t, jobs.StatusRunning, job.Status)
		require.False(t, job.Busy, "waiting for the first deadline")

		time.Sleep(time.Hour)
		synctest.Wait()
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, first.ID, points[0].SnapshotID, "the due capture replaced the earlier point")
		require.Empty(t, supervisor.Snapshot()[0].Error)
		require.False(t, supervisor.Snapshot()[0].Busy, "waiting after a capture")
	})
}

func TestBackupScheduleRetry(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		recent, err := createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		time.Sleep(20 * time.Minute)
		unavailable := d.Cfg.Backup.Repo + ".unavailable"
		require.NoError(t, os.Rename(d.Cfg.Backup.Repo, unavailable))
		supervisor := jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, supervisor.Shutdown(t.Context())) }()
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		require.Contains(t, supervisor.Snapshot()[0].Error, "opening scheduled backup repository")
		require.NoError(t, os.Rename(unavailable, d.Cfg.Backup.Repo))
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Equal(t, recent.ID, points[0].SnapshotID)
		require.Empty(t, supervisor.Snapshot()[0].Error)
		time.Sleep(35*time.Minute - time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Equal(t, recent.ID, points[0].SnapshotID)
		time.Sleep(time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, recent.ID, points[0].SnapshotID)
	})
}

func TestBackupScheduleRetryAfterClockCorrection(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		time.Sleep(2 * time.Hour)
		lock, err := repo.AcquireExclusiveLock("test-first-due-capture", false)
		require.NoError(t, err)
		defer func() {
			if lock != nil {
				require.NoError(t, lock.Release())
			}
		}()
		supervisor := jobs.New(t.Context(), d.Logger)
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		defer func() { require.NoError(t, supervisor.Shutdown(t.Context())) }()
		synctest.Wait()
		require.Contains(t, supervisor.Snapshot()[0].Error, "locked")
		require.NoError(t, lock.Release())
		lock = nil
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		// Write a recent manifest for the same backup content to model the state
		// ScheduleDelay sees after a backward clock correction; fake time stays fixed.
		_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: []string{points[0].SnapshotID}, AllowEmpty: true})
		require.NoError(t, err)
		points[0].CreatedAt = time.Now().Format(time.RFC3339)
		recent, err := repo.WriteManifest(points[0])
		require.NoError(t, err)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.NotEmpty(t, points)
		assert.NotEqual(t, recent, points[len(points)-1].SnapshotID, "retry must capture despite the recent timestamp")
		assert.Empty(t, supervisor.Snapshot()[0].Error)
	})
}

// capturedHook runs once when the schedule logs a successful capture, which
// happens before retention starts.
type capturedHook struct {
	once sync.Once
	run  func()
}

func (h *capturedHook) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturedHook) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "scheduled backup captured" {
		h.once.Do(h.run)
	}
	return nil
}

func (h *capturedHook) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *capturedHook) WithGroup(string) slog.Handler { return h }

func TestBackupScheduleCountsIntervalFromCapture(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		// A reader holds the repository so the retention prune waits for it.
		held := make(chan *backup.RepoLock, 1)
		d.Logger = slog.New(&capturedHook{run: func() {
			lock, err := repo.AcquireSharedLock("test-reader", false)
			assert.NoError(t, err)
			held <- lock
		}})
		start := time.Now()
		supervisor := jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, supervisor.Shutdown(t.Context())) }()
		require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
		synctest.Wait()
		lock := <-held
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		first := points[0].SnapshotID
		time.Sleep(50 * time.Second)
		require.NoError(t, lock.Release())
		synctest.Wait()
		// Retention finished 50 seconds after the capture; one interval after the capture is still ahead.
		time.Sleep(time.Until(start.Add(time.Hour)) - time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Equal(t, first, points[0].SnapshotID)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, first, points[0].SnapshotID)
	})
}

// restoredDeps restores repo's newest snapshot as a new installation of d's
// vault, the way an operator recovers onto another machine.
func restoredDeps(t *testing.T, repo *backup.Repo, d Deps) Deps {
	t.Helper()
	target := filepath.Join(t.TempDir(), "restored")
	_, err := backupapp.Restore(t.Context(), repo, "test", backup.RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	metadata, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(metadata), filepath.Join(target, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	restored := d
	restored.Store, restored.Blobs, restored.VaultRoot, restored.Gate = metadata, blobs, target, NewOperationGate()
	return restored
}

func TestBackupScheduleLogsUnchangedFailuresOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, message string
	}{
		{name: "owner conflict", message: "scheduled backup failed"},
		// The original installation's old scheduled point must not make the
		// unclaimed restored copy report a stale schedule.
		{name: "restored copy", message: "scheduled backup failed"},
		{name: "stale snapshot", message: `msg="newest scheduled backup is stale"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warmPackEncoders()
			synctest.Test(t, func(t *testing.T) {
				d := scheduledDeps(t)
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				switch tc.name {
				case "owner conflict":
					other := backupTestDeps(t)
					require.NoError(t, backupapp.EnsureRepositoryOwner(t.Context(), repo, other.Store, false))
				case "restored copy":
					_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
					require.NoError(t, err)
					time.Sleep(3 * time.Hour)
					d = restoredDeps(t, repo, d)
				case "stale snapshot":
					_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
					require.NoError(t, err)
					time.Sleep(3 * time.Hour)
					// Another writer keeps the repository busy, so every due capture fails.
					lock, err := repo.AcquireExclusiveLock("other-writer", false)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, lock.Release()) })
				}
				var logs bytes.Buffer
				d.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				supervisor := jobs.New(t.Context(), d.Logger)
				defer func() { require.NoError(t, supervisor.Shutdown(t.Context())) }()
				require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
				synctest.Wait()
				failure := supervisor.Snapshot()[0].Error
				switch tc.name {
				case "owner conflict":
					require.Equal(t, backupapp.ErrRepositoryOwnerConflict.Error(), failure)
				case "restored copy":
					require.Equal(t, backupapp.ErrRepositoryClaimRequired.Error(), failure)
				case "stale snapshot":
					require.Contains(t, failure, "lock")
					require.True(t, strings.HasSuffix(failure, "; newest scheduled backup is stale"), failure)
				}
				require.Equal(t, 1, strings.Count(logs.String(), tc.message))
				time.Sleep(5 * time.Minute)
				synctest.Wait()
				require.Equal(t, 1, strings.Count(logs.String(), tc.message))
			})
		})
	}
}

// seedFutureScheduledSnapshot leaves one scheduled snapshot dated 48h ahead on a 24h interval.
// The future date matters: ScheduleDelay returns the full interval for it, so a recomputed
// deadline equals the original one and only a pushed-back deadline shows.
func seedFutureScheduledSnapshot(t *testing.T, d *Deps, repo *backup.Repo) []*backup.Manifest {
	t.Helper()
	d.Cfg.Backup.Interval = config.Duration(24 * time.Hour)
	_, err := createScheduledBackupSnapshot(t.Context(), repo, *d)
	require.NoError(t, err)
	points, err := repo.ListSnapshots()
	require.NoError(t, err)
	require.Len(t, points, 1)
	_, err = backup.Forget(t.Context(), repo, backup.ForgetOptions{SnapshotIDs: []string{points[0].SnapshotID}, AllowEmpty: true})
	require.NoError(t, err)
	// Model a backward clock correction without moving synctest's timer clock.
	points[0].CreatedAt = time.Now().Add(48 * time.Hour).Format(time.RFC3339)
	_, err = repo.WriteManifest(points[0])
	require.NoError(t, err)
	return points
}

func TestBackupScheduleRuntimeOverrides(t *testing.T) {
	t.Parallel()
	t.Run("interval", testBackupScheduleRuntimeIntervalOverrides)
	t.Run("retention preserves deadline", testBackupScheduleRuntimeRetentionOverride)
}

func testBackupScheduleRuntimeRetentionOverride(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		points := seedFutureScheduledSnapshot(t, &d, repo)
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		synctest.Wait()
		time.Sleep(23 * time.Hour)
		_, err = d.Store.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{BackupKeepLast: new(2)})
		require.NoError(t, err)
		d.BackupScheduleWake <- struct{}{}
		synctest.Wait()
		unchanged, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Equal(t, points, unchanged, "retention changes wait for the next capture")
		time.Sleep(time.Hour)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2, "capture at the original deadline must use the updated retention")
	})
}

func testBackupScheduleRuntimeIntervalOverrides(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Cfg.Backup.Interval, d.Cfg.Backup.KeepLast = 0, 0
		d.Tracker = NewActivityTracker()
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		require.Empty(t, d.Jobs.Snapshot())
		time.Sleep(time.Second)
		_, err = d.Store.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{BackupIntervalSeconds: new(int64(3600)), BackupKeepLast: new(1)})
		require.NoError(t, err)
		d.BackupScheduleWake <- struct{}{}
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		synctest.Wait()
		require.Len(t, d.Jobs.Snapshot(), 1)
		require.Zero(t, d.Tracker.IdleFor())
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		time.Sleep(30 * time.Minute)
		_, err = d.Store.SetPhotoSettings(t.Context(), 2, store.PhotoSettingsUpdate{BackupIntervalSeconds: new(int64(7200))})
		require.NoError(t, err)
		d.BackupScheduleWake <- struct{}{}
		time.Sleep(time.Hour)
		synctest.Wait()
		unchanged, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Equal(t, points, unchanged)
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, unchanged, points)
		_, err = d.Store.SetPhotoSettings(t.Context(), 3, store.PhotoSettingsUpdate{ResetBackupInterval: true})
		require.NoError(t, err)
		d.BackupScheduleWake <- struct{}{}
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Positive(t, d.Tracker.IdleFor())
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		unchanged, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Equal(t, points, unchanged)
		_, err = d.Store.SetPhotoSettings(t.Context(), 4, store.PhotoSettingsUpdate{BackupIntervalSeconds: new(int64(7200))})
		require.NoError(t, err)
		d.BackupScheduleWake <- struct{}{}
		synctest.Wait()
		time.Sleep(time.Second)
		require.Zero(t, d.Tracker.IdleFor(), "re-enabled schedule must hold the daemon during its disabled wait")
	})
}

func TestBackupScheduleInvalidEffectiveConfig(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Tracker = NewActivityTracker()
		d.Cfg.Backup.Interval, d.Cfg.Backup.KeepLast = 0, 0
		// Metadata restore validates overrides independently of this daemon's config.
		_, err := d.Store.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{BackupIntervalSeconds: new(int64(3600))})
		require.NoError(t, err)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		synctest.Wait()
		require.Contains(t, d.Jobs.Snapshot()[0].Error, "positive keep_last")
		time.Sleep(time.Second)
		require.NotZero(t, d.Tracker.IdleFor(), "a schedule that cannot run must not hold the daemon")
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Empty(t, points, "invalid retention must prevent capture")
		_, err = d.Store.SetPhotoSettings(t.Context(), 2, store.PhotoSettingsUpdate{BackupKeepLast: new(1)})
		require.NoError(t, err)
		time.Sleep(time.Minute)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.Empty(t, d.Jobs.Snapshot()[0].Error)
		require.Zero(t, d.Tracker.IdleFor(), "a usable schedule holds the daemon again")
		require.NoError(t, d.Jobs.Shutdown(t.Context()))
		time.Sleep(time.Second)
		require.Positive(t, d.Tracker.IdleFor(), "shutdown must release the schedule hold")
	})
}

func TestBackupScheduleSettingsReadRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		duringWait     bool
		futureSnapshot bool
	}{
		{name: "initial read"},
		{name: "deadline read", duringWait: true},
		{name: "future snapshot deadline read", duringWait: true, futureSnapshot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warmPackEncoders()
			synctest.Test(t, func(t *testing.T) {
				driver := &backupReadContentionDriver{Driver: store.DefaultSQLiteDriver()}
				d := scheduledDeps(t, driver)
				// The interval comes only from the stored override, which a failed read cannot supply.
				d.Cfg.Backup.Interval = 0
				d.Tracker = NewActivityTracker()
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				intervalSeconds := int64(3600)
				if tc.futureSnapshot {
					// Match the seeded 24h interval so no capture precedes the contention, and keep
					// the future-dated snapshot beside the new capture.
					intervalSeconds = 24 * 60 * 60
					d.Cfg.Backup.KeepLast = 2
				}
				_, err = d.Store.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{BackupIntervalSeconds: &intervalSeconds})
				require.NoError(t, err)
				if tc.futureSnapshot {
					seedFutureScheduledSnapshot(t, &d, repo)
				}
				db, err := driver.Driver.Open(filepath.Join(d.VaultRoot, "docbank.db"), sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Immediate, BusyTimeout: time.Millisecond})
				require.NoError(t, err)
				defer func() { require.NoError(t, db.Close()) }()
				db.SetMaxOpenConns(1)
				d.Jobs = jobs.New(t.Context(), d.Logger)
				defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
				if tc.duringWait {
					require.NoError(t, startBackupSchedule(d))
					synctest.Wait()
					if tc.futureSnapshot {
						time.Sleep(24*time.Hour - 2*time.Minute)
					} else {
						time.Sleep(time.Hour)
					}
					synctest.Wait()
				}
				before, err := repo.ListSnapshots()
				require.NoError(t, err)
				// Release idle store connections so the external locker alone owns the lock.
				for _, pool := range driver.dbs {
					pool.SetMaxOpenConns(1)
					pool.SetMaxIdleConns(0)
				}
				_, err = db.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE")
				require.NoError(t, err)
				tx, err := db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				if !tc.duringWait {
					require.NoError(t, StartBackupSchedule(t.Context(), d))
				} else {
					time.Sleep(time.Minute)
				}
				synctest.Wait()
				status := d.Jobs.Snapshot()[0]
				require.Equal(t, jobs.StatusRunning, status.Status)
				require.Contains(t, status.Error, "locked")
				time.Sleep(time.Second)
				require.Zero(t, d.Tracker.IdleFor(), "a failed settings read must keep the daemon held")
				for _, pool := range driver.dbs {
					pool.SetMaxIdleConns(1)
				}
				require.NoError(t, tx.Rollback())
				require.NoError(t, db.Close())
				if tc.futureSnapshot {
					time.Sleep(30 * time.Second)
					synctest.Wait()
					early, err := repo.ListSnapshots()
					require.NoError(t, err)
					require.Equal(t, before, early, "recovered reads must keep the original deadline")
					time.Sleep(30 * time.Second)
				} else {
					time.Sleep(time.Minute)
				}
				synctest.Wait()
				require.Empty(t, d.Jobs.Snapshot()[0].Error)
				points, err := repo.ListSnapshots()
				require.NoError(t, err)
				if tc.futureSnapshot {
					require.Len(t, points, 2, "a read retry must capture at the original deadline despite the future timestamp")
					var kept bool
					for _, point := range points {
						kept = kept || point.SnapshotID == before[0].SnapshotID
					}
					require.True(t, kept, "retention must keep the future-dated snapshot")
					return
				}
				require.Len(t, points, 1)
				if tc.duringWait {
					require.Equal(t, before, points, "a read retry must preserve the successful capture's deadline")
				}
			})
		})
	}
}

// A restored vault can carry an interval override to a home whose config.toml
// has no repository. That schedule cannot run, so it must report the problem
// without keeping the daemon awake forever.
func TestBackupScheduleUnusableOverrideLetsDaemonIdle(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Cfg.Backup = config.BackupConfig{}
		d.Tracker = NewActivityTracker()
		_, err := d.Store.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{BackupIntervalSeconds: new(int64(3600)), BackupKeepLast: new(1)})
		require.NoError(t, err)
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		synctest.Wait()
		require.Contains(t, d.Jobs.Snapshot()[0].Error, "repo and positive keep_last")
		time.Sleep(time.Second)
		require.NotZero(t, d.Tracker.IdleFor(), "an unusable schedule must not hold the daemon")
	})
}

// A settings read that fails and recovers while the schedule waits must not
// hide the cleanup failure from the capture before it.
func TestBackupScheduleReadRecoveryKeepsCleanupFailure(t *testing.T) {
	t.Parallel()
	warmPackEncoders()
	synctest.Test(t, func(t *testing.T) {
		driver := &backupReadContentionDriver{Driver: store.DefaultSQLiteDriver()}
		d := scheduledDeps(t, driver)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
		require.NoError(t, err)
		time.Sleep(time.Hour)
		// Another writer holds the repository when retention tries to forget the old point.
		var writer *backup.RepoLock
		schedule := &backupSchedule{beforeForget: func() {
			lock, err := repo.AcquireExclusiveLock("other-writer", false)
			require.NoError(t, err)
			writer = lock
		}}
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		require.NoError(t, d.Jobs.Start("backup:scheduled", func(ctx context.Context) error {
			return runBackupSchedule(ctx, d, schedule)
		}))
		synctest.Wait()
		require.NotNil(t, writer)
		require.NoError(t, writer.Release())
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2, "the capture succeeds; only its cleanup fails")
		cleanup := d.Jobs.Snapshot()[0].Error
		require.Contains(t, cleanup, "forgetting scheduled backups")

		db, err := driver.Driver.Open(filepath.Join(d.VaultRoot, "docbank.db"), sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Immediate, BusyTimeout: time.Millisecond})
		require.NoError(t, err)
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(1)
		// Release idle store connections so the external locker alone owns the lock.
		for _, pool := range driver.dbs {
			pool.SetMaxOpenConns(1)
			pool.SetMaxIdleConns(0)
		}
		_, err = db.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE")
		require.NoError(t, err)
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Contains(t, d.Jobs.Snapshot()[0].Error, "database is locked")

		for _, pool := range driver.dbs {
			pool.SetMaxIdleConns(1)
		}
		require.NoError(t, tx.Rollback())
		require.NoError(t, db.Close())
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Equal(t, cleanup, d.Jobs.Snapshot()[0].Error)
	})
}

type backupReadContentionDriver struct {
	sqlite.Driver

	dbs []*sql.DB
}

func (d *backupReadContentionDriver) Open(path string, opts sqlite.OpenOptions) (*sql.DB, error) {
	opts.BusyTimeout = time.Millisecond //nolint:kennlint // deadlinetest: this driver runs only inside the backup schedule synctest bubble
	db, err := d.Driver.Open(path, opts)
	if err == nil {
		d.dbs = append(d.dbs, db)
	}
	return db, err
}
