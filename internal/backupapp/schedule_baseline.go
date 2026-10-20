package backupapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/backup"
)

const scheduleBaselineFile = "docbank-schedule-baseline"

// ScheduleBaseline preserves existing history before automatic captures begin.
// Callers must check repository ownership first. An empty boundary is persisted
// too, so a later restart cannot move it past the first automatic capture.
func ScheduleBaseline(ctx context.Context, repo *backup.Repo) (baseline string, err error) {
	data, err := os.ReadFile(repo.Path(scheduleBaselineFile))
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("reading scheduled backup baseline: %w", err)
	}
	lock, err := repo.AcquireExclusiveLockContext(ctx, "docbank-schedule-baseline", false)
	if err != nil {
		return "", fmt.Errorf("locking scheduled backup baseline: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Release()) }()
	data, err = os.ReadFile(repo.Path(scheduleBaselineFile))
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("reading scheduled backup baseline: %w", err)
	}
	manifests, err := repo.ListSnapshots()
	if err != nil {
		return "", fmt.Errorf("listing backups for schedule baseline: %w", err)
	}
	if len(manifests) > 0 {
		baseline = manifests[len(manifests)-1].SnapshotID
	}
	if err := atomicfile.WriteNew(repo.Path(scheduleBaselineFile), []byte(baseline+"\n")); err != nil {
		return "", fmt.Errorf("writing scheduled backup baseline: %w", err)
	}
	return baseline, nil
}
