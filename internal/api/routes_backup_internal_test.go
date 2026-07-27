package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"
)

func TestBackupRestoreReportGroupsFallbacksDeterministically(t *testing.T) {
	storage := &StorageStatus{
		LooseBlobs: 2, LooseBytes: 21, Packs: 1, PackStoredBytes: 55,
		PackedBlobs: 1, PackedRawBytes: 13, PackedStoredBytes: 11,
		DeadPackedBytes: 44,
	}
	report := backupRestoreReport("/restore", &backup.RestoreResult{
		SnapshotID: "snapshot", DBPath: "/restore/docbank.db", DBBytes: 12,
		AttachmentBlobs: 3, AttachmentBytes: 34, PackedAttachmentBlobs: 1,
		LooseAttachmentBlobs: 2, AttachmentPacks: 1, ExtrasFiles: 4,
		Duration: 1500 * time.Millisecond, DatabaseIntegrityChecked: true,
		PackFallbacks: []packstore.ImportFallback{
			{Reason: packstore.FallbackPackPublication},
			{Reason: packstore.FallbackBlobLimit},
			{Reason: packstore.FallbackPackPublication},
		},
	}, storage, "")
	assert.Equal(t, []BackupRestoreFallback{
		{Reason: "blob_limit", Count: 1},
		{Reason: "pack_publication", Count: 2},
	}, report.Fallbacks)
	assert.InDelta(t, 1.5, report.DurationSeconds, 0.001)
	assert.True(t, report.Proof.ContentVerified)
	assert.True(t, report.Proof.SQLiteIntegrity)
	assert.True(t, report.Proof.ManifestStats)
	assert.Equal(t, storage, report.Storage)
	assert.Empty(t, report.StorageWarning)
}
