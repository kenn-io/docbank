package store

import (
	"fmt"
	"strings"
)

type backendMaintenance struct {
	name     string
	owner    string
	boundary string
	triggers string
}

// Descriptive ownership inventory, grouped by effect. The named contract
// suites exercise the ported effects through native operations.
var backendMaintenanceEffects = []backendMaintenance{
	{"checksums", "content_checksums.go; INSERT-only runtime grant", "checksums contracts; no UPDATE path", `
 blob_checksums_immutable_update`},
	{"provenance", "ingest.go, provenance.go and provenance_bindings.go", "imports/sources contracts; immutable inserts and same-node predecessor validation", `
 ingests_immutable_update provenance_immutable_update provenance_same_node_insert
 provenance_version_bindings_immutable_update`},
	{"pack summaries", "pack_maintenance.go: refreshPackSummariesTx", "documents/physical-storage contracts", `
 blob_pack_summary_mapping_insert blob_pack_summary_mapping_delete
 blob_pack_summary_mapping_update blob_pack_summary_blob_delete blob_pack_summary_blob_insert`},
	{"content promotion", "document_maintenance.go: promoteContentBlobTx", "documents contracts", `
 content_versions_promote_rendition_staging content_versions_revoke_derivative_purge`},
	{"invalidation epochs", "document_events.go and retention_maintenance.go", "sources/retention/metadata-events contracts; people publication remains unqualified", `
 document_event_heads_deleted content_versions_invalidate_person_bindings`},
	{"photo timeline", "document_maintenance.go: refreshPhotoTimelineTx", "documents/imports/sources/retention contracts; SQLite pending table is a trigger intermediate", `
 photo_timeline_refresh photo_timeline_file_insert photo_timeline_role_change
 photo_timeline_version_delete photo_timeline_version_change
 photo_timeline_binding_insert photo_timeline_correction_insert`},
	{"photo export queue", "document_maintenance.go: refreshPhotoAssetWorkTx and refreshNodePhotoWorkTx", "documents/trash/retention/organization contracts; XMP publication remains unqualified", `
 photo_xmp_unsettled_refresh photo_xmp_unsettled_file_insert photo_xmp_unsettled_file_update
 photo_xmp_unsettled_file_delete photo_xmp_unsettled_asset_insert photo_xmp_unsettled_asset_update
 photo_xmp_unsettled_node_update photo_xmp_unsettled_node_insert photo_xmp_unsettled_node_delete
 photo_xmp_unsettled_receipt_delete`},
	{"audit photo export queue", "audit_photo_queue.go: queueAuditPhotosTx", "enrollment contracts; deletion of authority/memberships is unqualified", `
 photo_xmp_unsettled_membership_insert photo_xmp_unsettled_authority_insert`},
	{"audit deletion queue", "unported: metadata import/restore", "must queue affected photos when replacing authority/memberships", `
 photo_xmp_unsettled_membership_delete photo_xmp_unsettled_authority_delete`},
	{"photo metadata publication", "document_maintenance.go: refreshSourceMetadataPhotosTx", "metadata-events contracts; head deletion during recovery remains unqualified", `
 photo_timeline_technical_insert photo_timeline_technical_delete
 photo_timeline_head_insert photo_timeline_head_update
 photo_xmp_unsettled_technical_insert photo_xmp_unsettled_technical_delete
 photo_xmp_unsettled_head_insert photo_xmp_unsettled_head_update`},
	{"photo metadata recovery and XMP publication", "unported: source-head recovery and XMP writers", "must refresh export work when heads or receipts change", `
 photo_xmp_unsettled_head_delete photo_xmp_unsettled_receipt_insert photo_xmp_unsettled_receipt_update`},
	{"people candidates", "unported: person candidate publication", "must update the candidate-to-face projection in the publishing transaction", `
 person_candidate_faces_insert person_candidate_faces_update`},
	{"lexical indexes", "unported: search", "must preserve eligible rows and index access; schema comparison excludes FTS virtual/shadow tables", `
 nodes_fts_insert nodes_fts_delete nodes_fts_update
 photo_metadata_fts_insert photo_metadata_fts_delete
 photo_authored_fts_insert photo_authored_fts_update photo_authored_fts_delete
 photo_suggestion_fts_insert photo_suggestion_fts_update photo_suggestion_fts_delete
 tag_fts_insert tag_fts_update tag_fts_delete
 rendition_lexical_index_insert rendition_lexical_index_delete`},
	{"processing authority", "pgschema.Setup initializes incarnation; runtime has read-only grants", "consent changes and incarnation replacement remain unqualified", `
 current_processing_incarnation_immutable_update current_processing_incarnation_immutable_delete
 processing_incarnations_immutable_update processing_incarnations_immutable_delete
 processing_consent_grants_immutable_update processing_consent_grants_immutable_delete
 processing_consent_revocations_immutable_update processing_consent_revocations_immutable_delete`},
	{"metadata and event evidence", "source_metadata.go and document_events.go; INSERT-only publication grants", "metadata-events contracts; retained evidence is never rewritten", `
 source_metadata_generations_immutable_update document_event_generations_immutable_update
 document_events_immutable_update document_event_actors_immutable_update document_event_primaries_immutable_update`},
	{"retained processing records", "no runtime UPDATE grant; producers unported", "enrollment validates retained rows; derivatives/retention may delete them, never rewrite them", `
 document_people_generations_immutable_update
 media_sources_immutable_update media_source_versions_immutable_update
 media_input_artifacts_immutable_update visual_preview_generations_immutable_update
 embedding_vector_spaces_immutable_update embedding_input_generations_immutable_update
 embedding_generation_inputs_immutable_update embedding_vector_sets_immutable_update
 embedding_vector_rows_immutable_update embedding_sets_immutable_update
 processing_profiles_immutable_update rendition_builds_immutable_update
 rendition_artifacts_immutable_update rendition_units_immutable_update
 rendition_lexical_segments_immutable_update rendition_attachments_immutable_update`},
	{"transfer and batch records", "unported: collection snapshots, Bates and packages", "retained validation does not qualify immutable-record or state-transition writes", `
 collection_snapshots_immutable_update collection_snapshot_members_immutable_update
 collection_snapshot_representations_immutable_update
 bates_artifacts_immutable_update bates_artifact_pages_immutable_update
 bates_namespaces_immutable_update bates_namespace_cursors_monotone
 bates_allocations_transition_guard bates_page_labels_immutable_update
 package_volumes_immutable_update packages_transition_guard package_records_immutable_update
 package_labels_immutable_update package_import_receipts_immutable_update`},
	{"batch tag receipts", "batch_tag.go; runtime INSERT-only grants", "organization contracts; committed receipts survive tag deletion", `batch_tag_receipts_immutable_update batch_tag_receipts_immutable_delete`},
}

func printBackendMaintenance() {
	fmt.Println("\nSQLite catalog effects (ownership inventory; behavior evidence is in the named suites):")
	fmt.Println("| Effect | Triggers | PostgreSQL owner | Boundary |")
	fmt.Println("| --- | ---: | --- | --- |")
	for _, effect := range backendMaintenanceEffects {
		fmt.Printf("| %s | %d | %s | %s |\n", effect.name, len(strings.Fields(effect.triggers)), effect.owner, effect.boundary)
	}
	fmt.Printf("\nSchema exceptions: %d SQLite-only tables, %d deferred indexes, %d PostgreSQL cascade indexes.\n", len(sqliteOnlyBackendTables), len(deferredBackendIndexes), len(postgresOnlyBackendIndexes))
	fmt.Println("Exact objects and removal conditions: internal/store/backend_schema_exceptions_test.go.")
}
