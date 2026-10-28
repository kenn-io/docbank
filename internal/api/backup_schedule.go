package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
	"go.kenn.io/kit/backup"
)

// effectiveBackupConfig overlays the vault's runtime settings on config.toml.
func effectiveBackupConfig(cfg config.BackupConfig, settings store.PhotoSettings) config.BackupConfig {
	if settings.BackupIntervalSeconds != nil {
		cfg.Interval = config.Duration(time.Duration(*settings.BackupIntervalSeconds) * time.Second)
	}
	if settings.BackupKeepLast != nil {
		cfg.KeepLast = *settings.BackupKeepLast
	}
	return cfg
}

// StartBackupSchedule starts a job only when the effective schedule is enabled.
func StartBackupSchedule(ctx context.Context, d Deps) error {
	// A failed read starts the job anyway. RunBackupSchedule retries the read and holds the daemon.
	settings, err := d.Store.PhotoSettings(ctx)
	if err == nil && effectiveBackupConfig(d.Cfg.Backup, settings).Interval.Std() <= 0 {
		return nil
	}
	return startBackupSchedule(d)
}

func startBackupSchedule(d Deps) error {
	if d.Jobs == nil {
		return nil
	}
	err := d.Jobs.Start("backup:scheduled", func(ctx context.Context) error {
		return RunBackupSchedule(ctx, d)
	})
	if errors.Is(err, jobs.ErrDuplicate) || errors.Is(err, jobs.ErrClosed) {
		return nil
	}
	return err
}

// RunBackupSchedule runs serial captures until its supervisor cancels the job.
// Repositories must already be initialized by the operator.
func RunBackupSchedule(ctx context.Context, d Deps) error {
	return runBackupSchedule(ctx, d, &backupSchedule{})
}

func runBackupSchedule(ctx context.Context, d Deps, schedule *backupSchedule) error {
	// The worker starts only when scheduling is enabled or the start-time read
	// failed, so hold the daemon before the first read can block past idle_timeout.
	holding := false
	hold := func(on bool) {
		if d.Tracker != nil && on != holding {
			if on {
				d.Tracker.Begin()
			} else {
				d.Tracker.End()
			}
		}
		holding = on
	}
	hold(true)
	defer hold(false)
	captureDue := false
	lastFailure := ""
	reportFailure := func(err error, retry time.Duration) {
		jobs.ReportError(ctx, err)
		if failure := err.Error(); failure != lastFailure {
			d.Logger.Error("scheduled backup failed", "error", err, "retry_in", retry)
			lastFailure = failure
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		settings, err := d.Store.PhotoSettings(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A failed read can hide an enabled schedule, so keep the daemon up while retrying.
			hold(true)
			reportFailure(err, time.Minute)
			if err := waitBackupSchedule(ctx, time.Minute, d.BackupScheduleWake); err != nil {
				return err
			}
			continue
		}
		cfg := effectiveBackupConfig(d.Cfg.Backup, settings)
		interval := cfg.Interval.Std()
		if err := cfg.ValidateSchedule(); err != nil {
			// A schedule that cannot run, such as a restored vault's interval
			// override without a configured repository, reports in Jobs but does
			// not keep the daemon awake.
			hold(false)
			captureDue = false
			reportFailure(err, time.Minute)
			if err := waitBackupSchedule(ctx, time.Minute, d.BackupScheduleWake); err != nil {
				return err
			}
			continue
		}
		if interval <= 0 {
			hold(false)
			captureDue, lastFailure = false, ""
			jobs.ReportError(ctx, nil)
			if err := waitBackupSchedule(ctx, time.Minute, d.BackupScheduleWake); err != nil {
				return err
			}
			continue
		}
		hold(true)
		retry := min(interval, 5*time.Minute)
		// Busy covers the capture and retention only, so Jobs shows the
		// schedule idle while it waits for the next deadline.
		jobs.ReportBusy(ctx, true)
		deadline, attempted, err := schedule.capture(ctx, d, d.Logger, cfg, captureDue)
		jobs.ReportBusy(ctx, false)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if schedule.stale {
				err = fmt.Errorf("%w; newest scheduled backup is stale", err)
			}
			reportFailure(err, retry)
			deadline = time.Now().Add(retry)
		} else {
			captureDue = false
			lastFailure = ""
		}
		changed := false
		if waitErr := waitBackupDeadline(ctx, deadline, time.Now, d.BackupScheduleWake, func() (bool, error) {
			next, readErr := d.Store.PhotoSettings(ctx)
			if readErr != nil {
				reportFailure(readErr, time.Minute)
				return false, nil
			}
			if err == nil && lastFailure != "" {
				// Recovery from a read failure must not hide a pending cleanup failure.
				jobs.ReportError(ctx, schedule.cleanupErr)
				lastFailure = ""
			}
			changed = effectiveBackupConfig(d.Cfg.Backup, next).Interval != cfg.Interval
			return changed, nil
		}); waitErr != nil {
			return waitErr
		}
		if changed {
			captureDue = false
		} else if attempted || err == nil {
			// Preserve due captures across failures and consume successful wait deadlines.
			// Preflight failures leave captureDue unchanged: they haven't established a capture is due.
			captureDue = true
		}
	}
}

