package api

import (
	"cmp"
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, err := scheduledBackup(ctx, d, logger, captureDue)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			jobs.ReportError(ctx, err)
			logger.Error("scheduled backup failed", "error", err, "retry_in", retry)
			delay = retry
		}
		if err := waitBackupDeadline(ctx, time.Now().Add(delay), time.Now); err != nil {
			return err
		}
		if err == nil {
			// Consume the elapsed capture deadline even if Kit timestamps remain
			// ahead of wall time after a clock correction. Retries keep this due state.
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

func scheduledBackup(ctx context.Context, d Deps, logger *slog.Logger, captureDue bool) (time.Duration, error) {
	interval := d.Cfg.Backup.Interval.Std()
	if err := backupapp.ValidateDisjointRoots(d.VaultRoot, d.Cfg.Backup.Repo); err != nil {
		return 0, err
	}
	repo, err := backup.Open(d.Cfg.Backup.Repo)
	if err != nil {
		return 0, fmt.Errorf("opening scheduled backup repository: %w", err)
	}
	if err := backupapp.EnsureRepositoryOwner(ctx, repo, d.Store.VaultID(), false); err != nil {
		return 0, err
	}
	tag := backupapp.ScheduledTag(d.Store.VaultID())
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return 0, fmt.Errorf("listing scheduled backups: %w", err)
	}
	delay, stale, err := backupapp.ScheduleDelay(manifests, tag, time.Now(), interval)
	if err != nil {
		return 0, err
	}
	warning := reportStaleBackup(ctx, logger, stale)
	if delay > 0 && !captureDue {
		jobs.ReportError(ctx, nil)
		return delay, nil
	}
	snapshot, err := createScheduledBackupSnapshot(ctx, repo, d)
	if err != nil {
		return 0, err
	}
	logger.Info("scheduled backup captured", "snapshot", snapshot.ID)
	// Cleanup errors leave the new point intact and retry after the next capture.
	err = retainScheduledBackups(ctx, repo, tag, d.Cfg.Backup.KeepLast, snapshot.ID)
	// A successful recovery keeps the stale warning until a healthy check clears it.
	jobs.ReportError(ctx, cmp.Or(err, warning))
	if err != nil {
		logger.Warn("scheduled backup cleanup failed", "error", err)
	}
	return interval, nil
}

// reportStaleBackup surfaces a stale schedule in Jobs and the daemon log.
func reportStaleBackup(ctx context.Context, logger *slog.Logger, stale bool) error {
	if !stale {
		return nil
	}
	warning := errors.New("newest scheduled backup is stale")
	jobs.ReportError(ctx, warning)
	logger.Warn(warning.Error())
	return warning
}

func createScheduledBackupSnapshot(ctx context.Context, repo *backup.Repo, d Deps) (BackupSnapshot, error) {
	return captureBackupSnapshot(ctx, d.Gate, func() (*backup.Manifest, error) {
		return backupapp.CreateScheduled(ctx, repo, version.Version, d.Store, d.Blobs,
			backup.CreateOptions{ZstdLevel: d.Cfg.Backup.ZstdLevel, Freezer: &gateFreezer{gate: d.Gate}})
	})
}

func retainScheduledBackups(ctx context.Context, repo *backup.Repo, tag string, keep int, created string) error {
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return fmt.Errorf("listing backups for retention: %w", err)
	}
	remove := backupapp.ScheduledRetention(manifests, tag, keep, created)
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
