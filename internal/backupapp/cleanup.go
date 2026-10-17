package backupapp

import "go.kenn.io/kit/backup"

// BackupForgetReport reports selected snapshots and completed removals.
// Forgotten can be partial when cleanup returns an error.
type BackupForgetReport struct {
	Selected  []string `json:"selected"`
	Forgotten []string `json:"forgotten"`
}

// BackupPruneReport reports planned and completed pack cleanup.
// Byte counts exclude indexes and temporary files; they are not net savings.
type BackupPruneReport struct {
	PacksToRemove      []string `json:"packs_to_remove"` // includes old packs selected for rewriting
	PacksToRepack      []string `json:"packs_to_repack"`
	BytesToRemove      int64    `json:"bytes_to_remove"`
	LiveBytesToRewrite uint64   `json:"live_bytes_to_rewrite"` // current encoded payload bytes to copy
	RemovedPacks       []string `json:"removed_packs"`
	BytesRemoved       int64    `json:"bytes_removed"`
	BytesWritten       int64    `json:"bytes_written"`
}

// ForgetReport converts Kit cleanup progress, including partial results on error.
func ForgetReport(result *backup.ForgetResult) BackupForgetReport {
	if result == nil {
		return BackupForgetReport{}
	}
	return BackupForgetReport{Selected: result.Selected, Forgotten: result.Forgotten}
}

// PruneReport converts Kit cleanup progress, including partial results on error.
func PruneReport(result *backup.PruneResult) BackupPruneReport {
	if result == nil {
		return BackupPruneReport{}
	}
	return BackupPruneReport{
		PacksToRemove: result.PacksToRemove, PacksToRepack: result.PacksToRepack,
		BytesToRemove: result.BytesToRemove, LiveBytesToRewrite: result.LiveBytesToRewrite,
		RemovedPacks: result.RemovedPacks, BytesRemoved: result.BytesRemoved, BytesWritten: result.BytesWritten,
	}
}

// HasWork reports whether cleanup selected, planned, or completed any work.
func (r BackupForgetReport) HasWork() bool {
	return len(r.Selected) > 0 || len(r.Forgotten) > 0
}

// HasWork reports whether cleanup selected, planned, or completed any work.
func (r BackupPruneReport) HasWork() bool {
	return len(r.PacksToRemove) > 0 || len(r.PacksToRepack) > 0 ||
		len(r.RemovedPacks) > 0 || r.BytesToRemove != 0 || r.LiveBytesToRewrite != 0 ||
		r.BytesRemoved != 0 || r.BytesWritten != 0
}