func waitBackupDeadline(ctx context.Context, deadline time.Time, now func() time.Time, wake <-chan struct{}, changed func() (bool, error)) error {
	// Wall time counts suspension, while timer clocks can stop during sleep.
	// Read settings at each minute wake so runtime changes recompute the deadline.
	wallDeadline := deadline.Round(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if changed != nil {
			if changed, err := changed(); changed || err != nil {
				return err
			}
		}
		// The original monotonic deadline also bounds waits when wall time moves back.
		remaining := min(time.Until(deadline), wallDeadline.Sub(now().Round(0)))
		if remaining <= 0 {
			return nil
		}
		if err := waitBackupSchedule(ctx, min(remaining, time.Minute), wake); err != nil {
			return err
		}
	}
}

func waitBackupSchedule(ctx context.Context, delay time.Duration, wake <-chan struct{}) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}

type backupSchedule struct {
	stale bool
	// pruned means nothing became unreferenced since this run's last prune.
	// The zero value prunes on the first capture to clear earlier leftovers.
	pruned bool
	// beforeForget lets tests move ownership after retention selects points
	// and before Kit locks the repository to forget them.
	beforeForget func()
	// cleanupErr is the latest capture's retention failure, which Jobs keeps
	// showing until a later capture's cleanup succeeds.
	cleanupErr error
}

func (s *backupSchedule) capture(ctx context.Context, d Deps, logger *slog.Logger, cfg config.BackupConfig, captureDue bool) (time.Time, bool, error) {
	interval := cfg.Interval.Std()
	repo, err := backup.Open(cfg.Repo)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("opening scheduled backup repository: %w", err)
	}
	// Only history this installation owns may drive the schedule or stale reports.
	if err := backupapp.EnsureRepositoryOwner(ctx, repo, d.Store, false); err != nil {
		return time.Time{}, false, err
	}
	tag := backupapp.ScheduledTag(d.Store.VaultID())
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return time.Time{}, false, fmt.Errorf("listing scheduled backups: %w", err)
	}
	delay, stale := backupapp.ScheduleDelay(manifests, tag, time.Now(), interval)
	s.reportStaleBackup(ctx, logger, stale)
	if delay > 0 && !captureDue {
		jobs.ReportError(ctx, nil)
		return time.Now().Add(delay), false, nil
	}
	snapshot, err := createScheduledBackupSnapshot(ctx, repo, d)
	if err != nil {
		return time.Time{}, true, err
	}
	captured := time.Now()
	s.stale = false
	logger.Info("scheduled backup captured", "snapshot", snapshot.ID)
	// Cleanup errors surface in Jobs and retry after the next capture.
	err = s.retain(ctx, repo, d.Store, tag, cfg.KeepLast)
	s.cleanupErr = err
	jobs.ReportError(ctx, err)
	if err != nil {
		logger.Warn("scheduled backup cleanup failed", "error", err)
	}
	// Keep the capture's wall and monotonic deadline through retention, including suspension.
	return captured.Add(interval), true, nil
}

// reportStaleBackup surfaces a stale schedule in Jobs and the daemon log.
func (s *backupSchedule) reportStaleBackup(ctx context.Context, logger *slog.Logger, stale bool) {
	const warning = "newest scheduled backup is stale"
	if stale {
		jobs.ReportError(ctx, errors.New(warning))
		if !s.stale {
			logger.Warn(warning)
		}
	}
	s.stale = stale
}

func createScheduledBackupSnapshot(ctx context.Context, repo *backup.Repo, d Deps) (BackupSnapshot, error) {
	return captureBackupSnapshot(ctx, d.Gate, func() (*backup.Manifest, error) {
		return backupapp.CreateScheduled(ctx, repo, version.Version, d.Store, d.Blobs,
			backup.CreateOptions{ZstdLevel: d.Cfg.Backup.ZstdLevel, Freezer: &gateFreezer{gate: d.Gate}})
	})
}

// retain forgets scheduled points beyond keep. It prunes on a run's first
// capture and after a forget, including one that failed partway, so captures
// that remove nothing skip the exclusive pack scan.
func (s *backupSchedule) retain(ctx context.Context, repo *backup.Repo, metadata *store.Store, tag string, keep int) error {
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return fmt.Errorf("listing backups for retention: %w", err)
	}
	if remove := backupapp.ScheduledRetention(manifests, tag, keep); len(remove) > 0 {
		if s.beforeForget != nil {
			s.beforeForget()
		}
		s.pruned = false
		// A restored copy may have claimed the repository since the capture.
		// Claims need the lock Forget holds, so checking under it keeps this
		// installation from deleting history it no longer manages.
		if _, err := backup.Forget(ctx, repo, backup.ForgetOptions{SnapshotIDs: remove, Precondition: func() error {
			return backupapp.RequireRepositoryOwner(ctx, repo, metadata)
		}}); err != nil {
			return fmt.Errorf("forgetting scheduled backups: %w", err)
		}
	}
	if s.pruned {
		return nil
	}
	if _, err := backup.Prune(ctx, repo, backupapp.New(version.Version), backup.PruneOptions{}); err != nil {
		return fmt.Errorf("pruning scheduled backup storage: %w", err)
	}
	s.pruned = true
	return nil
}
