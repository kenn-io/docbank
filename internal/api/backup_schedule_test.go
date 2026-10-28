package api

import (
	"bytes"
	"context"
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
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func backupTestDeps(t *testing.T) Deps {
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
	return Deps{Store: metadata, Blobs: blobs, VaultRoot: root, Gate: NewOperationGate(), Logger: slog.New(slog.DiscardHandler)}
}

func scheduledDeps(t *testing.T) Deps {
	t.Helper()
	d := backupTestDeps(t)
	cfg := config.Default()
	cfg.Backup = config.BackupConfig{Repo: filepath.Join(t.TempDir(), "repo"), Interval: config.Duration(time.Hour), KeepLast: 1}
	d.Cfg = cfg
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

func TestBackupScheduleCaptureAndRetention(t *testing.T) {
	t.Parallel()
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
		_, _, err = schedule.capture(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		_, err = os.Stat(unusedPack)
		require.ErrorIs(t, err, os.ErrNotExist)
		firstScheduled := points[0].SnapshotID
		time.Sleep(time.Hour)
		_, _, err = schedule.capture(t.Context(), d, d.Logger, false)
		require.NoError(t, err)
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 1)
		require.NotEqual(t, firstScheduled, points[0].SnapshotID)
	})
}

func TestBackupSchedulePrunesOnlyAfterForgetting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Cfg.Backup.KeepLast = 2
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		schedule := backupSchedule{}
		_, _, err = schedule.capture(t.Context(), d, d.Logger, false)
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
		_, _, err = schedule.capture(t.Context(), d, d.Logger, true)
		require.NoError(t, err)
		_, err = os.Stat(unusedPack)
		require.NoError(t, err, "a capture that forgets nothing does not prune")
		time.Sleep(time.Hour)
		_, _, err = schedule.capture(t.Context(), d, d.Logger, true)
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
