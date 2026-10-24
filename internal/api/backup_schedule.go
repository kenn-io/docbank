package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/version"
	"go.kenn.io/kit/backup"
)

// RunBackupSchedule runs serial captures until its supervisor cancels the job.
// Repositories must already be initialized by the operator.
func RunBackupSchedule(ctx context.Context, d Deps) error {
	interval := d.Cfg.Backup.Interval.Std()
	if interval <= 0 {
		return nil
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	retry := min(interval, 5*time.Minute)
	captureDue := false
	lastFailure := ""
	schedule := backupSchedule{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, attempted, err := schedule.capture(ctx, d, logger, captureDue)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if schedule.stale {
				err = fmt.Errorf("%w; newest scheduled backup is stale", err)
			}
			jobs.ReportError(ctx, err)
			if failure := err.Error(); failure != lastFailure {
				logger.Error("scheduled backup failed", "error", err, "retry_in", retry)
				lastFailure = failure
			}
			delay = retry
		} else {
			lastFailure = ""
		}
		if err := waitBackupDeadline(ctx, time.Now().Add(delay), time.Now); err != nil {
			return err
		}
		if attempted || err == nil {
			// Preserve due captures across failures and consume successful wait deadlines.
			// Preflight failures leave captureDue unchanged: they haven't established a capture is due.
			captureDue = true
		}
	}
}

func waitBackupDeadline(ctx context.Context, deadline time.Time, now func() time.Time) error {
	// Wall time counts suspension, while timer clocks can stop during sleep.
	// Poll only the deadline so retries keep their original delay.
	wallDeadline := deadline.Round(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The original monotonic deadline also bounds waits when wall time moves back.
		remaining := min(time.Until(deadline), wallDeadline.Sub(now().Round(0)))
		if remaining <= 0 {
			return nil
		}
		if err := jobs.Wait(ctx, min(remaining, time.Minute)); err != nil {
			return err
		}
	}
}

type backupSchedule struct {
	stale bool
}

func (s *backupSchedule) capture(ctx context.Context, d Deps, logger *slog.Logger, captureDue bool) (time.Duration, bool, error) {
	interval := d.Cfg.Backup.Interval.Std()
	repo, err := backup.Open(d.Cfg.Backup.Repo)
	if err != nil {
		return 0, false, fmt.Errorf("opening scheduled backup repository: %w", err)
	}
	tag := backupapp.ScheduledTag(d.Store.VaultID())
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return 0, false, fmt.Errorf("listing scheduled backups: %w", err)
	}
	delay, stale, err := backupapp.ScheduleDelay(manifests, tag, time.Now(), interval)
	if err != nil {
		return 0, false, err
	}
	s.reportStaleBackup(ctx, logger, stale)
	if delay > 0 && !captureDue {
		if err := backupapp.EnsureCaptureAllowed(ctx, repo, d.Store, false); err != nil {
			return 0, false, err
		}
		jobs.ReportError(ctx, nil)
		return delay, false, nil
	}
	snapshot, err := createScheduledBackupSnapshot(ctx, repo, d)
	if err != nil {
		return 0, true, err
	}
	s.stale = false
	logger.Info("scheduled backup captured", "snapshot", snapshot.ID)
	// Cleanup errors surface in Jobs and retry after the next capture.
	err = retainScheduledBackups(ctx, repo, tag, d.Cfg.Backup.KeepLast)
	jobs.ReportError(ctx, err)
	if err != nil {
		logger.Warn("scheduled backup cleanup failed", "error", err)
	}
	return interval, true, nil
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

func retainScheduledBackups(ctx context.Context, repo *backup.Repo, tag string, keep int) error {
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return fmt.Errorf("listing backups for retention: %w", err)
	}
	remove := backupapp.ScheduledRetention(manifests, tag, keep)
	if len(remove) > 0 {
		if _, err := backup.Forget(ctx, repo, backup.ForgetOptions{SnapshotIDs: remove}); err != nil {
			return fmt.Errorf("forgetting scheduled backups: %w", err)
		}
	}
	_, err = backup.Prune(ctx, repo, backupapp.New(version.Version), backup.PruneOptions{})
	if err != nil {
		return fmt.Errorf("pruning scheduled backup storage: %w", err)
	}
	return nil
}
