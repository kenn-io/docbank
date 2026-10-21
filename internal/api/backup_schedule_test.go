package api

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestBackupScheduleRetry(t *testing.T) {
	t.Parallel()
	for _, clockCorrection := range []bool{false, true} {
		name := "retry after preflight failure"
		if clockCorrection {
			name = "clock correction after due failure"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if clockCorrection {
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

					return
				}
				d := scheduledDeps(t)
				supervisor := jobs.New(t.Context(), d.Logger)
				defer supervisor.Stop()
				require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
				synctest.Wait()
				require.Contains(t, supervisor.Snapshot()[0].Error, "opening scheduled backup repository")
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				time.Sleep(5*time.Minute - time.Second)
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
			})
		})
	}
}

func TestBackupScheduleLogsUnchangedFailuresOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		stale   bool
		failure string
		message string
	}{
		{name: "owner conflict", failure: backupapp.ErrRepositoryOwnerConflict.Error(), message: "scheduled backup failed"},
		{name: "stale snapshot", stale: true, failure: backupapp.ErrRepositoryOwnerConflict.Error() + "; newest scheduled backup is stale", message: `msg="newest scheduled backup is stale"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := scheduledDeps(t)
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				if tc.stale {
					_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
					require.NoError(t, err)
					time.Sleep(3 * time.Hour)
				}
				require.NoError(t, os.WriteFile(repo.Path("docbank-owner"), []byte("other-vault"), 0o600))
				var logs bytes.Buffer
				d.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				supervisor := jobs.New(t.Context(), d.Logger)
				defer func() { require.NoError(t, supervisor.Shutdown(t.Context())) }()
				require.NoError(t, supervisor.Start("backup:scheduled", func(ctx context.Context) error { return RunBackupSchedule(ctx, d) }))
				synctest.Wait()
				require.Equal(t, tc.failure, supervisor.Snapshot()[0].Error)
				require.Equal(t, 1, strings.Count(logs.String(), tc.message))
				time.Sleep(5 * time.Minute)
				synctest.Wait()
				require.Equal(t, 1, strings.Count(logs.String(), tc.message))
			})
		})
	}
}

func TestBackupScheduleRuntimeOverrides(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Cfg.Backup.Interval, d.Cfg.Backup.KeepLast = 0, 0
		d.Cfg.Server.APIKey = "synthetic-key"
		d.Tracker = NewActivityTracker()
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		manual, err := backupapp.Create(t.Context(), repo, "test", d.Store, d.Blobs, backup.CreateOptions{Tag: "manual"})
		require.NoError(t, err)
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		require.Empty(t, d.Jobs.Snapshot())
		time.Sleep(time.Second)
		server := NewServer(d)
		defer server.Close()
		put := func(revision string, body string) {
			req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/api/v1/photos/settings", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", d.Cfg.Server.APIKey)
			req.Header.Set("If-Match", revision)
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, req)
			require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		}
		put(`"1"`, `{"backup_interval_seconds":3600,"backup_keep_last":1}`)
		synctest.Wait()
		require.Len(t, d.Jobs.Snapshot(), 1)
		require.Zero(t, d.Tracker.IdleFor())
		points, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		time.Sleep(30 * time.Minute)
		put(`"2"`, `{"backup_interval_seconds":7200}`)
		time.Sleep(time.Hour)
		synctest.Wait()
		unchanged, err := repo.ListSnapshots()
		require.NoError(t, err)
		require.Equal(t, points, unchanged)
		time.Sleep(30 * time.Minute)
		synctest.Wait()
		points, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Len(t, points, 2)
		require.NotEqual(t, unchanged, points)
		_, err = repo.LoadManifest(manual.SnapshotID)
		require.NoError(t, err)
		put(`"3"`, `{"reset_backup_interval":true}`)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Positive(t, d.Tracker.IdleFor())
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		unchanged, err = repo.ListSnapshots()
		require.NoError(t, err)
		require.Equal(t, points, unchanged)
		put(`"4"`, `{"backup_interval_seconds":7200}`)
		synctest.Wait()
		time.Sleep(time.Second)
		require.Zero(t, d.Tracker.IdleFor(), "re-enabled schedule must hold the daemon during its disabled wait")
		require.NoError(t, d.Jobs.Shutdown(t.Context()))
		put(`"5"`, `{"backup_interval_seconds":3600}`)
	})
}

func TestBackupScheduleInvalidEffectiveConfig(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := scheduledDeps(t)
		d.Cfg.Backup.Interval, d.Cfg.Backup.KeepLast = 0, 0
		// Metadata restore validates overrides independently of this daemon's config.
		_, err := d.Store.SetPhotoSettings(t.Context(), 1, store.PhotoSettingsUpdate{BackupIntervalSeconds: new(int64(3600))})
		require.NoError(t, err)
		repo, err := backup.Init(d.Cfg.Backup.Repo)
		require.NoError(t, err)
		var logs bytes.Buffer
		d.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		d.Jobs = jobs.New(t.Context(), d.Logger)
		defer func() { require.NoError(t, d.Jobs.Shutdown(t.Context())) }()
		require.NoError(t, StartBackupSchedule(t.Context(), d))
		synctest.Wait()
		require.Contains(t, d.Jobs.Snapshot()[0].Error, "positive keep_last")
		require.Contains(t, logs.String(), "positive keep_last")
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
			synctest.Test(t, func(t *testing.T) {
				driver := &backupReadContentionDriver{Driver: store.DefaultSQLiteDriver()}
				d := scheduledDeps(t, driver)
				repo, err := backup.Init(d.Cfg.Backup.Repo)
				require.NoError(t, err)
				if tc.futureSnapshot {
					d.Cfg.Backup.Interval = config.Duration(24 * time.Hour)
					_, err = createScheduledBackupSnapshot(t.Context(), repo, d)
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
					require.NoError(t, startBackupSchedule(d))
				} else {
					time.Sleep(time.Minute)
				}
				synctest.Wait()
				status := d.Jobs.Snapshot()[0]
				require.Equal(t, jobs.StatusRunning, status.Status)
				require.Contains(t, status.Error, "locked")
				for _, pool := range driver.dbs {
					pool.SetMaxIdleConns(1)
				}
				require.NoError(t, tx.Rollback())
				require.NoError(t, db.Close())
				time.Sleep(time.Minute)
				synctest.Wait()
				require.Empty(t, d.Jobs.Snapshot()[0].Error)
				points, err := repo.ListSnapshots()
				require.NoError(t, err)
				require.Len(t, points, 1)
				if tc.futureSnapshot {
					require.NotEqual(t, before[0].SnapshotID, points[0].SnapshotID, "a read retry must capture at the original deadline despite the future timestamp")
				} else if tc.duringWait {
					require.Equal(t, before, points, "a read retry must preserve the successful capture's deadline")
				}
			})
		})
	}
}

type backupReadContentionDriver struct {
	sqlite.Driver

	dbs []*sql.DB
}

func (d *backupReadContentionDriver) Open(path string, opts sqlite.OpenOptions) (*sql.DB, error) {
	opts.BusyTimeout = time.Millisecond
	db, err := d.Driver.Open(path, opts)
	if err == nil {
		d.dbs = append(d.dbs, db)
	}
	return db, err
}
