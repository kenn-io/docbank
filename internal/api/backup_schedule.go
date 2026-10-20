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
	if err := d.Cfg.Validate(); err != nil {
		return err
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	retry := min(interval, 5*time.Minute)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, err := scheduledBackup(ctx, d, logger)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			jobs.ReportError(ctx, err)
			logger.Error("scheduled backup failed", "error", err, "retry_in", retry)
			delay = retry
		}
		if err := jobs.Wait(ctx, delay); err != nil {
			return err
		}
	}
}

func scheduledBackup(ctx context.Context, d Deps, logger *slog.Logger) (time.Duration, error) {
	interval := d.Cfg.Backup.Interval.Std()
	if err := backupapp.ValidateDisjointRoots(d.VaultRoot, d.Cfg.Backup.Repo); err != nil {
		return 0, err
	}
	repo, err := backup.Open(d.Cfg.Backup.Repo)
	if err != nil {
		return 0, fmt.Errorf("opening scheduled backup repository: %w", err)
	}
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return 0, fmt.Errorf("listing scheduled backups: %w", err)
	}
	delay, stale, err := backupapp.ScheduleDelay(manifests, time.Now(), interval)
	if err != nil {
		return 0, err
	}
	if stale {
		warning := errors.New("newest scheduled backup is stale")
		jobs.ReportError(ctx, warning)
		logger.Warn(warning.Error())
	}
	if delay > 0 {
		return delay, nil
	}
	snapshot, err := createBackupSnapshot(ctx, repo, d, d.Gate,
		backupCreateRequest{Tag: backupapp.ScheduledTag}, nil)
	if err != nil {
		return 0, err
	}
	logger.Info("scheduled backup captured", "snapshot", snapshot.ID)
	// Cleanup errors leave the new point intact and retry after the next capture.
	err = retainScheduledBackups(ctx, repo, d.Cfg.Backup.KeepLast, snapshot.ID)
	jobs.ReportError(ctx, err)
	if err != nil {
		logger.Warn("scheduled backup cleanup failed", "error", err)
	}
	// Also bounds recovery captures when the repository's clock is in the future.
	return interval, nil
}

func retainScheduledBackups(ctx context.Context, repo *backup.Repo, keep int, created string) error {
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return fmt.Errorf("listing backups for retention: %w", err)
	}
	remove, err := backupapp.ScheduledRetention(manifests, keep, created)
	if err != nil {
		return err
	}
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
