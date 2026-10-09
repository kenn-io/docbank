package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
)

type metadataPhotoFileV28 struct {
	Type        string  `json:"type"`
	FileID      string  `json:"file_id" db:"file_id"`
	AssetID     string  `json:"asset_id" db:"asset_id"`
	NodeID      int64   `json:"node_id" db:"node_id"`
	Role        string  `json:"role" db:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id" db:"sidecar_of_file_id"`
	CreatedAt   string  `json:"created_at" db:"created_at"`
}

func (v metadataPhotoFileV28) current() metadataPhotoFile {
	return metadataPhotoFile{Type: v.Type, FileID: v.FileID, AssetID: new(v.AssetID), NodeID: v.NodeID, Role: v.Role, SidecarOfID: v.SidecarOfID, CreatedAt: v.CreatedAt, Revision: 1}
}

var photoFileV28Metadata = newMetadataTable(metadataTable[metadataPhotoFileV28]{record: metadataPhotoFileV28{Type: metadataPhotoFileType}, table: "photo_files", suffix: "ORDER BY file_id", validate: func(v metadataPhotoFileV28) error { return validatePhotoFileMetadataRecord(v.current()) }, checkExport: true})

func photoMetadataTablesForLayout(layout metadataSourceLayout) []metadataRecordCodec {
	if layout.schemaVersion >= 30 {
		return photoMetadataTables
	}
	if layout.schemaVersion >= 29 {
		tables := slices.Clone(photoMetadataTables)
		tables[1] = photoFileV28Export{photoFileV28Metadata}
		return tables
	}
	return []metadataRecordCodec{
		photoMetadataTables[0],
		photoFileV28Export{photoFileV28Metadata},
		photoMetadataTables[2],
		newMetadataTable(metadataTable[metadataPhotoReceipt]{
			record: metadataPhotoReceipt{Type: metadataPhotoReceiptType},
			table:  "(SELECT *, NULL AS set_id FROM photo_change_receipts)",
			suffix: "ORDER BY receipt_id", validate: validatePhotoReceiptMetadataRecord, checkExport: true}),
	}
}

type photoFileV28Export struct{ metadataRecordCodec }

func normalizePhotoFileMetadata(raw jsontext.Value) (jsontext.Value, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"revision", "rating", "flag", "label", "caption", "creator", "copyright", "rotation"} {
		if _, exists := fields[name]; exists {
			return raw, nil
		}
	}
	required, nullable := photoFileV28Metadata.fields()
	if err := requireMetadataFields(raw, required, nullable); err != nil {
		return nil, err
	}
	var old metadataPhotoFileV28
	if err := json.Unmarshal(raw, &old, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if err := validatePhotoFileMetadataRecord(old.current()); err != nil {
		return nil, err
	}
	return json.Marshal(old.current(), json.Deterministic(true))
}

func (c photoFileV28Export) export(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	return photoFileV28Metadata.export(ctx, q, func(v any) error {
		old, ok := v.(metadataPhotoFileV28)
		if !ok {
			return errors.New("invalid schema-28 photo file")
		}
		return write(old.current())
	})
}

func validateV28Schema(db *sql.DB, _, _ []string) error {
	required := []string{"audit_authority", "audit_baselines", "audit_memberships", "audit_records", "audit_scopes", "batch_tag_receipts", "bates_allocations", "bates_artifact_pages", "bates_artifacts", "bates_namespace_cursors", "bates_namespaces", "bates_page_labels", "blob_checksums", "blob_locations", "blob_pack_entries", "blob_packs", "blob_stores", "blobs", "collection_labels", "collection_snapshot_members", "collection_snapshot_representations", "collection_snapshots", "content_versions", "current_processing_incarnation", "current_rendition_roots", "custodian_assignments", "derivative_blob_purge_pending", "derivative_pack_purge_pending", "derivative_purge_suppressions", "document_event_actors", "document_event_attempts", "document_event_builds", "document_event_dirty", "document_event_generations", "document_event_heads", "document_event_primaries", "document_event_state", "document_events", "document_people", "document_people_builds", "document_people_generations", "document_people_heads", "document_people_state", "email_attachments", "email_body_results", "email_document_publications", "email_document_relations", "email_generations", "email_heads", "email_part_artifacts", "embedding_failures", "embedding_generation_inputs", "embedding_heads", "embedding_input_generations", "embedding_jobs", "embedding_sets", "embedding_vector_rows", "embedding_vector_sets", "embedding_vector_spaces", "export_chunks", "export_documents", "export_jobs", "export_members", "export_plans", "export_role_roots", "export_sources", "extracted_text", "gc_loose_retirements", "ingests", "mailbox_archives", "mailbox_chunks", "mailbox_containers", "mailbox_jobs", "mailbox_occurrences", "mailbox_transfer_receipts", "media_input_artifacts", "media_occurrences", "media_operations", "media_source_versions", "media_sources", "node_tags", "nodes", "package_import_jobs", "package_import_receipts", "package_labels", "package_preflights", "package_records", "package_volumes", "packages", "page_documents", "page_frames", "page_images", "page_recipes", "page_render_jobs", "person_aliases", "person_document_assertions", "person_external_identities", "person_external_uid_aliases", "person_identities", "person_match_candidates", "person_merges", "person_splits", "persons", "photo_assets", "photo_change_receipts", "photo_files", "photo_library_settings", "photo_technical_metadata", "photo_technical_metadata_state", "processing_consent_grants", "processing_consent_revocations", "processing_incarnations", "processing_profiles", "provenance", "provenance_version_bindings", "rendition_artifacts", "rendition_attachments", "rendition_blob_staging", "rendition_builds", "rendition_heads", "rendition_job_waiters", "rendition_jobs", "rendition_lexical_generation_builds", "rendition_lexical_generation_manifests", "rendition_lexical_generations", "rendition_lexical_heads", "rendition_lexical_index", "rendition_lexical_segments", "rendition_lexical_superseded", "rendition_units", "saved_queries", "saved_query_runs", "source_metadata_generations", "source_metadata_heads", "storage_operation_cleanup", "storage_operation_stores", "storage_operations", "tags", "term_report_history", "text_extraction_queue", "text_searchable_versions", "vault_metadata", "vector_index_build_jobs", "vector_index_generations", "vector_index_heads", "vector_index_reader_leases", "vector_index_unavailable_coverage", "visual_preview_generations", "visual_preview_heads", "watch_sources"}
	for _, table := range required {
		columns, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			return errors.New("not a released schema-28 database")
		}
	}
	columns, err := tableColumns(db, "photo_files")
	if err != nil {
		return err
	}
	if !slices.Equal(columns, []string{"asset_id", "created_at", "file_id", "node_id", "role", "sidecar_of_file_id"}) {
		return errors.New("not a released schema-28 photo layout")
	}
	return nil
}

func validateV29Schema(db *sql.DB, blobs, packs []string) error {
	if err := validateV28Schema(db, blobs, packs); err != nil {
		return err
	}
	for _, table := range []string{"photo_sets", "photo_set_members"} {
		columns, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		if len(columns) == 0 {
			return errors.New("not a released schema-29 album layout")
		}
	}
	columns, err := tableColumns(db, "photo_change_receipts")
	if err != nil {
		return err
	}
	if !slices.Contains(columns, "set_id") {
		return errors.New("not a released schema-29 receipt layout")
	}
	return nil
}
