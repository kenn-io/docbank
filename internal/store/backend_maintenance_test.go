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
	{"pack summaries", "pack_maintenance.go: refreshPackSummariesTx", "documents/physical-storage/multi-store contracts", `
 blob_pack_summary_mapping_insert blob_pack_summary_mapping_delete
 blob_pack_summary_mapping_update blob_pack_summary_blob_delete blob_pack_summary_blob_insert`},
	{"content promotion", "document_maintenance.go: promoteContentBlobTx", "documents contracts", `
 content_versions_promote_rendition_staging content_versions_revoke_derivative_purge`},
	{"invalidation epochs", "document_events.go and retention_maintenance.go", "sources/retention/metadata-events/document-people contracts", `
 document_event_heads_deleted content_versions_invalidate_person_bindings`},
	{"photo timeline", "document_maintenance.go: refreshPhotoTimelineTx", "documents/imports/photo-import/photo-checkout/sources/retention contracts; SQLite pending table is a trigger intermediate", `
 photo_timeline_refresh photo_timeline_file_insert photo_timeline_role_change
 photo_timeline_version_delete photo_timeline_version_change
 photo_timeline_binding_insert photo_timeline_correction_insert`},
	{"photo export queue", "document_maintenance.go: refreshPhotoAssetWorkTx and refreshNodePhotoWorkTx", "documents/photo-import/photo-checkout/trash/retention/organization/photo-edit/photo-analysis/photo-quality-hidden/photo-review contracts", `
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
	{"photo XMP publication", "photo_xmp.go: settlePhotoXMPTx; document_maintenance.go: refreshPhotoAssetWorkTx", "photo-edit contracts; receipts are replaced rather than updated", `
 photo_xmp_unsettled_receipt_insert photo_xmp_unsettled_receipt_update`},
	{"photo metadata recovery", "unported: source-head recovery", "must refresh export work when heads are removed", `photo_xmp_unsettled_head_delete`},
	{"people candidates", "person_candidates.go: retainPersonCandidate", "people-curation contracts cover inserts; evidence replacement during metadata restore remains unqualified", `
 person_candidate_faces_insert person_candidate_faces_update`},
	{"lexical indexes", "unported: search", "must preserve eligible rows and index access; schema comparison excludes FTS virtual/shadow tables", `
 nodes_fts_insert nodes_fts_delete nodes_fts_update
 photo_metadata_fts_insert photo_metadata_fts_delete
 photo_authored_fts_insert photo_authored_fts_update photo_authored_fts_delete
 photo_suggestion_fts_insert photo_suggestion_fts_update photo_suggestion_fts_delete
 tag_fts_insert tag_fts_update tag_fts_delete
 rendition_lexical_index_insert rendition_lexical_index_delete`},
	{"processing authority", "consent.go; INSERT-only grants and revocations; setup owns incarnation", "renditions contracts; restore incarnation replacement remains unqualified", `
 current_processing_incarnation_immutable_update current_processing_incarnation_immutable_delete
 processing_incarnations_immutable_update processing_incarnations_immutable_delete
 processing_consent_grants_immutable_update processing_consent_grants_immutable_delete
 processing_consent_revocations_immutable_update processing_consent_revocations_immutable_delete`},
	{"metadata and event evidence", "source_metadata.go and document_events.go; INSERT-only publication grants", "metadata-events contracts; retained evidence is never rewritten", `
 source_metadata_generations_immutable_update document_event_generations_immutable_update
 document_events_immutable_update document_event_actors_immutable_update document_event_primaries_immutable_update`},
	{"document-person generations", "document_people.go; INSERT-only generation grants", "document-people contracts; publication replaces heads without rewriting retained attribution", `
 document_people_generations_immutable_update`},
	{"visual preview records", "visual_preview.go; INSERT-only generation grants", "visual-evidence contracts; active heads may change without rewriting retained generations", `
 visual_preview_generations_immutable_update`},
	{"media source and input authority", "media_sources.go, media_runtime.go and media_artifacts.go; INSERT-only grants", "media contracts; first binding and revocation update occurrences without rewriting source or input records", `
 media_sources_immutable_update media_source_versions_immutable_update
 media_input_artifacts_immutable_update`},
	{"embedding records", "embedding_catalog.go; runtime has no UPDATE grant", "embeddings contracts; immutable records may be collected after roots expire", `
 embedding_vector_spaces_immutable_update embedding_input_generations_immutable_update
 embedding_generation_inputs_immutable_update embedding_vector_sets_immutable_update
 embedding_vector_rows_immutable_update embedding_sets_immutable_update`},
	{"vector source membership epochs", "unported: vector_index.go", "SQLite-only derived state; PostgreSQL ranked search remains unqualified", `
 vector_source_embedding_heads_insert vector_source_embedding_heads_update vector_source_embedding_heads_delete
 vector_source_rendition_heads_insert vector_source_rendition_heads_update vector_source_rendition_heads_delete
 vector_source_visual_preview_generations_insert vector_source_visual_preview_generations_delete
 vector_source_photo_files_insert vector_source_photo_files_update vector_source_photo_files_delete
 vector_source_photo_assets_update vector_source_nodes_update`},
	{"rendition records", "processing_catalog.go and search.go; runtime has no UPDATE grant", "renditions contracts; cleanup may delete immutable records", `
 processing_profiles_immutable_update rendition_builds_immutable_update
 rendition_artifacts_immutable_update rendition_units_immutable_update
 rendition_lexical_segments_immutable_update rendition_attachments_immutable_update`},
	{"sealed snapshots and Bates publication", "packages.go, packages_stream.go and bates*.go; INSERT-only identity grants", "sealed-delivery contracts; Store enforces monotone ranges and one-way publication; PostgreSQL column grants alone do not enforce transitions", `
 collection_snapshots_immutable_update collection_snapshot_members_immutable_update
 collection_snapshot_representations_immutable_update
 bates_artifacts_immutable_update bates_artifact_pages_immutable_update
 bates_namespaces_immutable_update bates_namespace_cursors_monotone
 bates_allocations_transition_guard bates_page_labels_immutable_update
 package_volumes_immutable_update`},
	{"received package transitions", "package_import*.go; INSERT-only record, label and receipt grants; column-specific package/job updates", "received-packages contracts; Store fences leases and fills snapshot/completion once; PostgreSQL permits direct SQL rewrites of those columns; SQLite receipts block updates, not deletion", `
 packages_transition_guard package_records_immutable_update
 package_labels_immutable_update package_import_receipts_immutable_update`},
	{"document validity declarations", "unported: supersession.go", "PutSupersession, DeleteSupersession, PutEffectiveWindow, DeleteEffectiveWindow and NodeValidity are PostgreSQL-unqualified; documents contracts validate copied retained catalogs only", ""},
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
