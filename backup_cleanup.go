package docbank

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kit/backup"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/version"
)

var (
	// ErrBackupLastSnapshot means removing the selection needs AllowEmpty.
	ErrBackupLastSnapshot = backup.ErrLastSnapshot
	// ErrBackupSnapshotRequired means a retained incremental snapshot needs a selected parent.
	ErrBackupSnapshotRequired = backup.ErrSnapshotRequired
)

// BackupForgetOptions selects exact recovery points, not an automatic retention policy.
type BackupForgetOptions struct {
	SnapshotIDs []string // required; duplicates are ignored
	DryRun      bool
	AllowEmpty  bool // explicitly allow removing the last recovery point
	ForceUnlock bool // recover an abandoned repository lock
}

// BackupForgetReport lists the dependency-safe selection and completed removals.
// Forgotten can be partial on error; its last removal may not be durable if
// directory syncing failed. Removing snapshots does not reclaim packed bytes.
type BackupForgetReport struct {
	Selected  []string
	Forgotten []string
}

// BackupPruneOptions controls unused backup storage cleanup, not live-vault GC.
type BackupPruneOptions struct {
	DryRun      bool
	ForceUnlock bool
	Progress    func(BackupProgress)
}

// BackupPruneReport counts pack files, excluding indexes and temporary scratch.
// Planned bytes are not net savings: replacement encoding and framing affect
// the final size. Actual counts may be partial on error. Cleanup is not secure erasure.
type BackupPruneReport struct {
	PacksToRemove      []string // includes old packs selected for rewriting
	PacksToRepack      []string
	BytesToRemove      int64
	LiveBytesToRewrite uint64 // current encoded payload bytes to copy
	RemovedPacks       []string
	BytesRemoved       int64
	BytesWritten       int64
}

// Forget removes recovery points under Kit's exclusive repository lock.
// It refuses a selection needed by a retained snapshot or the last recovery
// point without AllowEmpty. DryRun checks the same rules without removal.
// No source vault is needed. Call Prune separately to reclaim unused storage.
func (r *BackupRepository) Forget(ctx context.Context, opts BackupForgetOptions) (BackupForgetReport, error) {
	if r == nil || r.repo == nil {
		return BackupForgetReport{}, errors.New("docbank backup repository is required")
	}
	result, err := backup.Forget(ctx, r.repo, backup.ForgetOptions{
		SnapshotIDs: opts.SnapshotIDs, DryRun: opts.DryRun,
		AllowEmpty: opts.AllowEmpty, ForceUnlock: opts.ForceUnlock,
	})
	var report BackupForgetReport
	if result != nil {
		report = BackupForgetReport{Selected: result.Selected, Forgotten: result.Forgotten}
	}
	if err != nil {
		return report, fmt.Errorf("removing backup snapshots: %w", err)
	}
	return report, nil
}

// Prune reclaims backup packs without opening a source vault. Kit traces all
// retained recovery points, removes unused packs, and rewrites packs with less
// than half their encoded payload still live. DryRun reports the same selection
// without modifying content. Both modes take the exclusive repository lock.
// Interrupted cleanup can be retried; mostly-live packs keep their unused bytes.
func (r *BackupRepository) Prune(ctx context.Context, opts BackupPruneOptions) (BackupPruneReport, error) {
	if r == nil || r.repo == nil {
		return BackupPruneReport{}, errors.New("docbank backup repository is required")
	}
	result, err := backup.Prune(ctx, r.repo, backupapp.New(version.Version), backup.PruneOptions{
		DryRun: opts.DryRun, ForceUnlock: opts.ForceUnlock,
		Progress: backupProgressCallback(opts.Progress),
	})
	var report BackupPruneReport
	if result != nil {
		report = BackupPruneReport{
			PacksToRemove: result.PacksToRemove, PacksToRepack: result.PacksToRepack,
			BytesToRemove: result.BytesToRemove, LiveBytesToRewrite: result.LiveBytesToRewrite,
			RemovedPacks: result.RemovedPacks, BytesRemoved: result.BytesRemoved, BytesWritten: result.BytesWritten,
		}
	}
	if err != nil {
		return report, fmt.Errorf("pruning backup repository: %w", err)
	}
	return report, nil
}
