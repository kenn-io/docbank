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
	{name: "visual-evidence", postgres: "native operations", scope: "Preview publication, exact-version reads, image preparation and retained email PDF evidence; excludes renderer executables and photo/person workflows.", scenarios: []backendScenario{
		{"VisualEvidenceSnapshots", nil, checkVisualEvidenceSnapshotsPostgres},
		{"VisualPreviewPublicationRollback", nil, checkVisualPreviewPublicationRollbackPostgres},
		{"PreviewBatchRecipes", func(t *testing.T) { t.Helper(); checkPreviewBatchRecipes(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPreviewBatchRecipes(t, newPostgresQueryStore) }},
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
		{"RenditionJobsNeverResubmitAmbiguousProviderWorkWithoutHandle", func(t *testing.T) {
			t.Helper()
			checkRenditionJobsNeverResubmitAmbiguousProviderWorkWithoutHandle(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobsNeverResubmitAmbiguousProviderWorkWithoutHandle(t, newPostgresQueryStore)
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
		{"PublishRenditionJobAtomicallyActivatesAuthorizedWaiters", func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobAtomicallyActivatesAuthorizedWaiters(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkPublishRenditionJobAtomicallyActivatesAuthorizedWaiters(t, newPostgresQueryStore)
		}},
		{"PublishRenditionJobFencesLeaseTheft", func(t *testing.T) { t.Helper(); checkPublishRenditionJobFencesLeaseTheft(t, newTestStore) }, func(t *testing.T) { t.Helper(); checkPublishRenditionJobFencesLeaseTheft(t, newPostgresQueryStore) }},
		{"RenditionJobRecoversFromStaleLexicalPublicationWithoutRepeatingEgress", func(t *testing.T) {
			t.Helper()
			checkRenditionJobRecoversFromStaleLexicalPublicationWithoutRepeatingEgress(t, newTestStore)
		}, func(t *testing.T) {
			t.Helper()
			checkRenditionJobRecoversFromStaleLexicalPublicationWithoutRepeatingEgress(t, newPostgresQueryStore)
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
	{name: "processing-publication", postgres: "unqualified", scope: "Extraction, email PDF rendering and media provider execution remain unqualified; retained-state cleanup is covered above."},
	{name: "photo-editing", postgres: "unqualified", scope: "Grouped import, XMP writing, authored metadata, people/face publication and photo review workflows remain unqualified."},
	{name: "search", postgres: "unqualified", scope: "Lexical/vector search and query execution remain unqualified; matching metadata columns do not establish ranking or index parity."},
	{name: "transfer-and-recovery", postgres: "unqualified", scope: "Metadata import/export, portable packages, backup and restore remain unqualified."},
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
	{name: "organization-and-delivery", postgres: "unqualified", scope: "Photo sets, custodian assignments, snapshots, Bates and delivery jobs remain unqualified."},
	{name: "multi-store", postgres: "unqualified", scope: "Placement, evacuation and storage-operation jobs remain unqualified."},
	{name: "public-vault", postgres: "unqualified", scope: "Daemon and embedded vault opening still use SQLite; there is no public PostgreSQL opener."},
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
