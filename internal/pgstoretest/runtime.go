// Package pgstoretest shares canonical Store runtime fixtures across PostgreSQL tests.
package pgstoretest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store/pgschema"
)

// RuntimeConfig installs canonical DDL and grants a separate test login the
// qualified Store operations. The caller opens and closes its runtime pool.
func RuntimeConfig(tb testing.TB, setup *sql.DB, cfg pgschema.Config) *pgx.ConnConfig {
	tb.Helper()
	ctx := tb.Context()
	_, err := pgschema.Setup(ctx, setup, cfg)
	require.NoError(tb, err)

	role := fmt.Sprintf("query_runtime_%x", uuid.New())
	quotedRole := pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "CREATE ROLE "+quotedRole+" LOGIN PASSWORD 'fixture-query-only'")
	require.NoError(tb, err)
	tb.Cleanup(func() {
		_, err := setup.ExecContext(context.WithoutCancel(tb.Context()), "DROP OWNED BY "+quotedRole+"; DROP ROLE "+quotedRole)
		require.NoError(tb, err)
	})
	schema := pgx.Identifier{cfg.Schema}.Sanitize()
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(lifecycle) ON "+schema+".blob_stores TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".storage_operation_stores TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".storage_operation_cleanup TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".photo_hidden_credentials TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(state,cursor,total_objects,completed_objects,copied_objects,copied_bytes,cancel_requested,error,receipt_json,updated_at,finished_at,retention_until) ON "+schema+".storage_operations TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schema+" TO "+quotedRole+
		"; GRANT SELECT ON ALL TABLES IN SCHEMA "+schema+" TO "+quotedRole+
		"; GRANT SELECT,INSERT,UPDATE,DELETE ON "+schema+".nodes,"+schema+".blobs,"+schema+".content_versions TO "+quotedRole+
		"; GRANT SELECT,INSERT ON "+schema+".blob_checksums TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".mailbox_archives,"+schema+".mailbox_chunks,"+schema+".mailbox_transfer_receipts TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(state,manifest_sha256) ON "+schema+".mailbox_containers TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,claim,job_json,query_json) ON "+schema+".mailbox_jobs TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(receipt_id,occurrence_json) ON "+schema+".mailbox_occurrences TO "+quotedRole+
		"; GRANT SELECT ON "+schema+".audit_records,"+schema+".audit_authority,"+schema+".audit_scopes,"+schema+".audit_baselines,"+schema+".audit_memberships TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".audit_records TO "+quotedRole+
		"; GRANT UPDATE(operation_sequence_high_water,allocation_entry_count,allocation_head) ON "+schema+".audit_authority TO "+quotedRole+
		"; GRANT UPDATE(entry_count,chain_head) ON "+schema+".audit_scopes TO "+quotedRole+
		"; GRANT SELECT,UPDATE(high_water) ON "+schema+".node_allocation TO "+quotedRole+
		"; GRANT SELECT,INSERT,DELETE ON "+schema+".vault_changes TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(canonical_json,state,expires_at) ON "+schema+".export_sources TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".export_chunks,"+schema+".export_members,"+schema+".export_documents,"+schema+".export_role_roots TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(canonical_json,expires_at) ON "+schema+".export_plans TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(canonical_json,state,expires_at,epoch,token,archive_name) ON "+schema+".export_jobs TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".collection_snapshots,"+schema+".collection_snapshot_members,"+schema+".collection_snapshot_representations,"+schema+".packages,"+schema+".package_volumes,"+schema+".bates_namespaces,"+schema+".bates_namespace_cursors,"+schema+".bates_allocations,"+schema+".bates_page_labels,"+schema+".bates_artifacts,"+schema+".bates_artifact_pages TO "+quotedRole+
		"; GRANT UPDATE(next_sequence) ON "+schema+".bates_namespace_cursors TO "+quotedRole+
		"; GRANT UPDATE(state,committed_at) ON "+schema+".bates_allocations TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".package_preflights TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".package_records,"+schema+".package_labels,"+schema+".package_import_receipts,"+schema+".package_import_jobs TO "+quotedRole+
		"; GRANT UPDATE(state,snapshot_id,completed_at) ON "+schema+".packages TO "+quotedRole+
		"; GRANT UPDATE(state,epoch,token,claim_owner,lease_expires_at,updated_at) ON "+schema+".package_import_jobs TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(display_name,display_name_folded,state,revision,updated_at,key_face_id) ON "+schema+".persons TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(person_id) ON "+schema+".person_identities TO "+quotedRole+
		"; GRANT INSERT,UPDATE(person_id,uid_kind,uid_state,last_seen_revision,display_name_snapshot,updated_at) ON "+schema+".person_external_identities TO "+quotedRole+
		"; GRANT INSERT,UPDATE(surviving_uid,observed_at) ON "+schema+".person_external_uid_aliases TO "+quotedRole+
		"; GRANT INSERT,UPDATE(surviving_person_id,reason,retired_at) ON "+schema+".person_aliases TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".person_merges,"+schema+".person_splits TO "+quotedRole+
		"; GRANT INSERT,UPDATE(person_id,raw_label,raw_label_folded,basis,source_ref,revision,recorded_at,retired_at) ON "+schema+".custodian_assignments TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(person_id,action,note,revision,photo_mark) ON "+schema+".person_document_assertions TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(suggested_person_id,state,decided_person_id,decided_at,revision) ON "+schema+".person_match_candidates TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".person_candidate_faces TO "+quotedRole+
		"; GRANT UPDATE(assertion_id) ON "+schema+".photo_faces TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(name,starred,revision,cover_asset_id,updated_at,deleted_at) ON "+schema+".photo_sets TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_set_members TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(preference,revision,updated_at,burst_gap_seconds,backup_interval_seconds,backup_keep_last,hidden_unlock_lifetime_seconds,representative_sequence) ON "+schema+".photo_library_settings TO "+quotedRole+
		"; GRANT DELETE,UPDATE(passcode_hash) ON "+schema+".photo_hidden_credentials TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_hidden_failures TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(locked_until) ON "+schema+".photo_hidden_lockout TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(state,focus,blur,brightness,color_red,color_green,color_blue,framing,aesthetics,face_reports_json,face_inputs) ON "+schema+".photo_quality_signals TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT SELECT,INSERT ON "+schema+".photo_xmp_unsettled TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".audit_authority,"+schema+".audit_scopes,"+schema+".audit_baselines,"+schema+".audit_memberships TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE ON "+schema+".blob_locations TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".blob_pack_entries,"+schema+".rendition_blob_staging,"+schema+".derivative_blob_purge_pending,"+schema+".photo_xmp_unsettled TO "+quotedRole+
		"; GRANT UPDATE(live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len) ON "+schema+".blob_packs TO "+quotedRole+
		"; GRANT INSERT,UPDATE(next_attempt_at) ON "+schema+".text_extraction_queue TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".text_searchable_versions TO "+quotedRole+
		"; GRANT INSERT,UPDATE(extractor_version,status,error,attempts,text,extracted_at) ON "+schema+".extracted_text TO "+quotedRole+
		"; GRANT INSERT,UPDATE(hidden_at,excluded_at,display_file_id,display_override_file_id,representative_rank,revision,updated_at) ON "+schema+".photo_assets TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_change_receipts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(asset_id,role,sidecar_of_file_id,source_provenance_identity,revision,rating,flag,label,caption,creator,copyright,rotation,confirmed_fields,flag_owner) ON "+schema+".photo_files TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_timeline TO "+quotedRole+
		"; GRANT INSERT,UPDATE(original_path,original_mtime,folder_key) ON "+schema+".photo_source_claims TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE ON "+schema+".ingests TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".provenance,"+schema+".provenance_version_bindings TO "+quotedRole+
		"; GRANT INSERT,UPDATE,DELETE ON "+schema+".collection_labels TO "+quotedRole+
		"; GRANT INSERT,UPDATE ON "+schema+".watch_sources,"+schema+".push_sources TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".watch_source_absences TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".document_event_heads,"+schema+".document_event_generations,"+schema+".document_people_heads,"+schema+".document_people TO "+quotedRole+
		"; GRANT INSERT,UPDATE(revision,reason) ON "+schema+".document_event_dirty TO "+quotedRole+
		"; GRANT UPDATE(publication_epoch,updated_at) ON "+schema+".document_people_state,"+schema+".document_event_state TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT DELETE ON "+schema+".rendition_heads,"+schema+".rendition_attachments,"+schema+".rendition_job_waiters,"+schema+".email_attachments,"+schema+".embedding_jobs,"+schema+".embedding_heads,"+schema+".embedding_sets,"+schema+".photo_files TO "+quotedRole+
		"; GRANT DELETE,UPDATE(state,phase,claim_owner,lease_expires_at,claim_epoch,available_at,selected_waiter_id,authorization_grant_id,authorization_incarnation_id,authorization_revocation_fence,provider_started,provider_resume_handle,execution_snapshot_json,lexical_generation_id,failure_code,updated_at) ON "+schema+".rendition_jobs TO "+quotedRole+
		"; GRANT UPDATE(state,failure_code,updated_at) ON "+schema+".rendition_job_waiters TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(root_kind,target_kind,target_id,fencing_token,recorded_at,expires_at,active,released_at) ON "+schema+".current_rendition_roots TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(name,revision,assignment_count) ON "+schema+".tags TO "+quotedRole+
		"; GRANT UPDATE(binding_epoch) ON "+schema+".document_people_state TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT DELETE ON "+schema+".embedding_input_generations,"+schema+".embedding_vector_sets,"+schema+".embedding_vector_spaces,"+schema+".embedding_failures,"+schema+".email_heads,"+schema+".email_part_artifacts,"+schema+".email_generations,"+schema+".email_body_results,"+schema+".rendition_builds,"+schema+".rendition_artifacts,"+schema+".rendition_units,"+schema+".rendition_lexical_segments,"+schema+".rendition_lexical_index,"+schema+".rendition_lexical_heads,"+schema+".rendition_lexical_generations,"+schema+".rendition_lexical_generation_manifests,"+schema+".text_searchable_versions,"+schema+".extracted_text,"+schema+".text_extraction_queue TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".rendition_lexical_stream_builds TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".derivative_blob_purge_pending,"+schema+".rendition_lexical_generations,"+schema+".rendition_lexical_stream_builds,"+schema+".rendition_lexical_generation_manifests,"+schema+".rendition_lexical_index TO "+quotedRole+
		"; GRANT UPDATE(generation_id) ON "+schema+".rendition_lexical_heads TO "+quotedRole+
		"; GRANT INSERT,UPDATE(purged_at,active,superseded_at,superseding_build_id) ON "+schema+".derivative_purge_suppressions TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(scan_hash) ON "+schema+".blob_packs TO "+quotedRole+
		"; GRANT INSERT,UPDATE ON "+schema+".blob_pack_entries TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".blob_locations TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".gc_loose_retirements,"+schema+".derivative_pack_purge_pending TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE ON "+schema+".node_tags TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".batch_tag_receipts TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(name,description,payload,fingerprint,revision,updated_at,photo_scope) ON "+schema+".saved_queries TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".source_metadata_generations,"+schema+".document_event_generations,"+schema+".document_events,"+schema+".document_event_actors,"+schema+".document_event_primaries TO "+quotedRole+
		"; GRANT INSERT,UPDATE(generation_id,published_at) ON "+schema+".source_metadata_heads TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_technical_metadata TO "+quotedRole+
		"; GRANT INSERT,UPDATE(projection_recipe) ON "+schema+".photo_technical_metadata_state TO "+quotedRole+
		"; GRANT INSERT,UPDATE(deriver_fingerprint,input_epoch) ON "+schema+".document_event_state TO "+quotedRole+
		"; GRANT INSERT,UPDATE(generation_id,input_epoch,published_at) ON "+schema+".document_event_heads TO "+quotedRole+
		"; GRANT INSERT,UPDATE(input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at) ON "+schema+".document_event_attempts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,scanned,published,failed,unavailable,updated_at,finished_at) ON "+schema+".document_event_builds TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".document_people_generations,"+schema+".document_people TO "+quotedRole+
		"; GRANT INSERT,UPDATE(event_generation_id,inputs_sha256,generation_id,unresolved_actors,suppressed_actors,candidate_overflow,resolver_fingerprint,binding_epoch,node_revision,edge_count,state,failure_reason,published_at) ON "+schema+".document_people_heads TO "+quotedRole+
		"; GRANT UPDATE(resolver_fingerprint) ON "+schema+".document_people_state TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,scanned,published,failed,updated_at,finished_at) ON "+schema+".document_people_builds TO "+quotedRole)
	require.NoError(tb, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".rendition_blob_staging,"+schema+".processing_consent_grants,"+schema+".processing_consent_revocations,"+schema+".processing_profiles,"+schema+".rendition_builds,"+schema+".rendition_artifacts,"+schema+".rendition_units,"+schema+".rendition_lexical_segments,"+schema+".rendition_attachments,"+schema+".rendition_jobs,"+schema+".rendition_job_waiters,"+schema+".rendition_lexical_heads,"+schema+".rendition_lexical_superseded TO "+quotedRole+
		"; GRANT INSERT,UPDATE(attachment_id,published_at) ON "+schema+".rendition_heads TO "+quotedRole+
		"; GRANT UPDATE(provider_attempts) ON "+schema+".rendition_jobs TO "+quotedRole+
		"; GRANT UPDATE(attachment_id) ON "+schema+".rendition_job_waiters TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".embedding_vector_spaces,"+schema+".embedding_input_generations,"+schema+".embedding_generation_inputs,"+schema+".embedding_vector_sets,"+schema+".embedding_vector_rows,"+schema+".embedding_sets TO "+quotedRole+
		"; GRANT INSERT,UPDATE(embedding_set_id,vector_space_id,profile_fingerprint,published_at,fencing_token) ON "+schema+".embedding_heads TO "+quotedRole+
		"; GRANT INSERT,UPDATE(failure_code,failed_at,fencing_token,attachment_id) ON "+schema+".embedding_failures TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,claim_owner,claim_epoch,claim_count,lease_expires_at,available_at,failure_code,receipt_json,updated_at) ON "+schema+".embedding_jobs TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".page_documents,"+schema+".page_frames,"+schema+".page_recipes,"+schema+".page_images,"+schema+".email_generations,"+schema+".email_part_artifacts,"+schema+".email_attachments,"+schema+".email_body_results,"+schema+".email_document_publications,"+schema+".email_document_relations TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,results_json,failure_code,epoch,token,updated_at) ON "+schema+".page_render_jobs TO "+quotedRole+
		"; GRANT INSERT,UPDATE(attachment_id,published_at) ON "+schema+".email_heads TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".email_document_publications TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".media_sources,"+schema+".media_source_versions,"+schema+".media_input_artifacts,"+schema+".media_occurrences,"+schema+".media_operations TO "+quotedRole+
		"; GRANT UPDATE(source_version_id,visible,revoked_at) ON "+schema+".media_occurrences TO "+quotedRole+
		"; GRANT UPDATE(receipt_json,updated_at,receipt_source_version_id,receipt_operation_state) ON "+schema+".media_operations TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".visual_preview_generations TO "+quotedRole+
		"; GRANT INSERT,UPDATE(generation_id,published_at) ON "+schema+".visual_preview_heads TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".photo_description_explicit_waiters,"+schema+".photo_face_scans TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(producer_fingerprint,caption,tags_json,visible_text,rotation,dismissed,awaiting,caption_resolved,resolved_tags_json) ON "+schema+".photo_suggestions TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(x,y,width,height,roll,facing,eyes_open,smile,detector_fingerprint,revision) ON "+schema+".photo_faces TO "+quotedRole)
	require.NoError(tb, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(last_operation_id,destination,destination_identity,state,destination_reserved,error,retention_released,retired_at,retirement_requested) ON "+schema+".photo_checkouts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(published,base_held,observed_identity,observed_hash,observed_size,observed_mtime,observed_since,working_state,version_id,blob_hash,size) ON "+schema+".photo_checkout_entries TO "+quotedRole)
	require.NoError(tb, err)

	config, err := pgx.ParseConfig(os.Getenv("DOCBANK_TEST_POSTGRES_DSN"))
	require.NoError(tb, err)
	require.NoError(tb, setup.QueryRowContext(ctx, `SELECT current_database()`).Scan(&config.Database))
	config.User, config.Password = role, "fixture-query-only"
	config.RuntimeParams["search_path"] = schema + ",pg_catalog"
	return config
}
