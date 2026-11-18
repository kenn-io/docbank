package processing

import (
	"testing"

	"go.kenn.io/docbank/internal/store"
)

func TestPackageImportWorkerPostgres(t *testing.T) {
	t.Parallel()
	openCatalog := func(t *testing.T, _ string) *store.Store {
		t.Helper()
		return postgresProcessingStore(t)
	}
	t.Run("CommitsOneVerifiedRootRecord", func(t *testing.T) {
		t.Parallel()
		checkPackageImportWorkerCommitsOneVerifiedRootRecord(t, openCatalog)
	})
	t.Run("ResumesAfterCommittedRecord", func(t *testing.T) {
		t.Parallel()
		checkPackageImportWorkerResumesAfterCommittedRecord(t, openCatalog)
	})
	t.Run("RecoversStagedNodeWithoutRecordReceipt", func(t *testing.T) {
		t.Parallel()
		checkPackageImportWorkerRecoversStagedNodeWithoutRecordReceipt(t, openCatalog)
	})
	t.Run("AcceptsMissingPostPreflightFileAsPartial", func(t *testing.T) {
		t.Parallel()
		checkPackageImportWorkerAcceptsMissingPostPreflightFileAsPartial(t, openCatalog)
	})
	t.Run("CancellationRemovesOnlyUnreceiptedDocuments", func(t *testing.T) {
		t.Parallel()
		checkPackageImportCancellationRemovesOnlyUnreceiptedDocuments(t, openCatalog)
	})
	t.Run("LoadFileExportRoundTripsThroughIndependentFreshVault", func(t *testing.T) {
		t.Parallel()
		checkLoadFileExportRoundTripsThroughIndependentFreshVault(t, openCatalog)
	})
}
