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
	{name: "processing-publication", postgres: "unqualified", scope: "Rendition, extraction, embedding, page, email and media producers remain unqualified; retained-state cleanup is covered above."},
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
