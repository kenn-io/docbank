package store

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestRetainProductionPackageReconcilesResponseLossAtEachWrite(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	dir := t.TempDir()
	request := production.RecipientPackageRequest{
		JobID: job.ID, ProfileID: "export-dat-opt-images-v1",
		Limits:          production.PackageLimits{MaxVolumeBytes: 50 << 20, MaxVolumeDocuments: 10},
		ArchivePath:     filepath.Join(dir, "recipient.zip"),
		QCPath:          filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	_, err := production.PublishRecipientPackage(t.Context(), f.Store, f, request)
	require.NoError(t, err)
	// Put the first physical ZIP write before any successful package retention:
	// its retry must recover even when the bytes exist but catalog authority does not.
	stages := []string{"recipient.zip:write", "directory"}
	for _, name := range []string{"recipient.zip", "qc.json", "transmittal.json"} {
		for _, boundary := range []string{"write", "blob", "ingest"} {
			if name == "recipient.zip" && boundary == "write" {
				continue
			}
			stages = append(stages, name+":"+boundary)
		}
	}
	for index, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			operationID := fmt.Sprintf("77777777-7777-4777-8777-%012d", index+1)
			injected := false
			_, err := f.retainProductionPackage(t.Context(), job.ID, operationID,
				request.ProfileID, request.Limits, request.ArchivePath, request.QCPath,
				request.TransmittalPath, restartPackageBlobWriter(f), func(written string) error {
					if written == stage {
						injected = true
						return errLostProductionResponse
					}
					return nil
				})
			require.True(t, injected)
			require.ErrorIs(t, err, errLostProductionResponse)
			f.reopen(t)
			retained, err := f.RetainProductionPackage(t.Context(), job.ID, operationID,
				request.ProfileID, request.Limits, request.ArchivePath, request.QCPath,
				request.TransmittalPath, restartPackageBlobWriter(f))
			require.NoError(t, err)
			f.reopen(t)
			replayed, err := f.RetainProductionPackage(t.Context(), job.ID, operationID,
				request.ProfileID, request.Limits, request.ArchivePath, request.QCPath,
				request.TransmittalPath, restartPackageBlobWriter(f))
			require.NoError(t, err)
			for _, pair := range [][2]ContentWriteReceipt{
				{retained.Archive, replayed.Archive},
				{retained.QC, replayed.QC},
				{retained.Transmittal, replayed.Transmittal},
			} {
				require.Equal(t, pair[0].Version.ID, pair[1].Version.ID)
				var facts, bindings int
				require.NoError(t, f.db.QueryRowContext(t.Context(),
					`SELECT COUNT(*) FROM provenance WHERE node_id=?`, pair[1].Node.ID).Scan(&facts))
				require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM provenance_version_bindings b
					JOIN provenance p ON p.identity=b.provenance_identity WHERE p.node_id=?`,
					pair[1].Node.ID).Scan(&bindings))
				require.Equal(t, 1, facts)
				require.Equal(t, 1, bindings)
			}
		})
	}
}
