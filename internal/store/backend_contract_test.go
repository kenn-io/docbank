package store

import (
	"flag"
	"fmt"
	"testing"
)

// This registry is the executable evidence inventory, not a production capability
// switch. Each scenario runs once on each selected backend. Backend-specific
// cases keep their own fixtures; a copied catalog never qualifies its producer.
type backendScenario struct {
	name     string
	sqlite   func(*testing.T)
	postgres func(*testing.T)
}
type backendSuite struct {
	name      string
	postgres  string
	scope     string
	scenarios []backendScenario
}

var backendSuites = []backendSuite{
	{name: "metadata-import", postgres: "native operations", scope: "Owner-only logical import into fresh PostgreSQL, SQLite round trips and derived-state recovery; excludes blob-byte verification, physical pack recovery and public opening.", scenarios: []backendScenario{
		{"MetadataImportCatalogs", nil, checkMetadataImportCatalogsPostgres},
		{"RetainedStringNativeWrites", nil, checkRetainedStringNativeWritesPostgres},
		{"MetadataImportRollback", nil, checkMetadataImportRollbackPostgres},
		{"MetadataImportAdmission", nil, checkMetadataImportAdmissionPostgres},
		{"MetadataImportKeepsFreshConsent", nil, checkMetadataImportKeepsFreshConsentPostgres},
	}},
	{name: "multi-store", postgres: "native operations", scope: "Secondary registry, inventory, placement, evacuation, storage jobs, cleanup and native repair/salvage; excludes deployment adapters, filesystem workers and metadata recovery.", scenarios: []backendScenario{
		{"StorageRepairRollback", nil, checkStorageRepairRollbackPostgres},
		{"MultiStorePhysicalRoundTrip", nil, checkMultiStorePhysicalRoundTripPostgres},
		{"MultiStoreReadSnapshots", nil, checkMultiStoreReadSnapshotsPostgres},
		{"PlacementPlanUsesRetainedSubtreeAndCompleteReferenceClosure", func(t *testing.T) {
			t.Helper()
			checkPlacementPlanUsesRetainedSubtreeAndCompleteReferenceClosure(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementPlanUsesRetainedSubtreeAndCompleteReferenceClosure(t, newPostgresQueryStore)
		}},
		{"PlacementCommitCannotExpandRetirementBeyondPreview", func(t *testing.T) {
			t.Helper()
			checkPlacementCommitCannotExpandRetirementBeyondPreview(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementCommitCannotExpandRetirementBeyondPreview(t, newPostgresQueryStore)
		}},
		{"PlacementCommitRejectsPlannedDestinationDrift", func(t *testing.T) { t.Helper(); checkPlacementCommitRejectsPlannedDestinationDrift(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPlacementCommitRejectsPlannedDestinationDrift(t, newPostgresQueryStore)
		}},
		{"PlacementPlanPinsAuditedContentToPrimaryByDefault", func(t *testing.T) {
			t.Helper()
			checkPlacementPlanPinsAuditedContentToPrimaryByDefault(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementPlanPinsAuditedContentToPrimaryByDefault(t, newPostgresQueryStore)
		}},
		{"PlacementPlanRejectsContentMissingFromBothRequestedStores", func(t *testing.T) {
			t.Helper()
			checkPlacementPlanRejectsContentMissingFromBothRequestedStores(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementPlanRejectsContentMissingFromBothRequestedStores(t, newPostgresQueryStore)
		}},
		{"EvacuationPlansOnlyAuthorityHeldBySource", func(t *testing.T) { t.Helper(); checkEvacuationPlansOnlyAuthorityHeldBySource(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEvacuationPlansOnlyAuthorityHeldBySource(t, newPostgresQueryStore)
		}},
		{"EvacuationPlansRetainedBlobsWithoutVersionReferences", func(t *testing.T) {
			t.Helper()
			checkEvacuationPlansRetainedBlobsWithoutVersionReferences(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEvacuationPlansRetainedBlobsWithoutVersionReferences(t, newPostgresQueryStore)
		}},
		{"S3PackedPlacementReportsContainerScratchAndEgress", func(t *testing.T) {
			t.Helper()
			checkS3PackedPlacementReportsContainerScratchAndEgress(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkS3PackedPlacementReportsContainerScratchAndEgress(t, newPostgresQueryStore)
		}},
		{"PlacementExistingLocalDestinationReportsVerificationRead", func(t *testing.T) {
			t.Helper()
			checkPlacementExistingLocalDestinationReportsVerificationRead(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementExistingLocalDestinationReportsVerificationRead(t, newPostgresQueryStore)
		}},
		{"PlacementExistingS3PackReportsVerificationEgressAndScratch", func(t *testing.T) {
			t.Helper()
			checkPlacementExistingS3PackReportsVerificationEgressAndScratch(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementExistingS3PackReportsVerificationEgressAndScratch(t, newPostgresQueryStore)
		}},
		{"PlacementExistingS3LooseReportsPhysicalVerificationEgress", func(t *testing.T) {
			t.Helper()
			checkPlacementExistingS3LooseReportsPhysicalVerificationEgress(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPlacementExistingS3LooseReportsPhysicalVerificationEgress(t, newPostgresQueryStore)
		}},
		{"SecondaryBlobStoreLifecycle", func(t *testing.T) { t.Helper(); checkSecondaryBlobStoreLifecycle(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkSecondaryBlobStoreLifecycle(t, newPostgresQueryStore) }},
		{"BlobStoreRemovalRejectsActiveDestinationOperation", func(t *testing.T) {
			t.Helper()
			checkBlobStoreRemovalRejectsActiveDestinationOperation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBlobStoreRemovalRejectsActiveDestinationOperation(t, newPostgresQueryStore)
		}},
		{"BlobStoreRemovalRequiresEmptyDetachedSecondary", func(t *testing.T) { t.Helper(); checkBlobStoreRemovalRequiresEmptyDetachedSecondary(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBlobStoreRemovalRequiresEmptyDetachedSecondary(t, newPostgresQueryStore)
		}},
		{"BlobStoreDetachRequiresCompletedPhysicalCleanup", func(t *testing.T) { t.Helper(); checkBlobStoreDetachRequiresCompletedPhysicalCleanup(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBlobStoreDetachRequiresCompletedPhysicalCleanup(t, newPostgresQueryStore)
		}},
		{"EvacuationCleanupIncludesPackAfterMappingsAreRevoked", func(t *testing.T) {
			t.Helper()
			checkEvacuationCleanupIncludesPackAfterMappingsAreRevoked(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEvacuationCleanupIncludesPackAfterMappingsAreRevoked(t, newPostgresQueryStore)
		}},
		{"BlobStoreInventoryReportsSoleAuthorityAndAffectedDocuments", func(t *testing.T) {
			t.Helper()
			checkBlobStoreInventoryReportsSoleAuthorityAndAffectedDocuments(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBlobStoreInventoryReportsSoleAuthorityAndAffectedDocuments(t, newPostgresQueryStore)
		}},
		{"BlobStoreUnreadableObjectsAccountsForUnavailableReplicaSet", func(t *testing.T) {
			t.Helper()
			checkBlobStoreUnreadableObjectsAccountsForUnavailableReplicaSet(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBlobStoreUnreadableObjectsAccountsForUnavailableReplicaSet(t, newPostgresQueryStore)
		}},
		{"BlobStoreRegistrationRejectsConflicts", func(t *testing.T) { t.Helper(); checkBlobStoreRegistrationRejectsConflicts(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBlobStoreRegistrationRejectsConflicts(t, newPostgresQueryStore) }},
		{"BlobStoreEvacuationRequiresVerifiedDestinationCoverage", func(t *testing.T) {
			t.Helper()
			checkBlobStoreEvacuationRequiresVerifiedDestinationCoverage(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBlobStoreEvacuationRequiresVerifiedDestinationCoverage(t, newPostgresQueryStore)
		}},
		{"BlobStoreEvacuationBeginRejectsActiveDestinationOperation", func(t *testing.T) {
			t.Helper()
			checkBlobStoreEvacuationBeginRejectsActiveDestinationOperation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBlobStoreEvacuationBeginRejectsActiveDestinationOperation(t, newPostgresQueryStore)
		}},
		{"BlobStoreEvacuationFinalizationRejectsActiveSourceOperation", func(t *testing.T) {
			t.Helper()
			checkBlobStoreEvacuationFinalizationRejectsActiveSourceOperation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBlobStoreEvacuationFinalizationRejectsActiveSourceOperation(t, newPostgresQueryStore)
		}},
		{"EmptyBlobStoreEvacuationFinalizationRejectsCancellation", func(t *testing.T) {
			t.Helper()
			checkEmptyBlobStoreEvacuationFinalizationRejectsCancellation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmptyBlobStoreEvacuationFinalizationRejectsCancellation(t, newPostgresQueryStore)
		}},
		{"DetachedBlobStoreEvacuationFinalizationRejectsCancellation", func(t *testing.T) {
			t.Helper()
			checkDetachedBlobStoreEvacuationFinalizationRejectsCancellation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDetachedBlobStoreEvacuationFinalizationRejectsCancellation(t, newPostgresQueryStore)
		}},
		{"StorageOperationPersistsProgressAndCancellation", func(t *testing.T) { t.Helper(); checkStorageOperationPersistsProgressAndCancellation(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkStorageOperationPersistsProgressAndCancellation(t, newPostgresQueryStore)
		}},
		{"StorageOperationRejectsConcurrentEvacuationsForOneStore", func(t *testing.T) {
			t.Helper()
			checkStorageOperationRejectsConcurrentEvacuationsForOneStore(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkStorageOperationRejectsConcurrentEvacuationsForOneStore(t, newPostgresQueryStore)
		}},
		{"StorageOperationRetryPreservesEvacuationFinalization", func(t *testing.T) {
			t.Helper()
			checkStorageOperationRetryPreservesEvacuationFinalization(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkStorageOperationRetryPreservesEvacuationFinalization(t, newPostgresQueryStore)
		}},
		{"StorageOperationRejectsCancellationAfterTerminalState", func(t *testing.T) {
			t.Helper()
			checkStorageOperationRejectsCancellationAfterTerminalState(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkStorageOperationRejectsCancellationAfterTerminalState(t, newPostgresQueryStore)
		}},
		{"StorageOperationCleanupPersistsUntilCompleted", func(t *testing.T) { t.Helper(); checkStorageOperationCleanupPersistsUntilCompleted(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkStorageOperationCleanupPersistsUntilCompleted(t, newPostgresQueryStore)
		}},
		{"PruneExpiredStorageOperationsPreservesPendingCleanup", func(t *testing.T) {
			t.Helper()
			checkPruneExpiredStorageOperationsPreservesPendingCleanup(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPruneExpiredStorageOperationsPreservesPendingCleanup(t, newPostgresQueryStore)
		}},
		{"StorageRecoveryRejectsDestinationAuthorityChangedDuringPublication", func(t *testing.T) {
			t.Helper()
			checkStorageRecoveryRejectsDestinationAuthorityChangedDuringPublication(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkStorageRecoveryRejectsDestinationAuthorityChangedDuringPublication(t, newPostgresQueryStore)
		}},
	}},
	{name: "metadata-export", postgres: "native operations", scope: "Pinned logical metadata and backup-scoped export, audit verification and recovery into fresh SQLite; excludes PostgreSQL import, physical recovery and public opening.", scenarios: []backendScenario{
		{"MetadataExportRecovery", func(t *testing.T) { t.Helper(); checkMetadataExportRecovery(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMetadataExportRecovery(t, newPostgresQueryStore) }},
		{"MetadataBackupAuthority", func(t *testing.T) { t.Helper(); checkMetadataBackupAuthority(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMetadataBackupAuthority(t, newPostgresQueryStore) }},
		{"MetadataExportFailures", func(t *testing.T) { t.Helper(); checkMetadataExportFailures(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMetadataExportFailures(t, newPostgresQueryStore) }},
		{"MetadataSnapshotPinned", nil, checkMetadataSnapshotPinnedPostgres},
		{"MetadataRetainedCatalogs", nil, checkMetadataRetainedCatalogsPostgres},
		{"MetadataProcessingJobs", func(t *testing.T) { t.Helper(); checkMetadataProcessingJobs(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMetadataProcessingJobs(t, newPostgresQueryStore) }},
	}},
	{name: "received-packages", postgres: "native operations", scope: "Received-package admission, leased staging, immutable records and labels, completion, cancellation and retained browsing; excludes filesystem workers and portable recovery.", scenarios: []backendScenario{
		{"ReceivedPackageStagingAndRetention", func(t *testing.T) { t.Helper(); checkReceivedPackageStagingAndRetention(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkReceivedPackageStagingAndRetention(t, newPostgresQueryStore) }},
		{"ReceivedPackageAdmissionRollback", nil, checkReceivedPackageAdmissionRollbackPostgres},
		{"ReceivedPackagePublicationRollback", nil, checkReceivedPackagePublicationRollbackPostgres},
		{"ReceivedPackageReadSnapshots", nil, checkReceivedPackageReadSnapshotsPostgres},

		{"CommitPackageRecordWithLeaseReplaysExactPayload", func(t *testing.T) { t.Helper(); checkCommitPackageRecordWithLeaseReplaysExactPayload(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkCommitPackageRecordWithLeaseReplaysExactPayload(t, newPostgresQueryStore)
		}},
		{"PackageImportJobAdmissionReplaysExactOperation", func(t *testing.T) { t.Helper(); checkPackageImportJobAdmissionReplaysExactOperation(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportJobAdmissionReplaysExactOperation(t, newPostgresQueryStore)
		}},
		{"PackageImportJobAdmissionRejectsSecondOperationForPackage", func(t *testing.T) {
			t.Helper()
			checkPackageImportJobAdmissionRejectsSecondOperationForPackage(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPackageImportJobAdmissionRejectsSecondOperationForPackage(t, newPostgresQueryStore)
		}},
		{"AdmitPackageImportPublishesIngestPackageAndJobAtomically", func(t *testing.T) {
			t.Helper()
			checkAdmitPackageImportPublishesIngestPackageAndJobAtomically(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkAdmitPackageImportPublishesIngestPackageAndJobAtomically(t, newPostgresQueryStore)
		}},
		{"PackageImportJobCancelFencesStaleFinalization", func(t *testing.T) { t.Helper(); checkPackageImportJobCancelFencesStaleFinalization(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportJobCancelFencesStaleFinalization(t, newPostgresQueryStore)
		}},
		{"PackageImportLeaseReclaimInvalidatesEarlierToken", func(t *testing.T) { t.Helper(); checkPackageImportLeaseReclaimInvalidatesEarlierToken(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportLeaseReclaimInvalidatesEarlierToken(t, newPostgresQueryStore)
		}},
		{"PackageImportGapReceiptIsFencedAndIdempotent", func(t *testing.T) { t.Helper(); checkPackageImportGapReceiptIsFencedAndIdempotent(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportGapReceiptIsFencedAndIdempotent(t, newPostgresQueryStore)
		}},
		{"PackageImportProgressIncludesGapsOnCommittedRecords", func(t *testing.T) {
			t.Helper()
			checkPackageImportProgressIncludesGapsOnCommittedRecords(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPackageImportProgressIncludesGapsOnCommittedRecords(t, newPostgresQueryStore)
		}},
		{"PackageImportProgressNamesRejectedRecordWithEmptyGapArray", func(t *testing.T) {
			t.Helper()
			checkPackageImportProgressNamesRejectedRecordWithEmptyGapArray(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPackageImportProgressNamesRejectedRecordWithEmptyGapArray(t, newPostgresQueryStore)
		}},
		{"PackageImportLeaseMayBeReleasedForImmediateRetry", func(t *testing.T) { t.Helper(); checkPackageImportLeaseMayBeReleasedForImmediateRetry(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportLeaseMayBeReleasedForImmediateRetry(t, newPostgresQueryStore)
		}},
		{"StalePackageWorkerCannotCommitARecordAfterCancel", func(t *testing.T) { t.Helper(); checkStalePackageWorkerCannotCommitARecordAfterCancel(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkStalePackageWorkerCannotCommitARecordAfterCancel(t, newPostgresQueryStore)
		}},
		{"PackageImportFinishRejectsUnrelatedSnapshot", func(t *testing.T) { t.Helper(); checkPackageImportFinishRejectsUnrelatedSnapshot(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportFinishRejectsUnrelatedSnapshot(t, newPostgresQueryStore)
		}},
		{"PackageImportFinishPublishesOnlyMatchingReceiptSnapshot", func(t *testing.T) {
			t.Helper()
			checkPackageImportFinishPublishesOnlyMatchingReceiptSnapshot(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPackageImportFinishPublishesOnlyMatchingReceiptSnapshot(t, newPostgresQueryStore)
		}},
		{"RevokePackageImportOwnerCancelsQueuedWork", func(t *testing.T) { t.Helper(); checkRevokePackageImportOwnerCancelsQueuedWork(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRevokePackageImportOwnerCancelsQueuedWork(t, newPostgresQueryStore)
		}},
		{"PackageImportDiscardRepairsOrRetainsPhotoAssets", func(t *testing.T) { t.Helper(); checkPackageImportDiscardRepairsOrRetainsPhotoAssets(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageImportDiscardRepairsOrRetainsPhotoAssets(t, newPostgresQueryStore)
		}},
		{"ReceivedLabelLookupRequiresScopeWhenSenderReusesIt", func(t *testing.T) {
			t.Helper()
			checkReceivedLabelLookupRequiresScopeWhenSenderReusesIt(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkReceivedLabelLookupRequiresScopeWhenSenderReusesIt(t, newPostgresQueryStore)
		}},
		{"ReceivedLabelLookupTreatsDocumentAndPageEndpointsAsOneOccurrence", func(t *testing.T) {
			t.Helper()
			checkReceivedLabelLookupTreatsDocumentAndPageEndpointsAsOneOccurrence(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkReceivedLabelLookupTreatsDocumentAndPageEndpointsAsOneOccurrence(t, newPostgresQueryStore)
		}},
		{"PackageRecordRejectsVersionOutsideItsIngest", func(t *testing.T) { t.Helper(); checkPackageRecordRejectsVersionOutsideItsIngest(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageRecordRejectsVersionOutsideItsIngest(t, newPostgresQueryStore)
		}},
		{"PackageTimelineInputsPreserveRawDateAndDeclaredZone", func(t *testing.T) {
			t.Helper()
			checkPackageTimelineInputsPreserveRawDateAndDeclaredZone(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPackageTimelineInputsPreserveRawDateAndDeclaredZone(t, newPostgresQueryStore)
		}},
		{"PackageLabelCandidatesPreserveByteKeys", func(t *testing.T) { t.Helper(); checkPackageLabelCandidatesPreserveByteKeys(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPackageLabelCandidatesPreserveByteKeys(t, newPostgresQueryStore) }},
		{"PackageLabelCandidatesPageAmbiguousSenderLabels", func(t *testing.T) { t.Helper(); checkPackageLabelCandidatesPageAmbiguousSenderLabels(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackageLabelCandidatesPageAmbiguousSenderLabels(t, newPostgresQueryStore)
		}},
	}},
	{name: "exports", postgres: "native operations", scope: "Explicit/node/chunk and completed-mailbox sources, pinned output plans, previews, export-job lifecycle and retained-output inspection; excludes query source selection, filesystem execution and recovery.", scenarios: []backendScenario{
		{"ExportWorkerFailureAndCapacity", func(t *testing.T) { t.Helper(); checkExportWorkerFailureAndCapacity(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkExportWorkerFailureAndCapacity(t, newPostgresQueryStore) }},
		{"ExportDiscoverySnapshots", nil, checkExportDiscoverySnapshotsPostgres},

		{"ExportEmailInspection", func(t *testing.T) { t.Helper(); checkExportEmailInspection(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkExportEmailInspection(t, newPostgresQueryStore) }},
		{"ExportFinishRollback", nil, checkExportFinishRollbackPostgres},
		{"ExplicitPhotoExport", nil, checkExplicitPhotoExportPostgres},

		{"ExportJobPublicationAndRelease", func(t *testing.T) { t.Helper(); checkExportJobPublicationAndRelease(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkExportJobPublicationAndRelease(t, newPostgresQueryStore) }},
		{"ExportRestartRevocationAndCleanup", func(t *testing.T) { t.Helper(); checkExportRestartRevocationAndCleanup(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkExportRestartRevocationAndCleanup(t, newPostgresQueryStore) }},
		{"ExportFailedResolutionKeepsReservation", func(t *testing.T) { t.Helper(); checkExportFailedResolutionKeepsReservation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkExportFailedResolutionKeepsReservation(t, newPostgresQueryStore) }},
		{"ExportPlanRollback", nil, checkExportPlanRollbackPostgres},
		{"ExportReadSnapshots", nil, checkExportReadSnapshotsPostgres},
		{"ExportChunksSealExactVersionsAndRejectChangedRetries", func(t *testing.T) {
			t.Helper()
			checkExportChunksSealExactVersionsAndRejectChangedRetries(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkExportChunksSealExactVersionsAndRejectChangedRetries(t, newPostgresQueryStore)
		}},
		{"ExportPlanFailureRollsBackAdmissionAndAllowsRetry", func(t *testing.T) {
			t.Helper()
			checkExportPlanFailureRollsBackAdmissionAndAllowsRetry(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkExportPlanFailureRollsBackAdmissionAndAllowsRetry(t, newPostgresQueryStore)
		}},
		{"ExportPlanOptionalPagesRespectMetadataBounds", func(t *testing.T) { t.Helper(); checkExportPlanOptionalPagesRespectMetadataBounds(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkExportPlanOptionalPagesRespectMetadataBounds(t, newPostgresQueryStore)
		}},
		{"ExportPlanPinsRolesAndCannotInventMissingText", func(t *testing.T) { t.Helper(); checkExportPlanPinsRolesAndCannotInventMissingText(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkExportPlanPinsRolesAndCannotInventMissingText(t, newPostgresQueryStore)
		}},
		{"ExportPreviewUsesFrozenReceiptsAndOwner", func(t *testing.T) { t.Helper(); checkExportPreviewUsesFrozenReceiptsAndOwner(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkExportPreviewUsesFrozenReceiptsAndOwner(t, newPostgresQueryStore) }},
		{"ExportCompletedMailboxCollectionPinsImportedVersions", func(t *testing.T) {
			t.Helper()
			checkExportCompletedMailboxCollectionPinsImportedVersions(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkExportCompletedMailboxCollectionPinsImportedVersions(t, newPostgresQueryStore)
		}},
		{"ExportPublicationReleasedOnlyAfterRetentionCleanup", func(t *testing.T) {
			t.Helper()
			checkExportPublicationReleasedOnlyAfterRetentionCleanup(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkExportPublicationReleasedOnlyAfterRetentionCleanup(t, newPostgresQueryStore)
		}},
		{"ExportSourceRetryFreezesHistoricalMembershipAndProtectsPrune", func(t *testing.T) {
			t.Helper()
			checkExportSourceRetryFreezesHistoricalMembershipAndProtectsPrune(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkExportSourceRetryFreezesHistoricalMembershipAndProtectsPrune(t, newPostgresQueryStore)
		}},
		{"ExportUpload1001MembersRequiresEveryChunkAndExactDigest", func(t *testing.T) {
			t.Helper()
			checkExportUpload1001MembersRequiresEveryChunkAndExactDigest(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkExportUpload1001MembersRequiresEveryChunkAndExactDigest(t, newPostgresQueryStore)
		}}}},
	{name: "sealed-delivery", postgres: "native operations", scope: "Inline and streaming sealed snapshots, package identity and preflights, Bates reservations and verified artifacts with history and custodian evidence; excludes query selection, received-package imports and filesystem delivery.", scenarios: []backendScenario{
		{"SealedDeliveryReservationRollback", nil, checkSealedDeliveryReservationRollbackPostgres},
		{"SealedDeliveryArtifactRollback", nil, checkSealedDeliveryArtifactRollbackPostgres},
		{"SealedDeliveryConcurrentReservations", nil, checkSealedDeliveryConcurrentReservationsPostgres},
		{"BatesEvidenceSnapshots", nil, checkBatesEvidenceSnapshotsPostgres},
		{"BatesNamespaceSnapshot", nil, checkBatesNamespaceSnapshotPostgres},
		{"BatesCollectionEvidenceFollowsFrozenSources", func(t *testing.T) { t.Helper(); checkBatesCollectionEvidenceFollowsFrozenSources(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBatesCollectionEvidenceFollowsFrozenSources(t, newPostgresQueryStore)
		}},
		{"BatesNamespaceDetailCursor", func(t *testing.T) { t.Helper(); checkBatesNamespaceDetailCursor(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBatesNamespaceDetailCursor(t, newPostgresQueryStore) }},
		{"BatesSnapshotReservationRechecksLiveSource", func(t *testing.T) { t.Helper(); checkBatesSnapshotReservationRechecksLiveSource(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBatesSnapshotReservationRechecksLiveSource(t, newPostgresQueryStore)
		}},
		{"PackagePreflightOwnerBlockingAndExpiry", func(t *testing.T) { t.Helper(); checkPackagePreflightOwnerBlockingAndExpiry(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPackagePreflightOwnerBlockingAndExpiry(t, newPostgresQueryStore) }},
		{"SealCollectionSnapshotFreezesExactVersion", func(t *testing.T) { t.Helper(); checkSealCollectionSnapshotFreezesExactVersion(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotFreezesExactVersion(t, newPostgresQueryStore)
		}},
		{"SnapshotSourceCollectionsFreezeMembership", func(t *testing.T) { t.Helper(); checkSnapshotSourceCollectionsFreezeMembership(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkSnapshotSourceCollectionsFreezeMembership(t, newPostgresQueryStore)
		}},
		{"SnapshotSelectedPagesUseVerifiedFullPDF", func(t *testing.T) { t.Helper(); checkSnapshotSelectedPagesUseVerifiedFullPDF(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkSnapshotSelectedPagesUseVerifiedFullPDF(t, newPostgresQueryStore) }},
		{"SnapshotSourceBatchChecksOverlappingMembershipInOneQuery", func(t *testing.T) {
			t.Helper()
			checkSnapshotSourceBatchChecksOverlappingMembershipInOneQuery(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSnapshotSourceBatchChecksOverlappingMembershipInOneQuery(t, newPostgresQueryStore)
		}},
		{"SealCollectionSnapshotStreamAcceptsLargeManifestAndMatchesCanonicalDigest", func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotStreamAcceptsLargeManifestAndMatchesCanonicalDigest(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotStreamAcceptsLargeManifestAndMatchesCanonicalDigest(t, newPostgresQueryStore)
		}},
		{"SealCollectionSnapshotStreamRollsBackAndReplaysExactly", func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotStreamRollsBackAndReplaysExactly(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotStreamRollsBackAndReplaysExactly(t, newPostgresQueryStore)
		}},
		{"SealCollectionSnapshotStreamValidatesFamilyAcrossBatches", func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotStreamValidatesFamilyAcrossBatches(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSealCollectionSnapshotStreamValidatesFamilyAcrossBatches(t, newPostgresQueryStore)
		}},
		{"PackageIdentityIsIdempotentAndImmutable", func(t *testing.T) { t.Helper(); checkPackageIdentityIsIdempotentAndImmutable(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPackageIdentityIsIdempotentAndImmutable(t, newPostgresQueryStore) }},
		{"PackagesListsStablePagesAndFiltersDirection", func(t *testing.T) { t.Helper(); checkPackagesListsStablePagesAndFiltersDirection(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPackagesListsStablePagesAndFiltersDirection(t, newPostgresQueryStore)
		}},
		{"ExpiredPreflightsAreSweptAndUnexpiredOnesSurvive", func(t *testing.T) { t.Helper(); checkExpiredPreflightsAreSweptAndUnexpiredOnesSurvive(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkExpiredPreflightsAreSweptAndUnexpiredOnesSurvive(t, newPostgresQueryStore)
		}},
		{"BatesPreviewIsTentativeAndRepeatable", func(t *testing.T) { t.Helper(); checkBatesPreviewIsTentativeAndRepeatable(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBatesPreviewIsTentativeAndRepeatable(t, newPostgresQueryStore) }},
		{"ABFixtureAllocatesFortyOneToFortyThree", func(t *testing.T) { t.Helper(); checkABFixtureAllocatesFortyOneToFortyThree(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkABFixtureAllocatesFortyOneToFortyThree(t, newPostgresQueryStore) }},
		{"NamespaceUniquenessOverflowAndUnverifiedPages", func(t *testing.T) { t.Helper(); checkNamespaceUniquenessOverflowAndUnverifiedPages(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkNamespaceUniquenessOverflowAndUnverifiedPages(t, newPostgresQueryStore)
		}},
		{"BatesGlobalLabelCollisionRollsBackCursor", func(t *testing.T) { t.Helper(); checkBatesGlobalLabelCollisionRollsBackCursor(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBatesGlobalLabelCollisionRollsBackCursor(t, newPostgresQueryStore)
		}},
		{"BatesArtifactReceiptsMustNameTheSealedSourcePDF", func(t *testing.T) { t.Helper(); checkBatesArtifactReceiptsMustNameTheSealedSourcePDF(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBatesArtifactReceiptsMustNameTheSealedSourcePDF(t, newPostgresQueryStore)
		}},
		{"BatesExplicitPagesMatchASealedPDFWithoutPageDocument", func(t *testing.T) {
			t.Helper()
			checkBatesExplicitPagesMatchASealedPDFWithoutPageDocument(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBatesExplicitPagesMatchASealedPDFWithoutPageDocument(t, newPostgresQueryStore)
		}},
		{"BatesRefusesToReserveASourceTooLargeToStamp", func(t *testing.T) { t.Helper(); checkBatesRefusesToReserveASourceTooLargeToStamp(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkBatesRefusesToReserveASourceTooLargeToStamp(t, newPostgresQueryStore)
		}},
		{"BatesRefusesAnEmptySource", func(t *testing.T) { t.Helper(); checkBatesRefusesAnEmptySource(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBatesRefusesAnEmptySource(t, newPostgresQueryStore) }},
		{"FindBatesArtifactsReturnsCandidatesForExactlyOneSelector", func(t *testing.T) {
			t.Helper()
			checkFindBatesArtifactsReturnsCandidatesForExactlyOneSelector(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkFindBatesArtifactsReturnsCandidatesForExactlyOneSelector(t, newPostgresQueryStore)
		}},
		{"FindBatesArtifactEvidenceContinuesWithoutChangingCandidate", func(t *testing.T) {
			t.Helper()
			checkFindBatesArtifactEvidenceContinuesWithoutChangingCandidate(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkFindBatesArtifactEvidenceContinuesWithoutChangingCandidate(t, newPostgresQueryStore)
		}},
		{"PackageMembersReadSealedOccurrenceThroughPackageID", func(t *testing.T) {
			t.Helper()
			checkPackageMembersReadSealedOccurrenceThroughPackageID(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPackageMembersReadSealedOccurrenceThroughPackageID(t, newPostgresQueryStore)
		}},
	}},

	{name: "document-people", postgres: "native operations", scope: "Document-person inputs, immutable attribution publication, freshness, failure coverage and rebuilds; excludes general People discovery and public vault opening.", scenarios: []backendScenario{
		{"AuthorityInputs", func(t *testing.T) { t.Helper(); checkDocumentPeopleAuthorityInputs(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkDocumentPeopleAuthorityInputs(t, newPostgresQueryStore) }},
		{"EvidenceIDs", func(t *testing.T) { t.Helper(); checkDocumentPeopleEvidenceIDs(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkDocumentPeopleEvidenceIDs(t, newPostgresQueryStore) }},
		{"Snapshots", nil, checkDocumentPeopleSnapshotsPostgres},
		{"Rollback", nil, checkDocumentPeopleRollbackPostgres},
		{"DocumentPeopleResolverInputsProvisionEligibleIdentityOnce", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleResolverInputsProvisionEligibleIdentityOnce(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleResolverInputsProvisionEligibleIdentityOnce(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleBoundsDerivedLabelsWithoutChangingEvidence", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleBoundsDerivedLabelsWithoutChangingEvidence(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleBoundsDerivedLabelsWithoutChangingEvidence(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleProvisioningKeepsPublishedVersionsCurrent", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleProvisioningKeepsPublishedVersionsCurrent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleProvisioningKeepsPublishedVersionsCurrent(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleNameAliasIsOnlyASuggestion", func(t *testing.T) { t.Helper(); checkDocumentPeopleNameAliasIsOnlyASuggestion(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleNameAliasIsOnlyASuggestion(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleReusedIdentitySuggestsOnlyActiveOwner", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleReusedIdentitySuggestsOnlyActiveOwner(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleReusedIdentitySuggestsOnlyActiveOwner(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleRetiredNameAliasKeepsUnassignedCandidate", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleRetiredNameAliasKeepsUnassignedCandidate(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleRetiredNameAliasKeepsUnassignedCandidate(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleDisplayOnlyActorRemainsUnresolved", func(t *testing.T) { t.Helper(); checkDocumentPeopleDisplayOnlyActorRemainsUnresolved(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleDisplayOnlyActorRemainsUnresolved(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleLoadFailureHasNoPublishableInputs", func(t *testing.T) { t.Helper(); checkDocumentPeopleLoadFailureHasNoPublishableInputs(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleLoadFailureHasNoPublishableInputs(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleDerivationDoesNotMutateAuditedAuthority", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleDerivationDoesNotMutateAuditedAuthority(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleDerivationDoesNotMutateAuditedAuthority(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleResolverInputsOpensNameOnlyCandidate", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleResolverInputsOpensNameOnlyCandidate(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleResolverInputsOpensNameOnlyCandidate(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleResolverInputsHonorsExternalUIDUnlink", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleResolverInputsHonorsExternalUIDUnlink(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleResolverInputsHonorsExternalUIDUnlink(t, newPostgresQueryStore)
		}},
		{"PublishDocumentPeopleRejectsChangedInputsBeforeWriting", func(t *testing.T) {
			t.Helper()
			checkPublishDocumentPeopleRejectsChangedInputsBeforeWriting(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishDocumentPeopleRejectsChangedInputsBeforeWriting(t, newPostgresQueryStore)
		}},
		{"PublishDocumentPeopleReplayInputIdentity", func(t *testing.T) { t.Helper(); checkPublishDocumentPeopleReplayInputIdentity(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPublishDocumentPeopleReplayInputIdentity(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleFailureRemainsRetryableAndCannotEraseSuccess", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFailureRemainsRetryableAndCannotEraseSuccess(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFailureRemainsRetryableAndCannotEraseSuccess(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleFailureReplacesOlderSuccessfulCoverage", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFailureReplacesOlderSuccessfulCoverage(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFailureReplacesOlderSuccessfulCoverage(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleReadsWithholdStaleAttributionAndRetainHistory", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleReadsWithholdStaleAttributionAndRetainHistory(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleReadsWithholdStaleAttributionAndRetainHistory(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleMembershipChangesInvalidateOnlyAffectedDocument", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleMembershipChangesInvalidateOnlyAffectedDocument(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleMembershipChangesInvalidateOnlyAffectedDocument(t, newPostgresQueryStore)
		}},
		{"PeopleRebuildReplay", func(t *testing.T) { t.Helper(); checkPeopleRebuildReplay(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeopleRebuildReplay(t, newPostgresQueryStore) }},
		{"DocumentPeopleCoverageCountsCurrentVersionWaitingForEvents", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleCoverageCountsCurrentVersionWaitingForEvents(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleCoverageCountsCurrentVersionWaitingForEvents(t, newPostgresQueryStore)
		}},
		{"PeopleRebuildWaitsForFailureRetryAndLateNodeRevision", func(t *testing.T) {
			t.Helper()
			checkPeopleRebuildWaitsForFailureRetryAndLateNodeRevision(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPeopleRebuildWaitsForFailureRetryAndLateNodeRevision(t, newPostgresQueryStore)
		}},
		{"PeopleRebuildCountsFailureAfterNodeRevisionChanges", func(t *testing.T) {
			t.Helper()
			checkPeopleRebuildCountsFailureAfterNodeRevisionChanges(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPeopleRebuildCountsFailureAfterNodeRevisionChanges(t, newPostgresQueryStore)
		}},
		{"PeopleRebuildStopsRetryingTerminalFailure", func(t *testing.T) { t.Helper(); checkPeopleRebuildStopsRetryingTerminalFailure(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPeopleRebuildStopsRetryingTerminalFailure(t, newPostgresQueryStore)
		}},
	}},
	{name: "people-browsing", postgres: "native operations", scope: "Name and recent pages, Unicode prefixes, evidence visibility, identity labels and photo summaries.", scenarios: []backendScenario{
		{"UnicodePrefixes", func(t *testing.T) { t.Helper(); checkPeopleBrowseUnicode(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeopleBrowseUnicode(t, newPostgresQueryStore) }},
		{"Snapshot", nil, checkPeopleBrowseSnapshotPostgres},
		{"NameIndex", nil, checkPeopleBrowseNameIndexPostgres},
		{"PeopleRecentFirstFollowsVisibleEvidence", func(t *testing.T) { t.Helper(); checkPeopleRecentFirstFollowsVisibleEvidence(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeopleRecentFirstFollowsVisibleEvidence(t, newPostgresQueryStore) }},
		{"PeopleRecentIgnoresUnavailablePhotoEvidence", func(t *testing.T) { t.Helper(); checkPeopleRecentIgnoresUnavailablePhotoEvidence(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPeopleRecentIgnoresUnavailablePhotoEvidence(t, newPostgresQueryStore)
		}},
		{"PeopleRecentFirstAfterNaming", func(t *testing.T) { t.Helper(); checkPeopleRecentFirstAfterNaming(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeopleRecentFirstAfterNaming(t, newPostgresQueryStore) }},
		{"PeopleRecentPagingSkipsGroupsThatRiseAheadOfCursor", func(t *testing.T) {
			t.Helper()
			checkPeopleRecentPagingSkipsGroupsThatRiseAheadOfCursor(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPeopleRecentPagingSkipsGroupsThatRiseAheadOfCursor(t, newPostgresQueryStore)
		}},
		{"PeopleByDisplayNameEvidenceVisibility", func(t *testing.T) { t.Helper(); checkPeopleByDisplayNameEvidenceVisibility(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeopleByDisplayNameEvidenceVisibility(t, newPostgresQueryStore) }},
		{"PeopleByDisplayNameOperatorCandidateAcceptance", func(t *testing.T) { t.Helper(); checkPeopleByDisplayNameOperatorCandidateAcceptance(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPeopleByDisplayNameOperatorCandidateAcceptance(t, newPostgresQueryStore)
		}},
		{"PeopleByDisplayNameMatchesWordPrefixes", func(t *testing.T) { t.Helper(); checkPeopleByDisplayNameMatchesWordPrefixes(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeopleByDisplayNameMatchesWordPrefixes(t, newPostgresQueryStore) }},
		{"PeopleSearchIdentityLabelsRespectEvidenceVisibility", func(t *testing.T) {
			t.Helper()
			checkPeopleSearchIdentityLabelsRespectEvidenceVisibility(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPeopleSearchIdentityLabelsRespectEvidenceVisibility(t, newPostgresQueryStore)
		}},
		{"PeoplePhotoSummary", func(t *testing.T) { t.Helper(); checkPeoplePhotoSummary(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeoplePhotoSummary(t, newPostgresQueryStore) }},
		{"PeoplePhotoSummaryEligibilityAcrossVisibilityAndOrder", func(t *testing.T) {
			t.Helper()
			checkPeoplePhotoSummaryEligibilityAcrossVisibilityAndOrder(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPeoplePhotoSummaryEligibilityAcrossVisibilityAndOrder(t, newPostgresQueryStore)
		}},
		{"PeoplePhotoSummaryPairedSources", func(t *testing.T) { t.Helper(); checkPeoplePhotoSummaryPairedSources(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeoplePhotoSummaryPairedSources(t, newPostgresQueryStore) }},
		{"PeoplePhotoSummarySharedPhotos", func(t *testing.T) { t.Helper(); checkPeoplePhotoSummarySharedPhotos(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPeoplePhotoSummarySharedPhotos(t, newPostgresQueryStore) }},
		{"PersonNameSearchFollowsAuthority", func(t *testing.T) { t.Helper(); checkPersonNameSearchFollowsAuthority(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonNameSearchFollowsAuthority(t, newPostgresQueryStore) }},
	}},
	{name: "people-curation", postgres: "native operations", scope: "People, identities, custodians, candidates, face decisions, merge/split and undo; people-browsing covers discovery; document-people covers resolver production.", scenarios: []backendScenario{
		{"FaceCandidateCurrentScope", func(t *testing.T) { t.Helper(); checkFaceCandidateCurrentScope(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkFaceCandidateCurrentScope(t, newPostgresQueryStore) }},
		{"Snapshots", nil, checkPeopleSnapshotsPostgres},
		{"Rollback", nil, checkPeopleRollbackPostgres},
		{"RetainedPackageCustody", nil, checkPeopleRetainedPackageCustodyPostgres},
		{"PersonRevisionFence", func(t *testing.T) { t.Helper(); checkPersonRevisionFence(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonRevisionFence(t, newPostgresQueryStore) }},
		{"PersonIdentityAuthorityAndSharedContactPoints", func(t *testing.T) { t.Helper(); checkPersonIdentityAuthorityAndSharedContactPoints(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPersonIdentityAuthorityAndSharedContactPoints(t, newPostgresQueryStore)
		}},
		{"PersonMutationsAdvanceBindingEpoch", func(t *testing.T) { t.Helper(); checkPersonMutationsAdvanceBindingEpoch(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonMutationsAdvanceBindingEpoch(t, newPostgresQueryStore) }},
		{"PersonDetailResolvesAlias", func(t *testing.T) { t.Helper(); checkPersonDetailResolvesAlias(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonDetailResolvesAlias(t, newPostgresQueryStore) }},
		{"RetirePersonTombstonesResolution", func(t *testing.T) { t.Helper(); checkRetirePersonTombstonesResolution(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkRetirePersonTombstonesResolution(t, newPostgresQueryStore) }},
		{"RetirePersonCutsOffInboundMergeAliases", func(t *testing.T) { t.Helper(); checkRetirePersonCutsOffInboundMergeAliases(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkRetirePersonCutsOffInboundMergeAliases(t, newPostgresQueryStore) }},
		{"RecordExternalUIDAliasesPreservesExistingHopBound", func(t *testing.T) {
			t.Helper()
			checkRecordExternalUIDAliasesPreservesExistingHopBound(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRecordExternalUIDAliasesPreservesExistingHopBound(t, newPostgresQueryStore)
		}},
		{"PersonExternalIdentityLimitRejectsNewAndAllowsExistingUpdate", func(t *testing.T) {
			t.Helper()
			checkPersonExternalIdentityLimitRejectsNewAndAllowsExistingUpdate(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPersonExternalIdentityLimitRejectsNewAndAllowsExistingUpdate(t, newPostgresQueryStore)
		}},
		{"PersonExternalIdentityForwardsAndHonorsUnlinkedTombstone", func(t *testing.T) {
			t.Helper()
			checkPersonExternalIdentityForwardsAndHonorsUnlinkedTombstone(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPersonExternalIdentityForwardsAndHonorsUnlinkedTombstone(t, newPostgresQueryStore)
		}},
		{"RecordExternalUIDAliasesRetiresCurrentIdentity", func(t *testing.T) { t.Helper(); checkRecordExternalUIDAliasesRetiresCurrentIdentity(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRecordExternalUIDAliasesRetiresCurrentIdentity(t, newPostgresQueryStore)
		}},
		{"LinkExternalIdentityRejectsCurrentAlias", func(t *testing.T) { t.Helper(); checkLinkExternalIdentityRejectsCurrentAlias(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkLinkExternalIdentityRejectsCurrentAlias(t, newPostgresQueryStore) }},
		{"PersonExternalCurrentIdentityRequiresAliasTransition", func(t *testing.T) {
			t.Helper()
			checkPersonExternalCurrentIdentityRequiresAliasTransition(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPersonExternalCurrentIdentityRequiresAliasTransition(t, newPostgresQueryStore)
		}},
		{"PersonExternalIdentityCannotBeLinkedToAnotherPerson", func(t *testing.T) {
			t.Helper()
			checkPersonExternalIdentityCannotBeLinkedToAnotherPerson(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPersonExternalIdentityCannotBeLinkedToAnotherPerson(t, newPostgresQueryStore)
		}},
		{"RecordExternalUIDAliasesRetargetsAfterSplit", func(t *testing.T) { t.Helper(); checkRecordExternalUIDAliasesRetargetsAfterSplit(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRecordExternalUIDAliasesRetargetsAfterSplit(t, newPostgresQueryStore)
		}},
		{"PersonActorKeysPaginationValidatesCursor", func(t *testing.T) { t.Helper(); checkPersonActorKeysPaginationValidatesCursor(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPersonActorKeysPaginationValidatesCursor(t, newPostgresQueryStore)
		}},
		{"RecordExternalUIDAliasesRejectsCycleAtomically", func(t *testing.T) { t.Helper(); checkRecordExternalUIDAliasesRejectsCycleAtomically(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRecordExternalUIDAliasesRejectsCycleAtomically(t, newPostgresQueryStore)
		}},
		{"ActorKeysForPersonForwardsMergedPersonID", func(t *testing.T) { t.Helper(); checkActorKeysForPersonForwardsMergedPersonID(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkActorKeysForPersonForwardsMergedPersonID(t, newPostgresQueryStore)
		}},
		{"CustodianPackageScopeKeepsDefaultAndRecordsSeparate", func(t *testing.T) {
			t.Helper()
			checkCustodianPackageScopeKeepsDefaultAndRecordsSeparate(t, newTestStore)
		}, nil},
		{"CustodianAssignmentUsesRealVersion", func(t *testing.T) { t.Helper(); checkCustodianAssignmentUsesRealVersion(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkCustodianAssignmentUsesRealVersion(t, newPostgresQueryStore) }},
		{"CustodianAdditionalRejectsDuplicateClaim", func(t *testing.T) { t.Helper(); checkCustodianAdditionalRejectsDuplicateClaim(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkCustodianAdditionalRejectsDuplicateClaim(t, newPostgresQueryStore)
		}},
		{"CustodianCollectionAppliesOnlyToMembers", func(t *testing.T) { t.Helper(); checkCustodianCollectionAppliesOnlyToMembers(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkCustodianCollectionAppliesOnlyToMembers(t, newPostgresQueryStore) }},
		{"CustodianCollectionRejectsCallerSuppliedProvenance", func(t *testing.T) {
			t.Helper()
			checkCustodianCollectionRejectsCallerSuppliedProvenance(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkCustodianCollectionRejectsCallerSuppliedProvenance(t, newPostgresQueryStore)
		}},
		{"CustodiansIncludesRetiredPersonAsUnresolved", func(t *testing.T) { t.Helper(); checkCustodiansIncludesRetiredPersonAsUnresolved(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkCustodiansIncludesRetiredPersonAsUnresolved(t, newPostgresQueryStore)
		}},
		{"CustodianMutationUsesRevisionFenceAndNullableCoordinates", func(t *testing.T) {
			t.Helper()
			checkCustodianMutationUsesRevisionFenceAndNullableCoordinates(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkCustodianMutationUsesRevisionFenceAndNullableCoordinates(t, newPostgresQueryStore)
		}},
		{"CustodiansForVersionOrdersDocumentBeforeCollectionClaims", func(t *testing.T) {
			t.Helper()
			checkCustodiansForVersionOrdersDocumentBeforeCollectionClaims(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkCustodiansForVersionOrdersDocumentBeforeCollectionClaims(t, newPostgresQueryStore)
		}},
		{"ResolveCustodianPreservesSenderClaimAndResolvesAlias", func(t *testing.T) {
			t.Helper()
			checkResolveCustodianPreservesSenderClaimAndResolvesAlias(t, newTestStore)
		}, nil},
		{"OperatorPackageCustodianDoesNotReplaceSenderPrimary", func(t *testing.T) {
			t.Helper()
			checkOperatorPackageCustodianDoesNotReplaceSenderPrimary(t, newTestStore)
		}, nil},
		{"MergePersonsDeduplicatesAliasesAndReplaysReceipt", func(t *testing.T) { t.Helper(); checkMergePersonsDeduplicatesAliasesAndReplaysReceipt(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMergePersonsDeduplicatesAliasesAndReplaysReceipt(t, newPostgresQueryStore)
		}},
		{"MergePersonsRejectsMovedRecordsBeyondReceiptLimit", func(t *testing.T) {
			t.Helper()
			checkMergePersonsRejectsMovedRecordsBeyondReceiptLimit(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMergePersonsRejectsMovedRecordsBeyondReceiptLimit(t, newPostgresQueryStore)
		}},
		{"MergeReceiptKeepsExternalTupleComponentsDistinct", func(t *testing.T) { t.Helper(); checkMergeReceiptKeepsExternalTupleComponentsDistinct(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMergeReceiptKeepsExternalTupleComponentsDistinct(t, newPostgresQueryStore)
		}},
		{"MergePersonsRejectsConflictingCurrentExternalUIDs", func(t *testing.T) {
			t.Helper()
			checkMergePersonsRejectsConflictingCurrentExternalUIDs(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMergePersonsRejectsConflictingCurrentExternalUIDs(t, newPostgresQueryStore)
		}},
		{"MergePersonsPreservesExternalUIDTransitions", func(t *testing.T) { t.Helper(); checkMergePersonsPreservesExternalUIDTransitions(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMergePersonsPreservesExternalUIDTransitions(t, newPostgresQueryStore)
		}},
		{"MergePersonsRewritesAliasChainsToOneHop", func(t *testing.T) { t.Helper(); checkMergePersonsRewritesAliasChainsToOneHop(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMergePersonsRewritesAliasChainsToOneHop(t, newPostgresQueryStore) }},
		{"SplitPersonMovesOnlyExplicitIdentitiesAndReplays", func(t *testing.T) { t.Helper(); checkSplitPersonMovesOnlyExplicitIdentitiesAndReplays(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkSplitPersonMovesOnlyExplicitIdentitiesAndReplays(t, newPostgresQueryStore)
		}},
		{"SplitPersonMovesCustodianAndExternalUID", func(t *testing.T) { t.Helper(); checkSplitPersonMovesCustodianAndExternalUID(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkSplitPersonMovesCustodianAndExternalUID(t, newPostgresQueryStore) }},
		{"PersonSplitMissingAssertion", func(t *testing.T) { t.Helper(); checkPersonSplitMissingAssertion(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonSplitMissingAssertion(t, newPostgresQueryStore) }},
		{"MergePersonsPreservesKeyFace", func(t *testing.T) { t.Helper(); checkMergePersonsPreservesKeyFace(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMergePersonsPreservesKeyFace(t, newPostgresQueryStore) }},

		{"DocumentPeopleFactsSensitiveUnresolvedActor", func(t *testing.T) { t.Helper(); checkDocumentPeopleFactsSensitiveUnresolvedActor(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFactsSensitiveUnresolvedActor(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleFactsSafeHiddenVisibilityUsesCallerSession", func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFactsSafeHiddenVisibilityUsesCallerSession(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFactsSafeHiddenVisibilityUsesCallerSession(t, newPostgresQueryStore)
		}},
		{"DocumentDateFactsNeverReturnsStaleClaims", func(t *testing.T) { t.Helper(); checkDocumentDateFactsNeverReturnsStaleClaims(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentDateFactsNeverReturnsStaleClaims(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleFactsPublicationAndFailure", func(t *testing.T) { t.Helper(); checkDocumentPeopleFactsPublicationAndFailure(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFactsPublicationAndFailure(t, newPostgresQueryStore)
		}},
		{"DocumentPeopleFactsFiltersBlindCopyEdge", func(t *testing.T) { t.Helper(); checkDocumentPeopleFactsFiltersBlindCopyEdge(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkDocumentPeopleFactsFiltersBlindCopyEdge(t, newPostgresQueryStore) }},
		{"DocumentPeopleFactsAssertionsAndRetiredPerson", func(t *testing.T) { t.Helper(); checkDocumentPeopleFactsAssertionsAndRetiredPerson(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkDocumentPeopleFactsAssertionsAndRetiredPerson(t, newPostgresQueryStore)
		}},
		{"OpenCandidatesForVersionScopesOccurrences", func(t *testing.T) {
			t.Helper()
			checkOpenCandidatesForVersionScopesOccurrences(t, false)
		}, func(t *testing.T) {
			t.Helper()
			checkOpenCandidatesForVersionScopesOccurrences(t, true)
		}},
		{"OpenPersonCandidateReplaysDecisionAndSeparatesNewEvidence", func(t *testing.T) {
			t.Helper()
			checkOpenPersonCandidateReplaysDecisionAndSeparatesNewEvidence(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkOpenPersonCandidateReplaysDecisionAndSeparatesNewEvidence(t, newPostgresQueryStore)
		}},
		{"DecidePersonCandidateLinksOnlyCapturedOccurrences", func(t *testing.T) {
			t.Helper()
			checkDecidePersonCandidateLinksOnlyCapturedOccurrences(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDecidePersonCandidateLinksOnlyCapturedOccurrences(t, newPostgresQueryStore)
		}},
		{"DecidePersonCandidateCreatesCuratedPersonAtomically", func(t *testing.T) {
			t.Helper()
			checkDecidePersonCandidateCreatesCuratedPersonAtomically(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDecidePersonCandidateCreatesCuratedPersonAtomically(t, newPostgresQueryStore)
		}},
		{"CandidateDecisionsAdvanceBindingEpochEvenWithoutNewAssertions", func(t *testing.T) {
			t.Helper()
			checkCandidateDecisionsAdvanceBindingEpochEvenWithoutNewAssertions(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkCandidateDecisionsAdvanceBindingEpochEvenWithoutNewAssertions(t, newPostgresQueryStore)
		}},
		{"AssertDocumentPersonUsesRevisionFenceAndBindingEpoch", func(t *testing.T) {
			t.Helper()
			checkAssertDocumentPersonUsesRevisionFenceAndBindingEpoch(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkAssertDocumentPersonUsesRevisionFenceAndBindingEpoch(t, newPostgresQueryStore)
		}},
		{"OpenPersonCandidateReportsQueueCapacity", func(t *testing.T) { t.Helper(); checkOpenPersonCandidateReportsQueueCapacity(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkOpenPersonCandidateReportsQueueCapacity(t, newPostgresQueryStore) }},
		{"CandidateAuthoritySurvivesPersonMerge", func(t *testing.T) { t.Helper(); checkCandidateAuthoritySurvivesPersonMerge(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkCandidateAuthoritySurvivesPersonMerge(t, newPostgresQueryStore) }},
		{"CandidateRetirementPreservesAssertionsAndSupersedesSuggestions", func(t *testing.T) {
			t.Helper()
			checkCandidateRetirementPreservesAssertionsAndSupersedesSuggestions(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkCandidateRetirementPreservesAssertionsAndSupersedesSuggestions(t, newPostgresQueryStore)
		}},
		{"CandidateAssertionsMergeMatchingActions", func(t *testing.T) { t.Helper(); checkCandidateAssertionsMergeMatchingActions(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkCandidateAssertionsMergeMatchingActions(t, newPostgresQueryStore) }},
		{"CandidateAuthorityPreventsMetadataImport", func(t *testing.T) { t.Helper(); checkCandidateAuthorityPreventsMetadataImport(t, newTestStore) }, nil},
		{"CandidateLinksSkipPrunedVersions", func(t *testing.T) { t.Helper(); checkCandidateLinksSkipPrunedVersions(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkCandidateLinksSkipPrunedVersions(t, newPostgresQueryStore) }},
		{"PersonAssertionDeletionAdvancesBindingEpoch", func(t *testing.T) { t.Helper(); checkPersonAssertionDeletionAdvancesBindingEpoch(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPersonAssertionDeletionAdvancesBindingEpoch(t, newPostgresQueryStore)
		}},
		{"BrowserMergeHiddenCustodian", func(t *testing.T) {
			t.Helper()
			checkBrowserMergeHiddenCustodian(t, newTestStore, []string{"document", "collection", "package", "package-snapshot", "package-record"})
		}, func(t *testing.T) {
			t.Helper()
			checkBrowserMergeHiddenCustodian(t, newPostgresQueryStore, []string{"document", "collection"})
		}},
		{"BrowserMergeRequiresHiddenUnlock", func(t *testing.T) { t.Helper(); checkBrowserMergeRequiresHiddenUnlock(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBrowserMergeRequiresHiddenUnlock(t, newPostgresQueryStore) }},
		{"PersonFacePreviews", func(t *testing.T) { t.Helper(); checkPersonFacePreviews(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonFacePreviews(t, newPostgresQueryStore) }},
		{"PersonFacePreviewsPreferAssignedSource", func(t *testing.T) { t.Helper(); checkPersonFacePreviewsPreferAssignedSource(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonFacePreviewsPreferAssignedSource(t, newPostgresQueryStore) }},
		{"PersonPhotoPreviewSourceFollowsAssignment", func(t *testing.T) { t.Helper(); checkPersonPhotoPreviewSourceFollowsAssignment(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPersonPhotoPreviewSourceFollowsAssignment(t, newPostgresQueryStore)
		}},
		{"WholePhotoMarkCreatesPersonAtomically", func(t *testing.T) { t.Helper(); checkWholePhotoMarkCreatesPersonAtomically(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkWholePhotoMarkCreatesPersonAtomically(t, newPostgresQueryStore) }},
		{"PhotoFaceManualNamesAndBoxValidation", func(t *testing.T) { t.Helper(); checkPhotoFaceManualNamesAndBoxValidation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceManualNamesAndBoxValidation(t, newPostgresQueryStore) }},
		{"PhotoFaceMergeRetargetsDuplicateAssertions", func(t *testing.T) { t.Helper(); checkPhotoFaceMergeRetargetsDuplicateAssertions(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceMergeRetargetsDuplicateAssertions(t, newPostgresQueryStore)
		}},
		{"PhotoFaceReplaysNamedFaceAfterPersonRetires", func(t *testing.T) { t.Helper(); checkPhotoFaceReplaysNamedFaceAfterPersonRetires(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceReplaysNamedFaceAfterPersonRetires(t, newPostgresQueryStore)
		}},
		{"PhotoFaceReplaysMergedPersonAfterSurvivorRetires", func(t *testing.T) { t.Helper(); checkPhotoFaceReplaysMergedPersonAfterSurvivorRetires(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceReplaysMergedPersonAfterSurvivorRetires(t, newPostgresQueryStore)
		}},
		{"PhotoFaceHiddenReadsAndWrites", func(t *testing.T) { t.Helper(); checkPhotoFaceHiddenReadsAndWrites(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceHiddenReadsAndWrites(t, newPostgresQueryStore) }},
		{"PhotoFaceAuditWritesFailClosed", func(t *testing.T) { t.Helper(); checkPhotoFaceAuditWritesFailClosed(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceAuditWritesFailClosed(t, newPostgresQueryStore) }},
		{"PhotoFaceBoxStaysStoredWhenPhotoRotationChanges", func(t *testing.T) { t.Helper(); checkPhotoFaceBoxStaysStoredWhenPhotoRotationChanges(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceBoxStaysStoredWhenPhotoRotationChanges(t, newPostgresQueryStore)
		}},
		{"PhotoFaceObservationTargetEligibility", func(t *testing.T) { t.Helper(); checkPhotoFaceObservationTargetEligibility(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceObservationTargetEligibility(t, newPostgresQueryStore) }},
		{"PhotoFaceReadIncludesSuppressedReference", func(t *testing.T) { t.Helper(); checkPhotoFaceReadIncludesSuppressedReference(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceReadIncludesSuppressedReference(t, newPostgresQueryStore)
		}},
		{"PhotoFaceGenericDepictedAuthorization", func(t *testing.T) { t.Helper(); checkPhotoFaceGenericDepictedAuthorization(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceGenericDepictedAuthorization(t, newPostgresQueryStore) }},
		{"MarkPersonInPhotoNamedRetryAfterMerge", func(t *testing.T) { t.Helper(); checkMarkPersonInPhotoNamedRetryAfterMerge(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMarkPersonInPhotoNamedRetryAfterMerge(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionNameRetractUndo", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionNameRetractUndo(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionNameRetractUndo(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionRejectDismissRename", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionRejectDismissRename(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionRejectDismissRename(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionUndoFencesChangedRecords", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionUndoFencesChangedRecords(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionUndoFencesChangedRecords(t, newPostgresQueryStore)
		}},
		{"PhotoFaceDecisionGroupedRejection", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionGroupedRejection(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionGroupedRejection(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionUndoRejectsReusedPreviousKeyFace", func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionUndoRejectsReusedPreviousKeyFace(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionUndoRejectsReusedPreviousKeyFace(t, newPostgresQueryStore)
		}},
		{"PhotoFaceDecisionWithdrawalMerge", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionWithdrawalMerge(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionWithdrawalMerge(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionPhotoMarkMerge", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionPhotoMarkMerge(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionPhotoMarkMerge(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionUndoClearRefusesRetiredPerson", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionUndoClearRefusesRetiredPerson(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionUndoClearRefusesRetiredPerson(t, newPostgresQueryStore)
		}},
		{"PhotoFaceNamingPreservesReplayDigestsAndClearedPeople", func(t *testing.T) {
			t.Helper()
			checkPhotoFaceNamingPreservesReplayDigestsAndClearedPeople(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceNamingPreservesReplayDigestsAndClearedPeople(t, newPostgresQueryStore)
		}},
		{"PhotoFaceCandidateAcceptanceClearsDecisions", func(t *testing.T) { t.Helper(); checkPhotoFaceCandidateAcceptanceClearsDecisions(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceCandidateAcceptanceClearsDecisions(t, newPostgresQueryStore)
		}},
		{"PhotoFaceDecisionWithdrawalPreservesOpenSuggestion", func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionWithdrawalPreservesOpenSuggestion(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionWithdrawalPreservesOpenSuggestion(t, newPostgresQueryStore)
		}},
		{"PersonAssertionSelectorsFilterBeforePaging", func(t *testing.T) { t.Helper(); checkPersonAssertionSelectorsFilterBeforePaging(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPersonAssertionSelectorsFilterBeforePaging(t, newPostgresQueryStore)
		}},
		{"PhotoFaceCandidateReadsAndDecisionsRequireHiddenAccess", func(t *testing.T) {
			t.Helper()
			checkPhotoFaceCandidateReadsAndDecisionsRequireHiddenAccess(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceCandidateReadsAndDecisionsRequireHiddenAccess(t, newPostgresQueryStore)
		}},
		{"PhotoFaceDecisionCandidateCapacity", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionCandidateCapacity(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionCandidateCapacity(t, newPostgresQueryStore) }},
		{"PhotoFaceDecisionRepeatedGroupedDismissal", func(t *testing.T) { t.Helper(); checkPhotoFaceDecisionRepeatedGroupedDismissal(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceDecisionRepeatedGroupedDismissal(t, newPostgresQueryStore)
		}},
		{"PersonMergeBumpsAffectedFaceOnce", func(t *testing.T) { t.Helper(); checkPersonMergeBumpsAffectedFaceOnce(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPersonMergeBumpsAffectedFaceOnce(t, newPostgresQueryStore) }},
		{"PhotoFaceMergeUnionsRejections", func(t *testing.T) { t.Helper(); checkPhotoFaceMergeUnionsRejections(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceMergeUnionsRejections(t, newPostgresQueryStore) }},
		{"PhotoFaceCandidateHistoryDoesNotFollowReusedFaceID", func(t *testing.T) {
			t.Helper()
			checkPhotoFaceCandidateHistoryDoesNotFollowReusedFaceID(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceCandidateHistoryDoesNotFollowReusedFaceID(t, newPostgresQueryStore)
		}},
		{"FaceCandidateRetainsMembershipForReusedFaceID", func(t *testing.T) { t.Helper(); checkFaceCandidateRetainsMembershipForReusedFaceID(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkFaceCandidateRetainsMembershipForReusedFaceID(t, newPostgresQueryStore)
		}},
		{"PersonMergeIgnoresCandidateFromPrunedFaceVersion", func(t *testing.T) { t.Helper(); checkPersonMergeIgnoresCandidateFromPrunedFaceVersion(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPersonMergeIgnoresCandidateFromPrunedFaceVersion(t, newPostgresQueryStore)
		}},
		{"PhotoFaceMergeKeepsSurvivorSuggestion", func(t *testing.T) { t.Helper(); checkPhotoFaceMergeKeepsSurvivorSuggestion(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceMergeKeepsSurvivorSuggestion(t, newPostgresQueryStore) }},
		{"PhotoFaceNamingSupersedesOtherSuggestions", func(t *testing.T) { t.Helper(); checkPhotoFaceNamingSupersedesOtherSuggestions(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceNamingSupersedesOtherSuggestions(t, newPostgresQueryStore)
		}},
		{"PhotoFaceWithdrawalUndoFencedByLaterRejection", func(t *testing.T) { t.Helper(); checkPhotoFaceWithdrawalUndoFencedByLaterRejection(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceWithdrawalUndoFencedByLaterRejection(t, newPostgresQueryStore)
		}},
		{"PhotoFaceClearedKeyUndoIgnoresRequestPhoto", func(t *testing.T) { t.Helper(); checkPhotoFaceClearedKeyUndoIgnoresRequestPhoto(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoFaceClearedKeyUndoIgnoresRequestPhoto(t, newPostgresQueryStore)
		}},
	}},
	{name: "photo-review", postgres: "native operations", scope: "Explicit-selection grouping, keeper and representative decisions, best-shot responses and undo; query browsing and people curation remain separate.", scenarios: []backendScenario{
		{"PhotoStackTimeRules", func(t *testing.T) { t.Helper(); checkPhotoStackTimeRules(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoStackTimeRules(t, newPostgresQueryStore) }},
		{"PhotoStackCameraSerialMatching", func(t *testing.T) { t.Helper(); checkPhotoStackCameraSerialMatching(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoStackCameraSerialMatching(t, newPostgresQueryStore) }},
		{"PhotoStackDecisionAfterSidecarPublication", func(t *testing.T) { t.Helper(); checkPhotoStackDecisionAfterSidecarPublication(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoStackDecisionAfterSidecarPublication(t, newPostgresQueryStore)
		}},
		{"PhotoStackRejectsStaleMembershipAndTargetsWithoutWrites", func(t *testing.T) {
			t.Helper()
			checkPhotoStackRejectsStaleMembershipAndTargetsWithoutWrites(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoStackRejectsStaleMembershipAndTargetsWithoutWrites(t, newPostgresQueryStore)
		}},
		{"PhotoStackAutomaticDecisionsPreserveCompanionFlags", func(t *testing.T) {
			t.Helper()
			checkPhotoStackAutomaticDecisionsPreserveCompanionFlags(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoStackAutomaticDecisionsPreserveCompanionFlags(t, newPostgresQueryStore)
		}},
		{"PhotoRepresentativeDecisionKeepsDefaultSettings", func(t *testing.T) { t.Helper(); checkPhotoRepresentativeDecisionKeepsDefaultSettings(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoRepresentativeDecisionKeepsDefaultSettings(t, newPostgresQueryStore)
		}},
		{"PhotoStackDecisionsRefuseStaleMetadata", func(t *testing.T) { t.Helper(); checkPhotoStackDecisionsRefuseStaleMetadata(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoStackDecisionsRefuseStaleMetadata(t, newPostgresQueryStore) }},
		{"PhotoBestShotsDecisions", func(t *testing.T) { t.Helper(); checkPhotoBestShotsDecisions(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoBestShotsDecisions(t, newPostgresQueryStore) }},
		{"PhotoBestShotsReadsAndStaleOffers", func(t *testing.T) { t.Helper(); checkPhotoBestShotsReadsAndStaleOffers(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoBestShotsReadsAndStaleOffers(t, newPostgresQueryStore) }},
		{"PhotoBestShotsFailedEnrichmentPublishesNothing", func(t *testing.T) { t.Helper(); checkPhotoBestShotsFailedEnrichmentPublishesNothing(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoBestShotsFailedEnrichmentPublishesNothing(t, newPostgresQueryStore)
		}},
		{"PhotoBestShotsConfirmedWinner", func(t *testing.T) { t.Helper(); checkPhotoBestShotsConfirmedWinner(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoBestShotsConfirmedWinner(t, newPostgresQueryStore) }},
		{"PhotoReviewRepresentatives", func(t *testing.T) { t.Helper(); checkPhotoReviewRepresentatives(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoReviewRepresentatives(t, newPostgresQueryStore) }},
		{"PhotoReviewHidden", func(t *testing.T) { t.Helper(); checkPhotoReviewHidden(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoReviewHidden(t, newPostgresQueryStore) }},
		{"PhotoReviewRollback", nil, checkPhotoReviewRollbackPostgres},
		{"PhotoReviewSnapshot", nil, checkPhotoReviewSnapshotPostgres},
		{"PhotoReviewRetainedVectors", nil, checkPhotoReviewRetainedVectorsPostgres},
	}},
	{name: "photo-albums", postgres: "native operations", scope: "Album lifecycle, explicit membership and cover summaries; query selection and snapshot delivery remain unqualified.", scenarios: []backendScenario{
		{"PhotoSetLifecycle", func(t *testing.T) { t.Helper(); checkPhotoSetLifecycle(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoSetLifecycle(t, newPostgresQueryStore) }},
		{"PhotoSetCovers", nil, func(t *testing.T) {
			t.Helper()
			t.Parallel()
			s, _, _ := photoSetCoverScenario(t, newPostgresQueryStore)
			checkRetentionMetadata(t, s)
		}},
		{"PhotoHiddenCoverAndPromoteAuthorization", func(t *testing.T) { t.Helper(); checkPhotoHiddenCoverAndPromoteAuthorization(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenCoverAndPromoteAuthorization(t, newPostgresQueryStore) }},
		{"PhotoSetReceiptsAndOrdering", func(t *testing.T) { t.Helper(); checkPhotoSetReceiptsAndOrdering(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoSetReceiptsAndOrdering(t, newPostgresQueryStore) }},
		{"PhotoSetHiddenSummaries", func(t *testing.T) { t.Helper(); checkPhotoSetHiddenSummaries(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoSetHiddenSummaries(t, newPostgresQueryStore) }},
		{"PhotoSetSnapshots", nil, checkPhotoSetSnapshotsPostgres},
		{"PhotoSetRollback", nil, checkPhotoSetRollbackPostgres},
	}},
	{name: "photo-quality-hidden", postgres: "native operations", scope: "Quality publication, hidden sessions and photo settings; excludes query browsing and people curation.", scenarios: []backendScenario{
		{"PhotoQualityLifecycle", func(t *testing.T) { t.Helper(); checkPhotoQualityLifecycle(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoQualityLifecycle(t, newPostgresQueryStore) }},
		{"PhotoQualityDetectedInputs", func(t *testing.T) { t.Helper(); checkPhotoQualityDetectedInputs(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoQualityDetectedInputs(t, newPostgresQueryStore) }},
		{"PhotoHiddenLifecycle", nil, checkPhotoHiddenLifecyclePostgres},
		{"PhotoSettingsRecomputeInheritedAssets", func(t *testing.T) { t.Helper(); checkPhotoSettingsRecomputeInheritedAssets(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoSettingsRecomputeInheritedAssets(t, newPostgresQueryStore) }},
		{"PhotoQualityRetainedDismissal", nil, checkPhotoQualityRetainedDismissalPostgres},
		{"PhotoQualityHiddenSnapshots", nil, checkPhotoQualityHiddenSnapshotsPostgres},
		{"PhotoQualityHiddenRollback", nil, checkPhotoQualityHiddenRollbackPostgres},
		{"PhotoQualityDisplayAndPublicationFence", func(t *testing.T) { t.Helper(); checkPhotoQualityDisplayAndPublicationFence(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoQualityDisplayAndPublicationFence(t, newPostgresQueryStore) }},
		{"PhotoHiddenPasscodeNormalization", func(t *testing.T) { t.Helper(); checkPhotoHiddenPasscodeNormalization(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenPasscodeNormalization(t, newPostgresQueryStore) }},
		{"PhotoHiddenRestorePreservesHidden", func(t *testing.T) { t.Helper(); checkPhotoHiddenRestorePreservesHidden(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenRestorePreservesHidden(t, newPostgresQueryStore) }},
		{"PhotoHiddenVisibilityChangeID", func(t *testing.T) { t.Helper(); checkPhotoHiddenVisibilityChangeID(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenVisibilityChangeID(t, newPostgresQueryStore) }},
		{"PhotoHiddenDisableRecordsEveryAsset", func(t *testing.T) { t.Helper(); checkPhotoHiddenDisableRecordsEveryAsset(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenDisableRecordsEveryAsset(t, newPostgresQueryStore) }},
		{"PhotoHiddenMutationGate", func(t *testing.T) { t.Helper(); checkPhotoHiddenMutationGate(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenMutationGate(t, newPostgresQueryStore) }},
		{"PhotoHiddenUnlockLifetimeOverride", func(t *testing.T) { t.Helper(); checkPhotoHiddenUnlockLifetimeOverride(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoHiddenUnlockLifetimeOverride(t, newPostgresQueryStore) }},
	}},
	{name: "photo-analysis", postgres: "native operations", scope: "Description and face-detection jobs and suggestion responses; excludes provider network calls and people curation.", scenarios: []backendScenario{
		{"PhotoFaceDetectionJob", func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionJob(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionJob(t, newPostgresQueryStore) }},
		{"PhotoAnalysisTargetPlan", func(t *testing.T) { t.Helper(); checkPhotoAnalysisTargetPlan(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoAnalysisTargetPlan(t, newPostgresQueryStore) }},
		{"PhotoAnalysisPublicationRollback", nil, checkPhotoAnalysisPublicationRollbackPostgres},
		{"PhotoSuggestionResponseRollback", nil, checkPhotoSuggestionResponseRollbackPostgres},
		{"PhotoFaceDetectionStableOverlap", func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionStableOverlap(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionStableOverlap(t, newPostgresQueryStore) }},
		{"PhotoFaceDetectionProtectsCandidatesAndUndoReceipts", func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionProtectsCandidatesAndUndoReceipts(t, false) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionProtectsCandidatesAndUndoReceipts(t, true) }},
		{"PhotoDescriptionTargetsRespectActiveTaskScope", func(t *testing.T) { t.Helper(); checkPhotoDescriptionTargetsRespectActiveTaskScope(t, false) }, func(t *testing.T) { t.Helper(); checkPhotoDescriptionTargetsRespectActiveTaskScope(t, true) }},

		{"PhotoSuggestionAcceptsSelectedFieldsAndDismissesTheRest", func(t *testing.T) {
			t.Helper()
			checkPhotoSuggestionAcceptsSelectedFieldsAndDismissesTheRest(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoSuggestionAcceptsSelectedFieldsAndDismissesTheRest(t, newPostgresQueryStore)
		}},
		{"PhotoSuggestionResponseChecksTheAssetItWasReviewedUnder", func(t *testing.T) {
			t.Helper()
			checkPhotoSuggestionResponseChecksTheAssetItWasReviewedUnder(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoSuggestionResponseChecksTheAssetItWasReviewedUnder(t, newPostgresQueryStore)
		}},
		{"PhotoSuggestionOffersNothingTheOwnerAlreadyHas", func(t *testing.T) { t.Helper(); checkPhotoSuggestionOffersNothingTheOwnerAlreadyHas(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoSuggestionOffersNothingTheOwnerAlreadyHas(t, newPostgresQueryStore)
		}},
		{"PhotoDescriptionJobPublishesOneSuggestionAndSettles", func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionJobPublishesOneSuggestionAndSettles(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionJobPublishesOneSuggestionAndSettles(t, newPostgresQueryStore)
		}},
		{"PhotoDescriptionTargetsReadsOnePageInOrder", func(t *testing.T) { t.Helper(); checkPhotoDescriptionTargetsReadsOnePageInOrder(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionTargetsReadsOnePageInOrder(t, newPostgresQueryStore)
		}},
		{"PhotoDescriptionExpiredGrantOverlappedByRenewalPublishesNothing", func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionExpiredGrantOverlappedByRenewalPublishesNothing(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionExpiredGrantOverlappedByRenewalPublishesNothing(t, newPostgresQueryStore)
		}},
		{"PhotoDescriptionWaitersKeepTheirOwnDismissalRule", func(t *testing.T) { t.Helper(); checkPhotoDescriptionWaitersKeepTheirOwnDismissalRule(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionWaitersKeepTheirOwnDismissalRule(t, newPostgresQueryStore)
		}},
		{"PhotoDescriptionLeaseExpiryRetriesInsteadOfQuarantining", func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionLeaseExpiryRetriesInsteadOfQuarantining(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionLeaseExpiryRetriesInsteadOfQuarantining(t, newPostgresQueryStore)
		}},
		{"PhotoRotationResponses", func(t *testing.T) { t.Helper(); checkPhotoRotationResponses(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoRotationResponses(t, newPostgresQueryStore) }},
		{"PhotoRotationResetsOnRepublish", func(t *testing.T) { t.Helper(); checkPhotoRotationResetsOnRepublish(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoRotationResetsOnRepublish(t, newPostgresQueryStore) }},
		{"PhotoRotationResponseRequiresCurrentProposal", func(t *testing.T) { t.Helper(); checkPhotoRotationResponseRequiresCurrentProposal(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoRotationResponseRequiresCurrentProposal(t, newPostgresQueryStore)
		}},
		{"PhotoDescriptionExplicitRefreshKeepsTextDecisions", func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionExplicitRefreshKeepsTextDecisions(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoDescriptionExplicitRefreshKeepsTextDecisions(t, newPostgresQueryStore)
		}},
		{"PhotoFaceDetectionCapacityRollback", func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionCapacityRollback(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoFaceDetectionCapacityRollback(t, newPostgresQueryStore) }},
	}},
	{name: "photo-checkout", postgres: "native operations", scope: "Checkout selection, publication, observation, commit-back and retirement; excludes public vault opening and filesystem workers.", scenarios: []backendScenario{
		{"PhotoCheckoutCommitAndRebind", func(t *testing.T) { t.Helper(); checkPhotoCheckoutCommitAndRebind(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoCheckoutCommitAndRebind(t, newPostgresQueryStore) }},
		{"PhotoCheckoutAlbumSelection", nil, checkPhotoCheckoutAlbumSelectionPostgres},
		{"PhotoCheckoutPublicationRecovery", nil, checkPhotoCheckoutPublicationRecoveryPostgres},
		{"PhotoCheckoutCommitRollback", nil, checkPhotoCheckoutCommitRollbackPostgres},
		{"PhotoCheckoutCommitSnapshot", nil, checkPhotoCheckoutCommitSnapshotPostgres},
		{"PhotoCheckoutObservationBatches", nil, checkPhotoCheckoutObservationBatchesPostgres},
		{"PreserveUncertainPhotoCheckoutHoldsSurvivesClearedPublication", func(t *testing.T) {
			t.Helper()
			checkPreserveUncertainPhotoCheckoutHoldsSurvivesClearedPublication(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPreserveUncertainPhotoCheckoutHoldsSurvivesClearedPublication(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutCreateValidation", func(t *testing.T) { t.Helper(); checkPhotoCheckoutCreateValidation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoCheckoutCreateValidation(t, newPostgresQueryStore) }},

		{"PhotoCheckoutStatusTracksOperationAcrossPauseAndFinish", func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutStatusTracksOperationAcrossPauseAndFinish(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutStatusTracksOperationAcrossPauseAndFinish(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutConflictsAndEntryPaths", func(t *testing.T) { t.Helper(); checkPhotoCheckoutConflictsAndEntryPaths(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoCheckoutConflictsAndEntryPaths(t, newPostgresQueryStore) }},
		{"PhotoCheckoutPreservesNodeBasenameWithoutProvenance", func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutPreservesNodeBasenameWithoutProvenance(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutPreservesNodeBasenameWithoutProvenance(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutSkipsNamesTooLongForTheDestination", func(t *testing.T) { t.Helper(); checkPhotoCheckoutSkipsNamesTooLongForTheDestination(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutSkipsNamesTooLongForTheDestination(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutNamesURLImportsByDocument", func(t *testing.T) { t.Helper(); checkPhotoCheckoutNamesURLImportsByDocument(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoCheckoutNamesURLImportsByDocument(t, newPostgresQueryStore) }},
		{"PhotoCheckoutEntryRequiresFrozenEligibleSource", func(t *testing.T) { t.Helper(); checkPhotoCheckoutEntryRequiresFrozenEligibleSource(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutEntryRequiresFrozenEligibleSource(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutRetireAfterAuditActivation", func(t *testing.T) { t.Helper(); checkPhotoCheckoutRetireAfterAuditActivation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoCheckoutRetireAfterAuditActivation(t, newPostgresQueryStore) }},
		{"PhotoCheckoutYearFolderUsesTimelineDay", func(t *testing.T) { t.Helper(); checkPhotoCheckoutYearFolderUsesTimelineDay(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoCheckoutYearFolderUsesTimelineDay(t, newPostgresQueryStore) }},
		{"PhotoCheckoutCreationRejectsSelectionPurgedBeforeCommit", func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutCreationRejectsSelectionPurgedBeforeCommit(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutCreationRejectsSelectionPurgedBeforeCommit(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutSkipsDuplicateNamesWithBoundedDetails", func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutSkipsDuplicateNamesWithBoundedDetails(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutSkipsDuplicateNamesWithBoundedDetails(t, newPostgresQueryStore)
		}},
		{"PhotoCheckoutScanOrCommitRequiresReservation", func(t *testing.T) { t.Helper(); checkPhotoCheckoutScanOrCommitRequiresReservation(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoCheckoutScanOrCommitRequiresReservation(t, newPostgresQueryStore)
		}},
	}},
	{name: "photo-import", postgres: "native operations", scope: "Grouped RAW/JPEG/sidecar ingestion, source replay, explicit pairing, browser continuation and import progress; excludes authored editing and XMP publication.", scenarios: []backendScenario{
		{"PhotoImportRollback", nil, checkPhotoImportRollbackPostgres},
		{"PhotoImportAcceptedSidecar", nil, checkPhotoImportAcceptedSidecarPostgres},
		{"PhotoImportEmbeddedObservation", func(t *testing.T) { t.Helper(); checkPhotoImportEmbeddedObservation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportEmbeddedObservation(t, newPostgresQueryStore) }},
		{"OrdinaryPhotoMediaEnrollment", func(t *testing.T) { t.Helper(); checkOrdinaryPhotoMediaEnrollment(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkOrdinaryPhotoMediaEnrollment(t, newPostgresQueryStore) }},
		{"PhotoImportLateSidecarUpdatesLockedHiddenAsset", func(t *testing.T) { t.Helper(); checkPhotoImportLateSidecarUpdatesLockedHiddenAsset(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportLateSidecarUpdatesLockedHiddenAsset(t, newPostgresQueryStore)
		}},
		{"PhotoImportAtomicGroup", func(t *testing.T) { t.Helper(); checkPhotoImportAtomicGroup(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportAtomicGroup(t, newPostgresQueryStore) }},
		{"PhotoImportQualifiedSidecarTargets", func(t *testing.T) { t.Helper(); checkPhotoImportQualifiedSidecarTargets(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportQualifiedSidecarTargets(t, newPostgresQueryStore) }},
		{"PhotoImportSeparatePhotosAreReported", func(t *testing.T) { t.Helper(); checkPhotoImportSeparatePhotosAreReported(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportSeparatePhotosAreReported(t, newPostgresQueryStore) }},
		{"PhotoImportMultipleRAWsImportAlone", func(t *testing.T) { t.Helper(); checkPhotoImportMultipleRAWsImportAlone(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportMultipleRAWsImportAlone(t, newPostgresQueryStore) }},
		{"PhotoImportLoneSidecarWaitsForItsImage", func(t *testing.T) { t.Helper(); checkPhotoImportLoneSidecarWaitsForItsImage(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportLoneSidecarWaitsForItsImage(t, newPostgresQueryStore) }},
		{"PhotoImportOperatorPairedRAWsAreSettled", func(t *testing.T) { t.Helper(); checkPhotoImportOperatorPairedRAWsAreSettled(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportOperatorPairedRAWsAreSettled(t, newPostgresQueryStore) }},
		{"PhotoImportDedupObservation", func(t *testing.T) { t.Helper(); checkPhotoImportDedupObservation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportDedupObservation(t, newPostgresQueryStore) }},
		{"PhotoImportReusesOrdinaryRAWInAnotherFolder", func(t *testing.T) { t.Helper(); checkPhotoImportReusesOrdinaryRAWInAnotherFolder(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportReusesOrdinaryRAWInAnotherFolder(t, newPostgresQueryStore)
		}},
		{"PhotoImportEditedSourceBecomesNewVersion", func(t *testing.T) { t.Helper(); checkPhotoImportEditedSourceBecomesNewVersion(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportEditedSourceBecomesNewVersion(t, newPostgresQueryStore)
		}},
		{"PhotoImportCreateOnlyPreservesGroupContent", func(t *testing.T) { t.Helper(); checkPhotoImportCreateOnlyPreservesGroupContent(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportCreateOnlyPreservesGroupContent(t, newPostgresQueryStore)
		}},
		{"PhotoImportEditedSourceLeavesSharedNodeAlone", func(t *testing.T) { t.Helper(); checkPhotoImportEditedSourceLeavesSharedNodeAlone(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportEditedSourceLeavesSharedNodeAlone(t, newPostgresQueryStore)
		}},
		{"PhotoImportDuplicateRawNextToSeparateJPEGIsReported", func(t *testing.T) {
			t.Helper()
			checkPhotoImportDuplicateRawNextToSeparateJPEGIsReported(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoImportDuplicateRawNextToSeparateJPEGIsReported(t, newPostgresQueryStore)
		}},
		{"PhotoImportVideoWithSameNameJPEG", func(t *testing.T) { t.Helper(); checkPhotoImportVideoWithSameNameJPEG(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportVideoWithSameNameJPEG(t, newPostgresQueryStore) }},
		{"PhotoImportSidecarDedupStaysWithinSourceGroup", func(t *testing.T) { t.Helper(); checkPhotoImportSidecarDedupStaysWithinSourceGroup(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportSidecarDedupStaysWithinSourceGroup(t, newPostgresQueryStore)
		}},
		{"PhotoSidecarImageTarget", func(t *testing.T) { t.Helper(); checkPhotoSidecarImageTarget(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoSidecarImageTarget(t, newPostgresQueryStore) }},
		{"PhotoImportNodeStates", func(t *testing.T) { t.Helper(); checkPhotoImportNodeStates(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportNodeStates(t, newPostgresQueryStore) }},
		{"PhotoImportVersionStates", func(t *testing.T) { t.Helper(); checkPhotoImportVersionStates(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportVersionStates(t, newPostgresQueryStore) }},
		{"PhotoImportOperationLifecycle", func(t *testing.T) { t.Helper(); checkPhotoImportOperationLifecycle(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportOperationLifecycle(t, newPostgresQueryStore) }},
		{"FinalizeLocalOperationSettlesCancelRace", func(t *testing.T) { t.Helper(); checkFinalizeLocalOperationSettlesCancelRace(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkFinalizeLocalOperationSettlesCancelRace(t, newPostgresQueryStore) }},
		{"PhotoImportRefusesAuditedVault", func(t *testing.T) { t.Helper(); checkPhotoImportRefusesAuditedVault(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoImportRefusesAuditedVault(t, newPostgresQueryStore) }},
		{"PhotoImportDestinationCreatesAndReusesPath", func(t *testing.T) { t.Helper(); checkPhotoImportDestinationCreatesAndReusesPath(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportDestinationCreatesAndReusesPath(t, newPostgresQueryStore)
		}},
		{"PhotoImportDestinationRollsBackOnInvalidComponent", func(t *testing.T) {
			t.Helper()
			checkPhotoImportDestinationRollsBackOnInvalidComponent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoImportDestinationRollsBackOnInvalidComponent(t, newPostgresQueryStore)
		}},
		{"PhotoImportDestinationRejectsAuditedVault", func(t *testing.T) { t.Helper(); checkPhotoImportDestinationRejectsAuditedVault(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoImportDestinationRejectsAuditedVault(t, newPostgresQueryStore)
		}},
		{"PreparePhotoImportDestination", func(t *testing.T) { t.Helper(); checkPreparePhotoImportDestination(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPreparePhotoImportDestination(t, newPostgresQueryStore) }},
		{"BrowserPhotoDirectoryContinuation", func(t *testing.T) { t.Helper(); checkBrowserPhotoDirectoryContinuation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBrowserPhotoDirectoryContinuation(t, newPostgresQueryStore) }},
		{"BrowserPhotoSizeCandidates", func(t *testing.T) { t.Helper(); checkBrowserPhotoSizeCandidates(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkBrowserPhotoSizeCandidates(t, newPostgresQueryStore) }},
	}},
	{name: "photo-edit", postgres: "native operations", scope: "Explicit photo metadata edits, keywords and undo; XMP classification, conflicts and sidecar publication. Excludes ranked photo selection and people curation.", scenarios: []backendScenario{
		{"PhotoXMPEmbeddedSourceClaim", func(t *testing.T) { t.Helper(); checkPhotoXMPEmbeddedSourceClaim(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPEmbeddedSourceClaim(t, newPostgresQueryStore) }},
		{"PhotoEditRollback", nil, checkPhotoEditRollbackPostgres},
		{"PhotoPreparationSnapshot", nil, checkPhotoPreparationSnapshotPostgres},
		{"PhotoEditRetainedSuggestions", nil, checkPhotoEditRetainedSuggestionsPostgres},
		{"PhotoXMPPublication", func(t *testing.T) { t.Helper(); checkPhotoXMPPublication(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPPublication(t, newPostgresQueryStore) }},
		{"PhotoXMPConflictResolution", func(t *testing.T) { t.Helper(); checkPhotoXMPConflictResolution(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPConflictResolution(t, newPostgresQueryStore) }},

		{"PhotoAuthoredBatchRotationDeltaPreservesMemberDifferencesAndUndo", func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredBatchRotationDeltaPreservesMemberDifferencesAndUndo(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredBatchRotationDeltaPreservesMemberDifferencesAndUndo(t, newPostgresQueryStore)
		}},
		{"PhotoAuthoredBatchConflictsAndAtomicFailure", func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchConflictsAndAtomicFailure(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredBatchConflictsAndAtomicFailure(t, newPostgresQueryStore)
		}},
		{"PhotoAuthoredBatchViewedContent", func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchViewedContent(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchViewedContent(t, newPostgresQueryStore) }},
		{"PhotoAuthoredBatchRevisionGroups", func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchRevisionGroups(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchRevisionGroups(t, newPostgresQueryStore) }},
		{"PhotoKeywordEditNoop", func(t *testing.T) { t.Helper(); checkPhotoKeywordEditNoop(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoKeywordEditNoop(t, newPostgresQueryStore) }},
		{"PhotoKeywordUndo", func(t *testing.T) { t.Helper(); checkPhotoKeywordUndo(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoKeywordUndo(t, newPostgresQueryStore) }},
		{"PhotoAuthoredInitialDecisionUndoRestoresConfirmation", func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredInitialDecisionUndoRestoresConfirmation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredInitialDecisionUndoRestoresConfirmation(t, newPostgresQueryStore)
		}},
		{"PhotoAuthoredBatchTargetChecksFollowRequestOrder", func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchTargetChecksFollowRequestOrder(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredBatchTargetChecksFollowRequestOrder(t, newPostgresQueryStore)
		}},
		{"PhotoAuthoredBatchEditsTargetsAcrossLoadChunks", func(t *testing.T) { t.Helper(); checkPhotoAuthoredBatchEditsTargetsAcrossLoadChunks(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoAuthoredBatchEditsTargetsAcrossLoadChunks(t, newPostgresQueryStore)
		}},
		{"PhotoXMPTargetCursorVisitsEveryOriginalOnce", func(t *testing.T) { t.Helper(); checkPhotoXMPTargetCursorVisitsEveryOriginalOnce(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoXMPTargetCursorVisitsEveryOriginalOnce(t, newPostgresQueryStore)
		}},
		{"PhotoXMPInvalidation", func(t *testing.T) { t.Helper(); checkPhotoXMPInvalidation(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPInvalidation(t, newPostgresQueryStore) }},
		{"PhotoXMPInputEligibilityMatchesTargetRoles", func(t *testing.T) { t.Helper(); checkPhotoXMPInputEligibilityMatchesTargetRoles(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPhotoXMPInputEligibilityMatchesTargetRoles(t, newPostgresQueryStore)
		}},
		{"PhotoXMPURLSidecarPairingAndClaim", func(t *testing.T) { t.Helper(); checkPhotoXMPURLSidecarPairingAndClaim(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPURLSidecarPairingAndClaim(t, newPostgresQueryStore) }},
		{"PhotoXMPReceiptLimit", func(t *testing.T) { t.Helper(); checkPhotoXMPReceiptLimit(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPReceiptLimit(t, newPostgresQueryStore) }},
		{"PhotoXMPAuditClassification", func(t *testing.T) { t.Helper(); checkPhotoXMPAuditClassification(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPhotoXMPAuditClassification(t, newPostgresQueryStore) }},
	}},
	{name: "visual-evidence", postgres: "native operations", scope: "Preview publication, exact-version reads, image preparation and retained email PDF evidence; excludes renderer executables and photo/person workflows.", scenarios: []backendScenario{
		{"VisualEvidenceSnapshots", nil, checkVisualEvidenceSnapshotsPostgres},
		{"VisualPreviewPublicationRollback", nil, checkVisualPreviewPublicationRollbackPostgres},
		{"PreviewBatchRecipes", func(t *testing.T) { t.Helper(); checkPreviewBatchRecipes(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPreviewBatchRecipes(t, newPostgresQueryStore) }},
		{"FaceCandidateCatalogReads", func(t *testing.T) { t.Helper(); checkFaceCandidateCatalogReads(t, false) }, func(t *testing.T) { t.Helper(); checkFaceCandidateCatalogReads(t, true) }},
		{"FaceMemberGridPreviews", func(t *testing.T) { t.Helper(); checkFaceMemberGridPreviews(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkFaceMemberGridPreviews(t, newPostgresQueryStore) }},
		{"MissingPhotoVisualPreviewTargetsAfter", func(t *testing.T) { t.Helper(); checkMissingPhotoVisualPreviewTargetsAfter(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMissingPhotoVisualPreviewTargetsAfter(t, newPostgresQueryStore) }},
		{"VisualPreviewPublicationIsExactVersionAndIdempotent", func(t *testing.T) {
			t.Helper()
			checkVisualPreviewPublicationIsExactVersionAndIdempotent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewPublicationIsExactVersionAndIdempotent(t, newPostgresQueryStore)
		}},
		{"VisualPreviewHeadAdvancesOnFirstRecordingAndHoldsOnRepublication", func(t *testing.T) {
			t.Helper()
			checkVisualPreviewHeadAdvancesOnFirstRecordingAndHoldsOnRepublication(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewHeadAdvancesOnFirstRecordingAndHoldsOnRepublication(t, newPostgresQueryStore)
		}},
		{"VisualPreviewHeadAppearsWhenMissingForRecordedGeneration", func(t *testing.T) {
			t.Helper()
			checkVisualPreviewHeadAppearsWhenMissingForRecordedGeneration(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewHeadAppearsWhenMissingForRecordedGeneration(t, newPostgresQueryStore)
		}},
		{"VisualPreviewTerminalHeadsHoldOnRepublication", func(t *testing.T) { t.Helper(); checkVisualPreviewTerminalHeadsHoldOnRepublication(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewTerminalHeadsHoldOnRepublication(t, newPostgresQueryStore)
		}},
		{"VisualPreviewRecordsDeterministicFailureWithoutBlobAuthority", func(t *testing.T) {
			t.Helper()
			checkVisualPreviewRecordsDeterministicFailureWithoutBlobAuthority(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewRecordsDeterministicFailureWithoutBlobAuthority(t, newPostgresQueryStore)
		}},
		{"VisualPreviewLifecycleFollowsExactContentVersion", func(t *testing.T) { t.Helper(); checkVisualPreviewLifecycleFollowsExactContentVersion(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewLifecycleFollowsExactContentVersion(t, newPostgresQueryStore)
		}},
		{"VisualPreviewGenerationByRecipe", func(t *testing.T) { t.Helper(); checkVisualPreviewGenerationByRecipe(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkVisualPreviewGenerationByRecipe(t, newPostgresQueryStore) }},
		{"VisualPreviewNonActiveRecipesKeepLargeHead", func(t *testing.T) { t.Helper(); checkVisualPreviewNonActiveRecipesKeepLargeHead(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkVisualPreviewNonActiveRecipesKeepLargeHead(t, newPostgresQueryStore)
		}},
		{"EmbeddingReconciliationRestoresPendingImageGenerationInMixedProfile", func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationRestoresPendingImageGenerationInMixedProfile(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationRestoresPendingImageGenerationInMixedProfile(t, newPostgresQueryStore)
		}},
		{"EmbeddingReconciliationPreparesEachImageOnce", func(t *testing.T) { t.Helper(); checkEmbeddingReconciliationPreparesEachImageOnce(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationPreparesEachImageOnce(t, newPostgresQueryStore)
		}},
		{"EmailPDFAttachmentNotifiesOnce", func(t *testing.T) { t.Helper(); checkEmailPDFAttachmentNotifiesOnce(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmailPDFAttachmentNotifiesOnce(t, newPostgresQueryStore) }},
	}},
	{name: "text-extraction", postgres: "native operations", scope: "Plain-text queue discovery, retries, results and rendition authority publication; excludes ranked search and recovery.", scenarios: []backendScenario{
		{"PDFTextDiscovery", func(t *testing.T) { t.Helper(); checkPDFTextDiscovery(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPDFTextDiscovery(t, newPostgresQueryStore) }},
		{"TextPublication", nil, checkTextPublicationPostgres},
		{"TextChangedResult", nil, checkTextChangedResultPostgres},
		{"TextPublicationRollback", nil, checkTextPublicationRollbackPostgres},
		{"TextPurgeScopes", nil, checkTextPurgeScopesPostgres},
		{"PendingAndFailedTextExtractions", func(t *testing.T) { t.Helper(); checkPendingAndFailedTextExtractions(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPendingAndFailedTextExtractions(t, newPostgresQueryStore) }},
		{"RecordExtractionNotifiesCurrentLiveNodesOnce", func(t *testing.T) { t.Helper(); checkRecordExtractionNotifiesCurrentLiveNodesOnce(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRecordExtractionNotifiesCurrentLiveNodesOnce(t, newPostgresQueryStore)
		}},
		{"RecordExtractionRetiresFailureAfterLegacyMigration", func(t *testing.T) {
			t.Helper()
			checkRecordExtractionRetiresFailureAfterLegacyMigration(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRecordExtractionRetiresFailureAfterLegacyMigration(t, newPostgresQueryStore)
		}},
		{"PendingTextExtractionsSkipsSupersededQueuedContent", func(t *testing.T) {
			t.Helper()
			checkPendingTextExtractionsSkipsSupersededQueuedContent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPendingTextExtractionsSkipsSupersededQueuedContent(t, newPostgresQueryStore)
		}},
		{"TextExtractionQueueDefersFailuresBehindReadyWork", func(t *testing.T) { t.Helper(); checkTextExtractionQueueDefersFailuresBehindReadyWork(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkTextExtractionQueueDefersFailuresBehindReadyWork(t, newPostgresQueryStore)
		}},
		{"MigrateLegacyPlainTextRetryPreservesQueuedWork", func(t *testing.T) { t.Helper(); checkMigrateLegacyPlainTextRetryPreservesQueuedWork(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMigrateLegacyPlainTextRetryPreservesQueuedWork(t, newPostgresQueryStore)
		}},
	}},
	{name: "pages", postgres: "native operations", scope: "Native page job replay, claims, cancellation, frames and image publication; excludes rendering processes and recovery.", scenarios: []backendScenario{
		{"PagePublicationRollback", nil, checkPagePublicationRollbackPostgres},
		{"EmailPageSnapshots", nil, checkEmailPageSnapshotsPostgres},
		{"PagePublicationFencesClaimsAndPreservesPartialAuthority", func(t *testing.T) {
			t.Helper()
			checkPagePublicationFencesClaimsAndPreservesPartialAuthority(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPagePublicationFencesClaimsAndPreservesPartialAuthority(t, newPostgresQueryStore)
		}},
		{"PageJobRestartCannotPublishAnOldClaim", func(t *testing.T) { t.Helper(); checkPageJobRestartCannotPublishAnOldClaim(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPageJobRestartCannotPublishAnOldClaim(t, newPostgresQueryStore) }},
		{"PageJobNewOperationRetriesFailedAndCanceledRequests", func(t *testing.T) {
			t.Helper()
			checkPageJobNewOperationRetriesFailedAndCanceledRequests(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPageJobNewOperationRetriesFailedAndCanceledRequests(t, newPostgresQueryStore)
		}},
		{"PageJobStaleCompletionRetiresClaim", func(t *testing.T) { t.Helper(); checkPageJobStaleCompletionRetiresClaim(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPageJobStaleCompletionRetiresClaim(t, newPostgresQueryStore) }},
		{"PageJobStaleCompletionRetiresOnlyItsOwnedClaim", func(t *testing.T) { t.Helper(); checkPageJobStaleCompletionRetiresOnlyItsOwnedClaim(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPageJobStaleCompletionRetiresOnlyItsOwnedClaim(t, newPostgresQueryStore)
		}},
		{"PageJobsConcurrentIdentityLimitsAndSourceFences", func(t *testing.T) { t.Helper(); checkPageJobsConcurrentIdentityLimitsAndSourceFences(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPageJobsConcurrentIdentityLimitsAndSourceFences(t, newPostgresQueryStore)
		}},
	}},
	{name: "email", postgres: "native operations", scope: "Native MIME inventories, body renditions, child documents, retention and relation reads; excludes plain-text extraction and recovery.", scenarios: []backendScenario{
		{"EmailPublicationRollback", nil, checkEmailPublicationRollbackPostgres},
		{"EmailBodyPublication", func(t *testing.T) { t.Helper(); checkEmailBodyPublication(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmailBodyPublication(t, newPostgresQueryStore) }},

		{"EmailDocumentsProcessingReportsServingRenditionOutcomes", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsProcessingReportsServingRenditionOutcomes(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsProcessingReportsServingRenditionOutcomes(t, newPostgresQueryStore)
		}},

		{"EmailPublicationSharesGenerationAndRetiresStaging", func(t *testing.T) {
			t.Helper()
			checkEmailPublicationSharesGenerationAndRetiresStaging(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailPublicationSharesGenerationAndRetiresStaging(t, newPostgresQueryStore)
		}},
		{"EmailSupersededBeforeBodyPublication", func(t *testing.T) { t.Helper(); checkEmailSupersededBeforeBodyPublication(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmailSupersededBeforeBodyPublication(t, newPostgresQueryStore) }},
		{"EmailMissingStatesAndDeterministicRefusal", func(t *testing.T) { t.Helper(); checkEmailMissingStatesAndDeterministicRefusal(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmailMissingStatesAndDeterministicRefusal(t, newPostgresQueryStore)
		}},
		{"EmailPurgeSharedGenerationAndPendingSuppression", func(t *testing.T) { t.Helper(); checkEmailPurgeSharedGenerationAndPendingSuppression(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmailPurgeSharedGenerationAndPendingSuppression(t, newPostgresQueryStore)
		}},
		{"EmailTargetsKeysetRetriesPendingBodiesAndSuppressionWins", func(t *testing.T) {
			t.Helper()
			checkEmailTargetsKeysetRetriesPendingBodiesAndSuppressionWins(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailTargetsKeysetRetriesPendingBodiesAndSuppressionWins(t, newPostgresQueryStore)
		}},
		{"EmailVersionPruneAndLastTrashLeaveOrphansForCollection", func(t *testing.T) {
			t.Helper()
			checkEmailVersionPruneAndLastTrashLeaveOrphansForCollection(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailVersionPruneAndLastTrashLeaveOrphansForCollection(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsPublicationRetryAndExactOccurrence", func(t *testing.T) { t.Helper(); checkEmailDocumentsPublicationRetryAndExactOccurrence(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsPublicationRetryAndExactOccurrence(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsAtomicStaleDestinationAndExplicitReuse", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsAtomicStaleDestinationAndExplicitReuse(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsAtomicStaleDestinationAndExplicitReuse(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsExplicitRootAttachment", func(t *testing.T) { t.Helper(); checkEmailDocumentsExplicitRootAttachment(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmailDocumentsExplicitRootAttachment(t, newPostgresQueryStore) }},
		{"EmailDocumentsDuplicateNamesBytesPartialAndAuditedCreation", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsDuplicateNamesBytesPartialAndAuditedCreation(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsDuplicateNamesBytesPartialAndAuditedCreation(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsRetainReferencesAcrossTrashReprocessPruneAndPurge", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsRetainReferencesAcrossTrashReprocessPruneAndPurge(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsRetainReferencesAcrossTrashReprocessPruneAndPurge(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsConcurrentReplayBoundedPagesAndRollback", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsConcurrentReplayBoundedPagesAndRollback(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsConcurrentReplayBoundedPagesAndRollback(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsUnavailableOutcomesNeverCreateEmptyChildren", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsUnavailableOutcomesNeverCreateEmptyChildren(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsUnavailableOutcomesNeverCreateEmptyChildren(t, newPostgresQueryStore)
		}},
		{"EmailDocumentsBulkPurgeSkipsRetainedInventory", func(t *testing.T) { t.Helper(); checkEmailDocumentsBulkPurgeSkipsRetainedInventory(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsBulkPurgeSkipsRetainedInventory(t, newPostgresQueryStore)
		}},

		{"EmailDocumentsRepublishReleasedReceiptReportsConflict", func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsRepublishReleasedReceiptReportsConflict(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmailDocumentsRepublishReleasedReceiptReportsConflict(t, newPostgresQueryStore)
		}},
	}},
	{name: "media", postgres: "native operations", scope: "Media source retention, input artifacts, occurrence visibility and operation receipts; excludes provider execution.", scenarios: []backendScenario{
		{"MediaConsentAndContinuations", func(t *testing.T) { t.Helper(); checkMediaConsentAndContinuations(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkMediaConsentAndContinuations(t, newPostgresQueryStore) }},
		{"MediaPublicationRollback", nil, checkMediaPublicationRollbackPostgres},
		{"MediaSnapshots", nil, checkMediaSnapshotsPostgres},
		{"MediaRevocationRemovesInputBoundRendition", func(t *testing.T) { t.Helper(); checkMediaRevocationRemovesInputBoundRendition(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMediaRevocationRemovesInputBoundRendition(t, newPostgresQueryStore)
		}},
		{"MediaIdentityConflicts", nil, checkMediaIdentityConflictsPostgres},
		{"RetainSuppliedMediaReusesCallerRevisionAcrossOperations", func(t *testing.T) {
			t.Helper()
			checkRetainSuppliedMediaReusesCallerRevisionAcrossOperations(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRetainSuppliedMediaReusesCallerRevisionAcrossOperations(t, newPostgresQueryStore)
		}},
		{"ImportMediaInputArtifactRejectsChangedImmutableAuthority", func(t *testing.T) {
			t.Helper()
			checkImportMediaInputArtifactRejectsChangedImmutableAuthority(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkImportMediaInputArtifactRejectsChangedImmutableAuthority(t, newPostgresQueryStore)
		}},
		{"SuppliedInputSelectorsFilterKind", func(t *testing.T) { t.Helper(); checkSuppliedInputSelectorsFilterKind(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkSuppliedInputSelectorsFilterKind(t, newPostgresQueryStore) }},
		{"MediaSourcesPageSelectsNewestVisibleOccurrences", func(t *testing.T) { t.Helper(); checkMediaSourcesPageSelectsNewestVisibleOccurrences(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMediaSourcesPageSelectsNewestVisibleOccurrences(t, newPostgresQueryStore)
		}},
		{"MediaSourceVersionSelectsTheExactVisibleRevision", func(t *testing.T) { t.Helper(); checkMediaSourceVersionSelectsTheExactVisibleRevision(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMediaSourceVersionSelectsTheExactVisibleRevision(t, newPostgresQueryStore)
		}},
		{"RetainRemoteRecordingMedia", func(t *testing.T) { t.Helper(); checkRetainRemoteRecordingMedia(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkRetainRemoteRecordingMedia(t, newPostgresQueryStore) }},
		{"MediaRevokeIsPrincipalScopedAndIdempotent", func(t *testing.T) { t.Helper(); checkMediaRevokeIsPrincipalScopedAndIdempotent(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMediaRevokeIsPrincipalScopedAndIdempotent(t, newPostgresQueryStore)
		}},
		{"MediaOccurrenceRejectsChangedClaimsAndKeepsOtherPrincipalVisible", func(t *testing.T) {
			t.Helper()
			checkMediaOccurrenceRejectsChangedClaimsAndKeepsOtherPrincipalVisible(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMediaOccurrenceRejectsChangedClaimsAndKeepsOtherPrincipalVisible(t, newPostgresQueryStore)
		}},
		{"MediaOccurrenceConcurrentDeclarationCreatesOneRevision", func(t *testing.T) {
			t.Helper()
			checkMediaOccurrenceConcurrentDeclarationCreatesOneRevision(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMediaOccurrenceConcurrentDeclarationCreatesOneRevision(t, newPostgresQueryStore)
		}},
		{"PublishMediaSourceVersionBindsOnlyUnboundOccurrences", func(t *testing.T) {
			t.Helper()
			checkPublishMediaSourceVersionBindsOnlyUnboundOccurrences(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishMediaSourceVersionBindsOnlyUnboundOccurrences(t, newPostgresQueryStore)
		}},
		{"MediaOperationReplaysWithoutRepeatingMutation", func(t *testing.T) { t.Helper(); checkMediaOperationReplaysWithoutRepeatingMutation(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMediaOperationReplaysWithoutRepeatingMutation(t, newPostgresQueryStore)
		}},
		{"MediaOperationConcurrentReplayRunsMutationOnce", func(t *testing.T) { t.Helper(); checkMediaOperationConcurrentReplayRunsMutationOnce(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkMediaOperationConcurrentReplayRunsMutationOnce(t, newPostgresQueryStore)
		}},
		{"RemoteRecordingReceiptScopesPrincipalAndVerb", func(t *testing.T) { t.Helper(); checkRemoteRecordingReceiptScopesPrincipalAndVerb(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRemoteRecordingReceiptScopesPrincipalAndVerb(t, newPostgresQueryStore)
		}},
	}},
	{name: "embeddings", postgres: "native operations", scope: "Document embedding catalogs, job claims, consent, reconciliation and publication; includes native image preparation; excludes ranked search.", scenarios: []backendScenario{
		{"EmbeddingWriteRevision", func(t *testing.T) { t.Helper(); checkEmbeddingWriteRevision(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmbeddingWriteRevision(t, newPostgresQueryStore) }},
		{"EmbeddingAttachmentPurgeLeavesDirectBindingPublishable", func(t *testing.T) {
			t.Helper()
			checkEmbeddingAttachmentPurgeLeavesDirectBindingPublishable(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingAttachmentPurgeLeavesDirectBindingPublishable(t, newPostgresQueryStore)
		}},
		{"EmbeddingPurgeFencesWorkBeforeStaging", func(t *testing.T) { t.Helper(); checkEmbeddingPurgeFencesWorkBeforeStaging(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmbeddingPurgeFencesWorkBeforeStaging(t, newPostgresQueryStore) }},
		{"EmbeddingPurgeFencesUnstagedChunkBindings", func(t *testing.T) { t.Helper(); checkEmbeddingPurgeFencesUnstagedChunkBindings(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingPurgeFencesUnstagedChunkBindings(t, newPostgresQueryStore)
		}},
		{"EmbeddingHistoricalRenditionPurgePreservesCurrentBinding", func(t *testing.T) {
			t.Helper()
			checkEmbeddingHistoricalRenditionPurgePreservesCurrentBinding(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingHistoricalRenditionPurgePreservesCurrentBinding(t, newPostgresQueryStore)
		}},
		{"EmbeddingHistoricalPurgeRemovesPendingChunkJobs", func(t *testing.T) { t.Helper(); checkEmbeddingHistoricalPurgeRemovesPendingChunkJobs(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingHistoricalPurgeRemovesPendingChunkJobs(t, newPostgresQueryStore)
		}},
		{"EmbeddingCatalogHeadsAndFailuresAreProfileScoped", func(t *testing.T) { t.Helper(); checkEmbeddingCatalogHeadsAndFailuresAreProfileScoped(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingCatalogHeadsAndFailuresAreProfileScoped(t, newPostgresQueryStore)
		}},
		{"EmbeddingLegacyProfilesReuseOnlyMatchingProducer", func(t *testing.T) { t.Helper(); checkEmbeddingLegacyProfilesReuseOnlyMatchingProducer(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingLegacyProfilesReuseOnlyMatchingProducer(t, newPostgresQueryStore)
		}},
		{"EmbeddingPublication", nil, checkEmbeddingPublicationPostgres},
		{"ImageEmbeddingPublication", nil, checkImageEmbeddingPublicationPostgres},
		{"EmbeddingCoverageSnapshot", nil, checkEmbeddingCoverageSnapshotPostgres},
		{"ProcessingCoverageReportsRebuildWhilePreviousGenerationServes", func(t *testing.T) {
			t.Helper()
			checkProcessingCoverageReportsRebuildWhilePreviousGenerationServes(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkProcessingCoverageReportsRebuildWhilePreviousGenerationServes(t, newPostgresQueryStore)
		}},
		{"ProcessingCoverageEmbeddingReplacementKeepsServingHead", func(t *testing.T) {
			t.Helper()
			checkProcessingCoverageEmbeddingReplacementKeepsServingHead(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkProcessingCoverageEmbeddingReplacementKeepsServingHead(t, newPostgresQueryStore)
		}},
		{"ProcessingCoverageSharedBuildKeepsVersionStates", func(t *testing.T) { t.Helper(); checkProcessingCoverageSharedBuildKeepsVersionStates(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkProcessingCoverageSharedBuildKeepsVersionStates(t, newPostgresQueryStore)
		}},
		{"EmbeddingCatalogDeduplicatesExactSetsButFencesAttachmentContext", func(t *testing.T) {
			t.Helper()
			checkEmbeddingCatalogDeduplicatesExactSetsButFencesAttachmentContext(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingCatalogDeduplicatesExactSetsButFencesAttachmentContext(t, newPostgresQueryStore)
		}},
		{"EmbeddingCatalogLeasedStageAndPublishAreAtomicallyFenced", func(t *testing.T) {
			t.Helper()
			checkEmbeddingCatalogLeasedStageAndPublishAreAtomicallyFenced(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingCatalogLeasedStageAndPublishAreAtomicallyFenced(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobCatalogClaimsExactRequestedJob", func(t *testing.T) { t.Helper(); checkEmbeddingJobCatalogClaimsExactRequestedJob(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobCatalogClaimsExactRequestedJob(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobReconciliationRequiresCurrentAuthorityAndFreshConsent", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReconciliationRequiresCurrentAuthorityAndFreshConsent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReconciliationRequiresCurrentAuthorityAndFreshConsent(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobReconciliationRequiresExactChunkBindingPolicy", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReconciliationRequiresExactChunkBindingPolicy(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReconciliationRequiresExactChunkBindingPolicy(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobLeaseCannotRenewAfterExpiry", func(t *testing.T) { t.Helper(); checkEmbeddingJobLeaseCannotRenewAfterExpiry(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmbeddingJobLeaseCannotRenewAfterExpiry(t, newPostgresQueryStore) }},
		{"EmbeddingJobProviderUnavailableRetryBudgetIsDurable", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobProviderUnavailableRetryBudgetIsDurable(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobProviderUnavailableRetryBudgetIsDurable(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobExistingStaleHeadDoesNotSuppressReplacement", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobExistingStaleHeadDoesNotSuppressReplacement(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobExistingStaleHeadDoesNotSuppressReplacement(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobsRetainQueuedInputsUntilExplicitPurge", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobsRetainQueuedInputsUntilExplicitPurge(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobsRetainQueuedInputsUntilExplicitPurge(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobReleasePreservesSuccessorClaim", func(t *testing.T) { t.Helper(); checkEmbeddingJobReleasePreservesSuccessorClaim(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReleasePreservesSuccessorClaim(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobAbandonmentPreservesSuccessorClaim", func(t *testing.T) { t.Helper(); checkEmbeddingJobAbandonmentPreservesSuccessorClaim(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobAbandonmentPreservesSuccessorClaim(t, newPostgresQueryStore)
		}},
		{"EmbeddingEgressKeepsOriginalConsentAcrossBatches", func(t *testing.T) { t.Helper(); checkEmbeddingEgressKeepsOriginalConsentAcrossBatches(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingEgressKeepsOriginalConsentAcrossBatches(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobReplacementConsentCannotRebindAdmittedWork", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReplacementConsentCannotRebindAdmittedWork(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReplacementConsentCannotRebindAdmittedWork(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobReconciliationCreatesSuccessorForFreshGrant", func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReconciliationCreatesSuccessorForFreshGrant(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobReconciliationCreatesSuccessorForFreshGrant(t, newPostgresQueryStore)
		}},
		{"EmbeddingGCReleasesTerminalJobArtifacts", func(t *testing.T) { t.Helper(); checkEmbeddingGCReleasesTerminalJobArtifacts(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmbeddingGCReleasesTerminalJobArtifacts(t, newPostgresQueryStore) }},
		{"EmbeddingJobEnqueueRejectsStaleSource", func(t *testing.T) { t.Helper(); checkEmbeddingJobEnqueueRejectsStaleSource(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmbeddingJobEnqueueRejectsStaleSource(t, newPostgresQueryStore) }},
		{"EmbeddingJobsResumeAfterSourceRestoration", func(t *testing.T) { t.Helper(); checkEmbeddingJobsResumeAfterSourceRestoration(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingJobsResumeAfterSourceRestoration(t, newPostgresQueryStore)
		}},
		{"EmbeddingReconciliationSkipsSuppressedRetainedGeneration", func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationSkipsSuppressedRetainedGeneration(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationSkipsSuppressedRetainedGeneration(t, newPostgresQueryStore)
		}},
		{"EmbeddingReconciliationSkipsExistingTerminalJob", func(t *testing.T) { t.Helper(); checkEmbeddingReconciliationSkipsExistingTerminalJob(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationSkipsExistingTerminalJob(t, newPostgresQueryStore)
		}},
		{"EmbeddingReconciliationAdvancesPastUnreadableGeneration", func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationAdvancesPastUnreadableGeneration(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEmbeddingReconciliationAdvancesPastUnreadableGeneration(t, newPostgresQueryStore)
		}},
		{"EmbeddingJobExpiredClaimsExhaustBudget", func(t *testing.T) { t.Helper(); checkEmbeddingJobExpiredClaimsExhaustBudget(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmbeddingJobExpiredClaimsExhaustBudget(t, newPostgresQueryStore) }},
	}},
	{name: "renditions", postgres: "native operations", scope: "Original-file consent, jobs, leases, staging, publication and exact text readback; excludes derived-photo jobs and search.", scenarios: []backendScenario{
		{"RenditionRetryErrors", nil, checkRenditionRetryErrorsPostgres},
		{"RenditionConnectionFailure", nil, checkRenditionConnectionFailurePostgres},
		{"RenditionArtifactStaging", func(t *testing.T) { t.Helper(); checkRenditionArtifactStaging(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkRenditionArtifactStaging(t, newPostgresQueryStore) }},
		{"RenditionReplacementEmbeddings", nil, checkRenditionReplacementEmbeddingsPostgres},
		{"RenditionSnapshots", nil, checkRenditionSnapshotsPostgres},
		{"RenditionPublicationRollback", nil, checkRenditionPublicationRollbackPostgres},
		{"RenditionWorkerResume", func(t *testing.T) { t.Helper(); checkRenditionWorkerResume(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkRenditionWorkerResume(t, newPostgresQueryStore) }},

		{"ProcessingConsentAuthorizesOnlyExactCurrentGrant", func(t *testing.T) { t.Helper(); checkProcessingConsentAuthorizesOnlyExactCurrentGrant(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkProcessingConsentAuthorizesOnlyExactCurrentGrant(t, newPostgresQueryStore)
		}},
		{"ProcessingConsentSetAuthorizesItsExactSiblingGrant", func(t *testing.T) {
			t.Helper()
			checkProcessingConsentSetAuthorizesItsExactSiblingGrant(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkProcessingConsentSetAuthorizesItsExactSiblingGrant(t, newPostgresQueryStore)
		}},
		{"ProcessingConsentRejectsExpiredAndDriftedOperations", func(t *testing.T) {
			t.Helper()
			checkProcessingConsentRejectsExpiredAndDriftedOperations(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkProcessingConsentRejectsExpiredAndDriftedOperations(t, newPostgresQueryStore)
		}},
		{"ProcessingConsentRevocationFencesSubmissionAndPublication", func(t *testing.T) {
			t.Helper()
			checkProcessingConsentRevocationFencesSubmissionAndPublication(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkProcessingConsentRevocationFencesSubmissionAndPublication(t, newPostgresQueryStore)
		}},
		{"ProcessingConsentRevocationIsCheckedInPublicationTransaction", func(t *testing.T) {
			t.Helper()
			checkProcessingConsentRevocationIsCheckedInPublicationTransaction(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkProcessingConsentRevocationIsCheckedInPublicationTransaction(t, newPostgresQueryStore)
		}},
		{"RenditionJobsDeduplicateSharedBuildAndFenceLeaseTheft", func(t *testing.T) {
			t.Helper()
			checkRenditionJobsDeduplicateSharedBuildAndFenceLeaseTheft(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobsDeduplicateSharedBuildAndFenceLeaseTheft(t, newPostgresQueryStore)
		}},
		{"RenditionJobWaiterCannotAdoptReplacementConsent", func(t *testing.T) { t.Helper(); checkRenditionJobWaiterCannotAdoptReplacementConsent(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRenditionJobWaiterCannotAdoptReplacementConsent(t, newPostgresQueryStore)
		}},
		{"RenditionJobsSeparateDifferentExecutionIdentities", func(t *testing.T) {
			t.Helper()
			checkRenditionJobsSeparateDifferentExecutionIdentities(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobsSeparateDifferentExecutionIdentities(t, newPostgresQueryStore)
		}},
		{"RenditionJobsUseCapturedPolicyToSeparateRetentionOutputs", func(t *testing.T) {
			t.Helper()
			checkRenditionJobsUseCapturedPolicyToSeparateRetentionOutputs(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobsUseCapturedPolicyToSeparateRetentionOutputs(t, newPostgresQueryStore)
		}},
		{"EnqueueRenditionJobRejectsFilenameDisclosureIdentityDrift", func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobRejectsFilenameDisclosureIdentityDrift(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobRejectsFilenameDisclosureIdentityDrift(t, newPostgresQueryStore)
		}},
		{"EnqueueRenditionJobReusesAndRootsExistingSharedBuild", func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobReusesAndRootsExistingSharedBuild(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobReusesAndRootsExistingSharedBuild(t, newPostgresQueryStore)
		}},
		{"EnqueueCompletedRenditionJobRestoresSupersededHeadWithoutProviderEgress", func(t *testing.T) {
			t.Helper()
			checkEnqueueCompletedRenditionJobRestoresSupersededHeadWithoutProviderEgress(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEnqueueCompletedRenditionJobRestoresSupersededHeadWithoutProviderEgress(t, newPostgresQueryStore)
		}},
		{"PurgeDerivativesCancelsQueuedRenditionJobAndSuppressesReenqueue", func(t *testing.T) {
			t.Helper()
			checkPurgeDerivativesCancelsQueuedRenditionJobAndSuppressesReenqueue(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPurgeDerivativesCancelsQueuedRenditionJobAndSuppressesReenqueue(t, newPostgresQueryStore)
		}},
		{"EnqueueRenditionJobRejectsNewWaiterForTerminalBuild", func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobRejectsNewWaiterForTerminalBuild(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobRejectsNewWaiterForTerminalBuild(t, newPostgresQueryStore)
		}},
		{"EnqueueRenditionJobRejectsUnauthorizedWaiter", func(t *testing.T) { t.Helper(); checkEnqueueRenditionJobRejectsUnauthorizedWaiter(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkEnqueueRenditionJobRejectsUnauthorizedWaiter(t, newPostgresQueryStore)
		}},
		{"RenditionJobWorkChoosesAnAuthorizedWaiter", func(t *testing.T) { t.Helper(); checkRenditionJobWorkChoosesAnAuthorizedWaiter(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRenditionJobWorkChoosesAnAuthorizedWaiter(t, newPostgresQueryStore)
		}},
		{"RenditionJobsRecoverAmbiguousProviderWork", func(t *testing.T) {
			t.Helper()
			checkRenditionJobsRecoverAmbiguousProviderWork(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobsRecoverAmbiguousProviderWork(t, newPostgresQueryStore)
		}},
		{"RenditionJobNoHandleRetryKeepsAdmissionAuthority", func(t *testing.T) { t.Helper(); checkRenditionJobNoHandleRetryKeepsAdmissionAuthority(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRenditionJobNoHandleRetryKeepsAdmissionAuthority(t, newPostgresQueryStore)
		}},
		{"ReopenableStagedFailureRetainsBuildAcrossDerivativeSweep", func(t *testing.T) {
			t.Helper()
			checkReopenableStagedFailureRetainsBuildAcrossDerivativeSweep(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkReopenableStagedFailureRetainsBuildAcrossDerivativeSweep(t, newPostgresQueryStore)
		}},
		{"RenditionJobStagingIsIdempotentWithinOneClaim", func(t *testing.T) { t.Helper(); checkRenditionJobStagingIsIdempotentWithinOneClaim(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRenditionJobStagingIsIdempotentWithinOneClaim(t, newPostgresQueryStore)
		}},
		{"RenditionPublicationPreservesLegacyText", func(t *testing.T) { t.Helper(); checkRenditionPublicationPreservesLegacyText(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkRenditionPublicationPreservesLegacyText(t, newPostgresQueryStore) }},
		{"PublishRenditionJobAtomicallyActivatesAuthorizedWaiters", func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobAtomicallyActivatesAuthorizedWaiters(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobAtomicallyActivatesAuthorizedWaiters(t, newPostgresQueryStore)
		}},
		{"PublishRenditionJobFencesLeaseTheft", func(t *testing.T) { t.Helper(); checkPublishRenditionJobFencesLeaseTheft(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPublishRenditionJobFencesLeaseTheft(t, newPostgresQueryStore) }},
		{"RenditionJobPublishesCurrentLexicalGenerationWithoutRepeatingEgress", func(t *testing.T) {
			t.Helper()
			checkRenditionJobPublishesCurrentLexicalGenerationWithoutRepeatingEgress(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobPublishesCurrentLexicalGenerationWithoutRepeatingEgress(t, newPostgresQueryStore)
		}},
		{"PublishRenditionJobRejectsRevokedEgressAndPreservesPreviousHead", func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobRejectsRevokedEgressAndPreservesPreviousHead(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobRejectsRevokedEgressAndPreservesPreviousHead(t, newPostgresQueryStore)
		}},
		{"PublishRenditionJobAllowsDegradedActivationForRevokedWaiter", func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobAllowsDegradedActivationForRevokedWaiter(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobAllowsDegradedActivationForRevokedWaiter(t, newPostgresQueryStore)
		}},
		{"RenditionJobFencesDisclosedFilenameAfterRename", func(t *testing.T) { t.Helper(); checkRenditionJobFencesDisclosedFilenameAfterRename(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkRenditionJobFencesDisclosedFilenameAfterRename(t, newPostgresQueryStore)
		}},
		{"PublishRenditionJobPersistsRejectionsOnFailure", func(t *testing.T) { t.Helper(); checkPublishRenditionJobPersistsRejectionsOnFailure(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobPersistsRejectionsOnFailure(t, newPostgresQueryStore)
		}},
		{"ResolveRenditionTextOmissionOnlyPDF", func(t *testing.T) { t.Helper(); checkResolveRenditionTextOmissionOnlyPDF(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkResolveRenditionTextOmissionOnlyPDF(t, newPostgresQueryStore) }},
		{"EmptyPDFChunkSettlement", func(t *testing.T) { t.Helper(); checkEmptyPDFChunkSettlement(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkEmptyPDFChunkSettlement(t, newPostgresQueryStore) }},
		{"ResolveRenditionTextReportsExactFailedAttempts", func(t *testing.T) { t.Helper(); checkResolveRenditionTextReportsExactFailedAttempts(t, newTestStore) }, func(t *testing.T) {
			t.Helper()
			checkResolveRenditionTextReportsExactFailedAttempts(t, newPostgresQueryStore)
		}},
		{"ResolveRenditionTextBindsLiveNodeSourceHeadAndGeneration", func(t *testing.T) {
			t.Helper()
			checkResolveRenditionTextBindsLiveNodeSourceHeadAndGeneration(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkResolveRenditionTextBindsLiveNodeSourceHeadAndGeneration(t, newPostgresQueryStore)
		}},
	}},
	{name: "schema", postgres: "installed catalog comparison", scope: "Logical columns, keys and indexes; explicit engine differences.", scenarios: []backendScenario{{"Schema", nil, checkBackendSchemaPostgres}}},
	{name: "tree", postgres: "native operations", scope: "Node projections, directory pages, paths, names and snapshots.", scenarios: []backendScenario{
		{"NodeQueries", checkNodeQueriesSQLite, checkNodeQueriesPostgres},
	}},
	{name: "history", postgres: "native operations", scope: "Content-version reads and repeatable-read snapshots.", scenarios: []backendScenario{
		{"ContentVersionQueries", checkContentVersionQueriesSQLite, checkContentVersionQueriesPostgres},
		{"ContentVersionSnapshot", nil, checkContentVersionSnapshotPostgres},
	}},
	{name: "checksums", postgres: "native operations", scope: "Checksum recording and immutable readback.", scenarios: []backendScenario{
		{"ChecksumRecording", checkChecksumRecordingSQLite, checkChecksumRecordingPostgres},
	}},
	{name: "transactions", postgres: "native operations", scope: "Commit, rollback, cancellation, notifications, writer admission and schema isolation.", scenarios: []backendScenario{
		{"StorageTransactions", checkStorageTransactionsSQLite, checkStorageTransactionsPostgres},
		{"StorageWriterAdmission", nil, checkStorageWriterAdmissionPostgres},
		{"StorageWritersSeparateSchemas", nil, checkStorageWritersSeparateSchemasPostgres},
	}},
	{name: "audit", postgres: "native operations", scope: "Canonical records, status, history, head advancement and transactional node allocation.", scenarios: []backendScenario{
		{"AuditTrashOriginMove", func(t *testing.T) {
			t.Helper()
			checkAuditTrashOriginMove(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkAuditTrashOriginMove(t, newPostgresQueryStore)
		}},
		{"AuditHistory", nil, checkAuditHistoryPostgres},
		{"AuditHistorySnapshot", nil, checkAuditHistorySnapshotPostgres},
		{"AuditRecords", nil, checkAuditRecordsPostgres},
		{"AuditStatus", nil, checkAuditStatusPostgres},
		{"AuditStatusSnapshot", nil, checkAuditStatusSnapshotPostgres},
		{"NodeAllocationAfterDelete", nil, checkNodeAllocationAfterDeletePostgres},
		{"AuditAuthorityAfterRollback", nil, checkAuditAuthorityAfterRollbackPostgres},
		{"AuditAdvancement", nil, checkAuditAdvancementPostgres},
	}},
	{name: "enrollment", postgres: "native operations", scope: "Native initial/additional enrollment; copied retained catalogs qualify validation, not their producers.", scenarios: []backendScenario{
		{"AuditEnrollmentPhotos", nil, checkAuditEnrollmentPhotosPostgres},
		{"AuditEnrollmentCatalogs", nil, checkAuditEnrollmentCatalogsPostgres},
		{"AuditEnrollmentFresh", nil, checkAuditEnrollmentFreshPostgres},
		{"AuditEnrollmentChangedPreview", nil, checkAuditEnrollmentChangedPreviewPostgres},
		{"AuditEnrollmentPhotoReceipt", nil, checkAuditEnrollmentPhotoReceiptPostgres},
	}},
	{name: "documents", postgres: "native operations", scope: "Ordinary and audited creation, replacement, revert and moves; physical and photo effects.", scenarios: []backendScenario{
		{"DocumentWrites", func(t *testing.T) {
			t.Helper()
			checkDocumentWrites(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentWrites(t, newPostgresQueryStore)
		}},
		{"DocumentNegativeSize", nil, checkDocumentNegativeSizePostgres},
		{"DocumentPhotoMaintenance", nil, checkDocumentPhotoMaintenancePostgres},
		{"DocumentSidecarWork", nil, checkDocumentSidecarWorkPostgres},
		{"DocumentTextSuppression", nil, checkDocumentTextSuppressionPostgres},
		{"DocumentBlobPromotion", nil, checkDocumentBlobPromotionPostgres},
		{"DocumentReintroducedPackMappings", nil, checkDocumentReintroducedPackMappingsPostgres},
	}},
	{name: "imports", postgres: "native operations", scope: "Native document ingest, plans and watch/push observations; excludes grouped photo import.", scenarios: []backendScenario{
		{"FindUploadedFile", func(t *testing.T) { t.Helper(); checkFindUploadedFile(t, newTestStore(t)) }, func(t *testing.T) { t.Helper(); checkFindUploadedFile(t, newPostgresQueryStore(t)) }},
		{"IngestOperations", nil, checkIngestOperationsPostgres},
		{"PlannedIngest", nil, checkPlannedIngestPostgres},
		{"WatchAndPush", nil, checkWatchAndPushPostgres},
	}},
	{name: "sources", postgres: "native operations", scope: "Provenance, collections, labels and related photo/event effects.", scenarios: []backendScenario{
		{"ProvenancePhotoEffects", nil, checkProvenancePhotoEffectsPostgres},
		{"ProvenanceInvalidation", nil, checkProvenanceInvalidationPostgres},
		{"ImportReadSnapshots", nil, checkImportReadSnapshotsPostgres},
		{"ProvenanceRequeuesPhotoExport", nil, checkProvenanceRequeuesPhotoExportPostgres},
		{"MixedCaseEmbeddedSource", checkMixedCaseEmbeddedSourceSQLite, checkMixedCaseEmbeddedSourcePostgres},
	}},
	{name: "trash", postgres: "native operations", scope: "Recoverable document/folder/photo-group deletion and restore.", scenarios: []backendScenario{
		{"TrashRestore", nil, checkTrashRestorePostgres},
		{"TrashRestoreNames", nil, checkTrashRestoreNamesPostgres},
		{"TrashPageSnapshot", nil, checkTrashPageSnapshotPostgres},
		{"PhotoTrashRestore", nil, checkPhotoTrashRestorePostgres},
	}},
	{name: "retention", postgres: "native operations", scope: "Permanent trash emptying and history pruning with retained dependencies.", scenarios: []backendScenario{
		{"RetentionCleanup", nil, checkRetentionCleanupPostgres},
		{"RetainedCleanupFixtures", nil, checkRetainedCleanupFixturesPostgres},
		{"CleanupSharedProcessing", nil, checkCleanupSharedProcessingPostgres},
		{"CleanupInvalidationAndRollback", nil, checkCleanupInvalidationAndRollbackPostgres},
		{"CleanupEmailRetention", nil, checkCleanupEmailRetentionPostgres},
		{"CleanupPhotoReceipts", nil, checkCleanupPhotoReceiptsPostgres},
		{"CleanupEmbeddingAuthority", nil, checkCleanupEmbeddingAuthorityPostgres},
		{"CleanupSidecarHistory", nil, checkCleanupSidecarHistoryPostgres},
		{"CleanupPhotoTimeline", nil, checkCleanupPhotoTimelinePostgres},
	}},
	{name: "derivatives", postgres: "native operations", scope: "Purge, retained roots, rebuild authorization, leases and rollback; excludes processing publication.", scenarios: []backendScenario{
		{"DerivativeCleanup", nil, checkDerivativeCleanupPostgres},
		{"DerivativeSharedLegacy", nil, checkDerivativeSharedLegacyPostgres},
		{"DerivativeEmbeddingRoots", nil, checkDerivativeEmbeddingRootsPostgres},
		{"DerivativeLexicalReplacement", nil, checkDerivativeLexicalReplacementPostgres},
		{"DerivativeAuditRollback", nil, checkDerivativeAuditRollbackPostgres},
		{"DerivativeEmailRetention", nil, checkDerivativeEmailRetentionPostgres},
		{"DerivativeExportRetention", nil, checkDerivativeExportRetentionPostgres},
		{"DerivativePlanSnapshot", nil, checkDerivativePlanSnapshotPostgres},
	}},
	{name: "physical-storage", postgres: "native operations", scope: "Primary pack/repack, byte readback, GC catalogs and exact cleanup receipts; excludes multi-store jobs.", scenarios: []backendScenario{
		{"PackCatalogContract", checkPackCatalogContractSQLite, checkPackCatalogContractPostgres},
		{"PhysicalStorageLifecycle", nil, checkPhysicalStorageLifecyclePostgres},
		{"PhysicalCleanupRecovery", nil, checkPhysicalCleanupRecoveryPostgres},
		{"PackSummaryMutations", nil, checkPackSummaryMutationsPostgres},
		{"PhysicalMaintenancePages", nil, checkPhysicalMaintenancePagesPostgres},
	}},
	{name: "retained-face-metadata", postgres: "retained-data validation", scope: "Validates copied face metadata; does not qualify PostgreSQL detection/publication.", scenarios: []backendScenario{
		{"PhotoFaceMetadata", nil, checkPhotoFaceMetadataPostgres},
	}},
	{name: "metadata-events", postgres: "native operations", scope: "Source metadata, photo technical projections and document-event publication, attempts and rebuilds.", scenarios: []backendScenario{
		{"SourceMetadataPhotoEffects", nil, checkSourceMetadataPhotoEffectsPostgres},
		{"MetadataEventSnapshots", nil, checkMetadataEventSnapshotsPostgres},
		{"MetadataPublicationRollback", nil, checkMetadataPublicationRollbackPostgres},
		{"SourceMetadataRepublicationSelectsRecordedGeneration", func(t *testing.T) {
			t.Helper()
			checkSourceMetadataRepublicationSelectsRecordedGeneration(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSourceMetadataRepublicationSelectsRecordedGeneration(t, newPostgresQueryStore)
		}},
		{"HistoricalSourceMetadataOmitsCurrentAttachmentFacts", func(t *testing.T) {
			t.Helper()
			checkHistoricalSourceMetadataOmitsCurrentAttachmentFacts(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkHistoricalSourceMetadataOmitsCurrentAttachmentFacts(t, newPostgresQueryStore)
		}},
		{"TrashedSourceMetadataOmitsLiveAttachmentFacts", func(t *testing.T) {
			t.Helper()
			checkTrashedSourceMetadataOmitsLiveAttachmentFacts(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTrashedSourceMetadataOmitsLiveAttachmentFacts(t, newPostgresQueryStore)
		}},
		{"NodeSourceMetadataViewKeepsAttachmentCoordinatesTogether", func(t *testing.T) {
			t.Helper()
			checkNodeSourceMetadataViewKeepsAttachmentCoordinatesTogether(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkNodeSourceMetadataViewKeepsAttachmentCoordinatesTogether(t, newPostgresQueryStore)
		}},
		{"MissingSourceMetadataTargetsAreFingerprintScopedAndResumable", func(t *testing.T) {
			t.Helper()
			checkMissingSourceMetadataTargetsAreFingerprintScopedAndResumable(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMissingSourceMetadataTargetsAreFingerprintScopedAndResumable(t, newPostgresQueryStore)
		}},
		{"MissingSourceMetadataTargetsIncludeDistinctRetainedContent", func(t *testing.T) {
			t.Helper()
			checkMissingSourceMetadataTargetsIncludeDistinctRetainedContent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMissingSourceMetadataTargetsIncludeDistinctRetainedContent(t, newPostgresQueryStore)
		}},
		{"PhotoTechnicalMetadataRepublication", func(t *testing.T) {
			t.Helper()
			checkPhotoTechnicalMetadataRepublication(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoTechnicalMetadataRepublication(t, newPostgresQueryStore)
		}},
		{"PhotoTechnicalMetadataVersionBinding", func(t *testing.T) {
			t.Helper()
			checkPhotoTechnicalMetadataVersionBinding(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPhotoTechnicalMetadataVersionBinding(t, newPostgresQueryStore)
		}},
		{"PublishDocumentEventsAcceptsCanonicalEmptyProjection", func(t *testing.T) {
			t.Helper()
			checkPublishDocumentEventsAcceptsCanonicalEmptyProjection(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishDocumentEventsAcceptsCanonicalEmptyProjection(t, newPostgresQueryStore)
		}},
		{"PublishDocumentEventsFeedDetectsOutputChanges", func(t *testing.T) {
			t.Helper()
			checkPublishDocumentEventsFeedDetectsOutputChanges(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishDocumentEventsFeedDetectsOutputChanges(t, newPostgresQueryStore)
		}},
		{"PublishDocumentEventsRetainsActorEvidenceInProjections", func(t *testing.T) {
			t.Helper()
			checkPublishDocumentEventsRetainsActorEvidenceInProjections(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishDocumentEventsRetainsActorEvidenceInProjections(t, newPostgresQueryStore)
		}},
		{"DocumentEventStateAndRecipeAreLazyAndIdempotent", func(t *testing.T) {
			t.Helper()
			checkDocumentEventStateAndRecipeAreLazyAndIdempotent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventStateAndRecipeAreLazyAndIdempotent(t, newPostgresQueryStore)
		}},
		{"MissingDocumentEventTargetsUsesEpochRevisionAndUUIDCursor", func(t *testing.T) {
			t.Helper()
			checkMissingDocumentEventTargetsUsesEpochRevisionAndUUIDCursor(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkMissingDocumentEventTargetsUsesEpochRevisionAndUUIDCursor(t, newPostgresQueryStore)
		}},
		{"DocumentEventAttemptsFenceInputsAndStopTerminalRetry", func(t *testing.T) {
			t.Helper()
			checkDocumentEventAttemptsFenceInputsAndStopTerminalRetry(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventAttemptsFenceInputsAndStopTerminalRetry(t, newPostgresQueryStore)
		}},
		{"DocumentEventRebuildReceiptIsAtomicAndIdempotent", func(t *testing.T) {
			t.Helper()
			checkDocumentEventRebuildReceiptIsAtomicAndIdempotent(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventRebuildReceiptIsAtomicAndIdempotent(t, newPostgresQueryStore)
		}},
		{"DocumentEventBuildRefreshCountsCurrentTerminalAttempts", func(t *testing.T) {
			t.Helper()
			checkDocumentEventBuildRefreshCountsCurrentTerminalAttempts(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventBuildRefreshCountsCurrentTerminalAttempts(t, newPostgresQueryStore)
		}},
		{"F10PublicationDirtiesOnlyWhenActiveHeadChanges", func(t *testing.T) {
			t.Helper()
			checkF10PublicationDirtiesOnlyWhenActiveHeadChanges(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkF10PublicationDirtiesOnlyWhenActiveHeadChanges(t, newPostgresQueryStore)
		}},
		{"DocumentEventPublicationRequiresCapturedManifestDigest", func(t *testing.T) {
			t.Helper()
			checkDocumentEventPublicationRequiresCapturedManifestDigest(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventPublicationRequiresCapturedManifestDigest(t, newPostgresQueryStore)
		}},
		{"DocumentEventCoverageReportUsesOnlyFreshCurrentFileHeads", func(t *testing.T) {
			t.Helper()
			checkDocumentEventCoverageReportUsesOnlyFreshCurrentFileHeads(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventCoverageReportUsesOnlyFreshCurrentFileHeads(t, newPostgresQueryStore)
		}},
		{"DocumentEventCoverageReportsFreshTerminalStates", func(t *testing.T) {
			t.Helper()
			checkDocumentEventCoverageReportsFreshTerminalStates(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkDocumentEventCoverageReportsFreshTerminalStates(t, newPostgresQueryStore)
		}},
		{"OlderCompleteReceiptCanCoexistWithNewerPendingCurrentCoverage", func(t *testing.T) {
			t.Helper()
			checkOlderCompleteReceiptCanCoexistWithNewerPendingCurrentCoverage(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkOlderCompleteReceiptCanCoexistWithNewerPendingCurrentCoverage(t, newPostgresQueryStore)
		}},
	}},
	{name: "processing-publication", postgres: "native rendition/PDF/supplied-media workers", scope: "internal/processing tests qualify shared-build publication, durable resume, consent/source fencing, email-PDF receipt/export and supplied-media continuation. Embedding-worker execution, remote acquisition and photo-derived workers remain unqualified."},
	{name: "photo-editing", postgres: "unqualified", scope: "Query-driven photo review remains unqualified. Explicit decisions use photo-review; people edits use people-curation; query selection stays with search."},
	{name: "search", postgres: "unqualified", scope: "Lexical/vector search and query execution remain unqualified; matching metadata columns do not establish ranking or index parity."},
	{name: "transfer-and-recovery", postgres: "native backup capture and SQLite recovery", scope: "internal/backupapp tests qualify repository capture, loose/packed bytes, audited and photo recovery, source loss and missing-byte failure. PostgreSQL-target restore remains unqualified; metadata-export and metadata-import cover native logical transfer."},
	{name: "organization", postgres: "native operations", scope: "Tags, audited bulk edits and saved definitions; excludes query execution and delivery.", scenarios: []backendScenario{
		{"BatchTagsMixedChangesNoOpsAndCallerOrder", func(t *testing.T) {
			t.Helper()
			checkBatchTagsMixedChangesNoOpsAndCallerOrder(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBatchTagsMixedChangesNoOpsAndCallerOrder(t, newPostgresQueryStore)
		}},
		{"BatchTagsAcceptsOneAndOneThousandTargets", func(t *testing.T) {
			t.Helper()
			checkBatchTagsAcceptsOneAndOneThousandTargets(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkBatchTagsAcceptsOneAndOneThousandTargets(t, newPostgresQueryStore)
		}},
		{"PreviewBatchTagsExactMembershipAndStaleFences", func(t *testing.T) {
			t.Helper()
			checkPreviewBatchTagsExactMembershipAndStaleFences(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPreviewBatchTagsExactMembershipAndStaleFences(t, newPostgresQueryStore)
		}},
		{"BatchTagSnapshot", nil, checkBatchTagSnapshotPostgres},
		{"SavedDefinitionRetainedRuns", nil, checkSavedDefinitionRetainedRunsPostgres},
		{"TagPhotoReceipts", nil, checkTagPhotoReceiptsPostgres},
		{"TagMailboxGuard", nil, checkTagMailboxGuardPostgres},
		{"TagPhotoWork", func(t *testing.T) {
			t.Helper()
			checkTagPhotoWork(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTagPhotoWork(t, newPostgresQueryStore)
		}},
		{"AuditedTagWrites", nil, checkAuditedTagWritesPostgres},
		{"TagLifecycleAndNodeRevisions", func(t *testing.T) {
			t.Helper()
			checkTagLifecycleAndNodeRevisions(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTagLifecycleAndNodeRevisions(t, newPostgresQueryStore)
		}},
		{"TagAssignmentCountsStayExact", func(t *testing.T) {
			t.Helper()
			checkTagAssignmentCountsStayExact(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTagAssignmentCountsStayExact(t, newPostgresQueryStore)
		}},
		{"TagPagesKeepAssignmentCounts", func(t *testing.T) {
			t.Helper()
			checkTagPagesKeepAssignmentCounts(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTagPagesKeepAssignmentCounts(t, newPostgresQueryStore)
		}},
		{"TaggedNodePagesKeepCurrentMetadataAndTrashCounts", func(t *testing.T) {
			t.Helper()
			checkTaggedNodePagesKeepCurrentMetadataAndTrashCounts(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTaggedNodePagesKeepCurrentMetadataAndTrashCounts(t, newPostgresQueryStore)
		}},
		{"TagAssignmentPathUsesCurrentTopology", func(t *testing.T) {
			t.Helper()
			checkTagAssignmentPathUsesCurrentTopology(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkTagAssignmentPathUsesCurrentTopology(t, newPostgresQueryStore)
		}},
		{"SavedQueryLifecycleCanonicalizesAndFencesRevisions", func(t *testing.T) {
			t.Helper()
			checkSavedQueryLifecycleCanonicalizesAndFencesRevisions(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSavedQueryLifecycleCanonicalizesAndFencesRevisions(t, newPostgresQueryStore)
		}},
		{"SavedQueriesAreBoundedPhotoScopedFilteredAndNameSorted", func(t *testing.T) {
			t.Helper()
			checkSavedQueriesAreBoundedPhotoScopedFilteredAndNameSorted(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSavedQueriesAreBoundedPhotoScopedFilteredAndNameSorted(t, newPostgresQueryStore)
		}},
		{"SavedQueryConcurrentSameNameCreateHasOneWinner", func(t *testing.T) {
			t.Helper()
			checkSavedQueryConcurrentSameNameCreateHasOneWinner(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSavedQueryConcurrentSameNameCreateHasOneWinner(t, newPostgresQueryStore)
		}},
		{"SavedQueryConcurrentSameRevisionEditHasOneWinner", func(t *testing.T) {
			t.Helper()
			checkSavedQueryConcurrentSameRevisionEditHasOneWinner(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkSavedQueryConcurrentSameRevisionEditHasOneWinner(t, newPostgresQueryStore)
		}},
	}},
	{name: "organization-and-delivery", postgres: "unqualified", scope: "Query-selected albums remain unqualified; exports covers native explicit-source plans and jobs; sealed-delivery covers snapshots, package identity and Bates; received-packages covers native intake and completion; people-curation covers custodians."},
	{name: "public-vault", postgres: "unqualified", scope: "Internal Store opening validates runtime credentials and pins the pool schema. Daemon and embedded Vault opening still use SQLite; there is no public PostgreSQL opener."},
}

func TestBackendContractsSQLite(t *testing.T) {
	t.Parallel()
	runBackendContracts(t, false)
}

func TestBackendContractsPostgres(t *testing.T) {
	t.Parallel()
	runBackendContracts(t, true)
}

func runBackendContracts(t *testing.T, postgres bool) {
	t.Helper()
	for _, suite := range backendSuites {
		for _, scenario := range suite.scenarios {
			run := scenario.sqlite
			if postgres {
				run = scenario.postgres
			}
			if run != nil {
				t.Run(suite.name+"/"+scenario.name, run)
			}
		}
	}
}

var reportBackendCoverage = flag.Bool("backend-coverage", false, "print the backend evidence inventory without opening databases")

func printBackendCoverage() {
	fmt.Println("SQLite remains the released backend. Shared cases below supplement its full suite.")
	fmt.Println("Counts identify scenarios, not completeness or performance guarantees. Unlisted operations remain unqualified.")
	fmt.Println("| Operation | PostgreSQL evidence | Shared scenarios | PostgreSQL-only scenarios | Boundary |")
	fmt.Println("| --- | --- | ---: | ---: | --- |")
	for _, suite := range backendSuites {
		shared, pgOnly := 0, 0
		for _, scenario := range suite.scenarios {
			if scenario.sqlite != nil && scenario.postgres != nil {
				shared++
			} else if scenario.postgres != nil {
				pgOnly++
			}
		}
		fmt.Printf("| %s | %s | %d | %d | %s |\n", suite.name, suite.postgres, shared, pgOnly, suite.scope)
	}
	printBackendMaintenance()
}
