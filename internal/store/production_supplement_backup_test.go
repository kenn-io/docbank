package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionSupplementSurvivesPhysicalBackupRestore(t *testing.T) {
	driver := productionBackupDriver(t)
	f, parent := publishedRealRetentionFixture(t)
	materializeProductionEmailBlobs(t, f)
	child := supplementChildFixture(t, f, parent)
	request := production.SupplementRequest{OperationID: "78000000-0000-4000-8000-000000000046",
		ParentJobID: parent.ID, JobID: child.ID, ParentReceiptSHA256: parent.Receipt.SHA256,
		PreparedSHA256: child.RevisionSHA256, PreparedInputSHA256: child.PreparedInputSHA256}
	link, err := f.CreateProductionSupplement(t.Context(), "synthetic-operator", request)
	require.NoError(t, err)
	published := publishSupplementChild(t, f, child)
	allocation, err := f.ProductionNumberingForJob(t.Context(), child.ID)
	require.NoError(t, err)
	beforeNumber, err := f.FindPublishedProductionNumber(t.Context(), allocation.Labels[0].Label)
	require.NoError(t, err)
	require.Equal(t, published.ID, beforeNumber.JobID)
	beforeMetadata := productionBackupMetadata(t, f.Store)
	beforeBlobs := productionBackupBlobBytes(t, f)

	repository := filepath.Join(t.TempDir(), "supplement-backup")
	require.NoError(t, runProductionBackupDriver(t, driver, "create", f.root, repository))
	target := filepath.Join(t.TempDir(), "supplement-restore")
	require.NoError(t, runProductionBackupDriver(t, driver, "restore", repository, target))
	restored := requireProductionBackupRestored(t, target, beforeMetadata, beforeBlobs)
	restoredLink, err := restored.LoadProductionSupplement(t.Context(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, link, restoredLink)
	restoredAllocation, err := restored.ProductionNumberingForJob(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, allocation, restoredAllocation)
	restoredNumber, err := restored.FindPublishedProductionNumber(t.Context(), allocation.Labels[0].Label)
	require.NoError(t, err)
	require.Equal(t, beforeNumber, restoredNumber)
}
