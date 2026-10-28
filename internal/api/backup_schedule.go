package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
	"go.kenn.io/kit/backup"
)

// RunBackupSchedule runs serial captures until its supervisor cancels the job.
// Repositories must already be initialized by the operator.
func RunBackupSchedule(ctx context.Context, d Deps) error {
	interval := d.Cfg.Backup.Interval.Std()
	retry := min(interval, 5*time.Minute)
	captureDue := false
	lastFailure := ""
	schedule := backupSchedule{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Busy covers the capture and retention only, so Jobs shows the
		// schedule idle while it waits for the next deadline.
		jobs.ReportBusy(ctx, true)
		deadline, attempted, err := schedule.capture(ctx, d, d.Logger, captureDue)
		jobs.ReportBusy(ctx, false)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if schedule.stale {
				err = fmt.Errorf("%w; newest scheduled backup is stale", err)
			}
			jobs.ReportError(ctx, err)
			if failure := err.Error(); failure != lastFailure {
				d.Logger.Error("scheduled backup failed", "error", err, "retry_in", retry)
				lastFailure = failure
			}
			deadline = time.Now().Add(retry)
		} else {
			lastFailure = ""
		}
		if err := waitBackupDeadline(ctx, deadline, time.Now); err != nil {
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
	// pruned means nothing became unreferenced since this run's last prune.
	// The zero value prunes on the first capture to clear earlier leftovers.
	pruned bool
	// beforeForget lets tests move ownership after retention selects points
	// and before Kit locks the repository to forget them.
	beforeForget func()
}

func (s *backupSchedule) capture(ctx context.Context, d Deps, logger *slog.Logger, captureDue bool) (time.Time, bool, error) {
	interval := d.Cfg.Backup.Interval.Std()
	repo, err := backup.Open(d.Cfg.Backup.Repo)
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
	err = s.retain(ctx, repo, d.Store, tag, d.Cfg.Backup.KeepLast)
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
