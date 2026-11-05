package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
	"go.kenn.io/docbank/internal/waitlock"
)

// Queries use setup-owned DDL and an explicitly granted runtime login. This
// constructs the qualified store surface; complete PostgreSQL vaults are not openable.
func newPostgresQueryStore(t *testing.T) *Store {
	t.Helper()
	setup, owner := pgtest.NewDatabase(t)
	return newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
}

func newPostgresSchemaStore(t *testing.T, setup *sql.DB, cfg pgschema.Config) *Store {
	t.Helper()
	ctx := t.Context()
	id, err := pgschema.Setup(ctx, setup, cfg)
	require.NoError(t, err)

	role := fmt.Sprintf("query_runtime_%x", uuid.New())
	quotedRole := pgx.Identifier{role}.Sanitize()
	_, err = setup.ExecContext(ctx, "CREATE ROLE "+quotedRole+" LOGIN PASSWORD 'fixture-query-only'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := setup.ExecContext(context.Background(), "DROP OWNED BY "+quotedRole+"; DROP ROLE "+quotedRole)
		require.NoError(t, err)
	})
	schema := pgx.Identifier{cfg.Schema}.Sanitize()
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".photo_hidden_credentials TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(state,cursor,total_objects,completed_objects,copied_objects,copied_bytes,cancel_requested,error,receipt_json,updated_at,finished_at,retention_until) ON "+schema+".storage_operations TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+schema+" TO "+quotedRole+
		"; GRANT SELECT ON ALL TABLES IN SCHEMA "+schema+" TO "+quotedRole+
		"; GRANT SELECT,INSERT,UPDATE,DELETE ON "+schema+".nodes,"+schema+".blobs,"+schema+".content_versions TO "+quotedRole+
		"; GRANT SELECT,INSERT ON "+schema+".blob_checksums TO "+quotedRole+
		"; GRANT SELECT ON "+schema+".audit_records,"+schema+".audit_authority,"+schema+".audit_scopes,"+schema+".audit_baselines,"+schema+".audit_memberships TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".audit_records TO "+quotedRole+
		"; GRANT UPDATE(operation_sequence_high_water,allocation_entry_count,allocation_head) ON "+schema+".audit_authority TO "+quotedRole+
		"; GRANT UPDATE(entry_count,chain_head) ON "+schema+".audit_scopes TO "+quotedRole+
		"; GRANT SELECT,UPDATE(high_water) ON "+schema+".node_allocation TO "+quotedRole+
		"; GRANT SELECT,INSERT,DELETE ON "+schema+".vault_changes TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT SELECT,INSERT ON "+schema+".photo_xmp_unsettled TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".audit_authority,"+schema+".audit_scopes,"+schema+".audit_baselines,"+schema+".audit_memberships TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE ON "+schema+".blob_locations TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".blob_pack_entries,"+schema+".rendition_blob_staging,"+schema+".derivative_blob_purge_pending,"+schema+".photo_xmp_unsettled TO "+quotedRole+
		"; GRANT UPDATE(live_entries,live_stored_bytes,live_raw_bytes,max_live_stored_len,max_live_raw_len) ON "+schema+".blob_packs TO "+quotedRole+
		"; GRANT INSERT,UPDATE(next_attempt_at) ON "+schema+".text_extraction_queue TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".text_searchable_versions TO "+quotedRole+
		"; GRANT INSERT,UPDATE(extractor_version,status,error,attempts,text,extracted_at) ON "+schema+".extracted_text TO "+quotedRole+
		"; GRANT INSERT,UPDATE(hidden_at,excluded_at,display_file_id,display_override_file_id,revision,updated_at) ON "+schema+".photo_assets TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_change_receipts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(asset_id,role,sidecar_of_file_id,source_provenance_identity,revision,rating,flag,label,caption,creator,copyright,rotation,confirmed_fields,flag_owner) ON "+schema+".photo_files TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_timeline TO "+quotedRole+
		"; GRANT INSERT,UPDATE(original_path,original_mtime,folder_key) ON "+schema+".photo_source_claims TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE ON "+schema+".ingests TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".provenance,"+schema+".provenance_version_bindings TO "+quotedRole+
		"; GRANT INSERT,UPDATE,DELETE ON "+schema+".collection_labels TO "+quotedRole+
		"; GRANT INSERT,UPDATE ON "+schema+".watch_sources,"+schema+".push_sources TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".watch_source_absences TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".document_event_heads,"+schema+".document_event_generations,"+schema+".document_people_heads,"+schema+".document_people TO "+quotedRole+
		"; GRANT INSERT,UPDATE(revision,reason) ON "+schema+".document_event_dirty TO "+quotedRole+
		"; GRANT UPDATE(publication_epoch,updated_at) ON "+schema+".document_people_state,"+schema+".document_event_state TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT DELETE ON "+schema+".rendition_heads,"+schema+".rendition_attachments,"+schema+".rendition_job_waiters,"+schema+".email_attachments,"+schema+".embedding_jobs,"+schema+".embedding_heads,"+schema+".embedding_sets,"+schema+".photo_files TO "+quotedRole+
		"; GRANT DELETE,UPDATE(state,phase,claim_owner,lease_expires_at,claim_epoch,available_at,selected_waiter_id,authorization_grant_id,authorization_incarnation_id,authorization_revocation_fence,provider_started,provider_resume_handle,execution_snapshot_json,lexical_generation_id,failure_code,updated_at) ON "+schema+".rendition_jobs TO "+quotedRole+
		"; GRANT UPDATE(state,failure_code,updated_at) ON "+schema+".rendition_job_waiters TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(root_kind,target_kind,target_id,fencing_token,recorded_at,expires_at,active,released_at) ON "+schema+".current_rendition_roots TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(name,revision,assignment_count) ON "+schema+".tags TO "+quotedRole+
		"; GRANT UPDATE(binding_epoch) ON "+schema+".document_people_state TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT DELETE ON "+schema+".embedding_input_generations,"+schema+".embedding_vector_sets,"+schema+".embedding_vector_spaces,"+schema+".embedding_failures,"+schema+".email_heads,"+schema+".email_part_artifacts,"+schema+".email_generations,"+schema+".email_body_results,"+schema+".rendition_builds,"+schema+".rendition_artifacts,"+schema+".rendition_units,"+schema+".rendition_lexical_segments,"+schema+".rendition_lexical_index,"+schema+".rendition_lexical_heads,"+schema+".rendition_lexical_generations,"+schema+".rendition_lexical_generation_manifests,"+schema+".text_searchable_versions,"+schema+".extracted_text,"+schema+".text_extraction_queue TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".rendition_lexical_stream_builds TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".derivative_blob_purge_pending,"+schema+".rendition_lexical_generations,"+schema+".rendition_lexical_stream_builds,"+schema+".rendition_lexical_generation_manifests,"+schema+".rendition_lexical_index TO "+quotedRole+
		"; GRANT UPDATE(generation_id) ON "+schema+".rendition_lexical_heads TO "+quotedRole+
		"; GRANT INSERT,UPDATE(purged_at,active,superseded_at,superseding_build_id) ON "+schema+".derivative_purge_suppressions TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(scan_hash) ON "+schema+".blob_packs TO "+quotedRole+
		"; GRANT INSERT,UPDATE ON "+schema+".blob_pack_entries TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".blob_locations TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".gc_loose_retirements,"+schema+".derivative_pack_purge_pending TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE ON "+schema+".node_tags TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".batch_tag_receipts TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(name,description,payload,fingerprint,revision,updated_at) ON "+schema+".saved_queries TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".source_metadata_generations,"+schema+".document_event_generations,"+schema+".document_events,"+schema+".document_event_actors,"+schema+".document_event_primaries TO "+quotedRole+
		"; GRANT INSERT,UPDATE(generation_id,published_at) ON "+schema+".source_metadata_heads TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_technical_metadata TO "+quotedRole+
		"; GRANT INSERT,UPDATE(projection_recipe) ON "+schema+".photo_technical_metadata_state TO "+quotedRole+
		"; GRANT INSERT,UPDATE(deriver_fingerprint,input_epoch) ON "+schema+".document_event_state TO "+quotedRole+
		"; GRANT INSERT,UPDATE(generation_id,input_epoch,published_at) ON "+schema+".document_event_heads TO "+quotedRole+
		"; GRANT INSERT,UPDATE(input_epoch,input_revision,inputs_sha256,state,diagnostic_json,attempted_at) ON "+schema+".document_event_attempts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,scanned,published,failed,unavailable,updated_at,finished_at) ON "+schema+".document_event_builds TO "+quotedRole)
	require.NoError(t, err)
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".rendition_blob_staging,"+schema+".processing_consent_grants,"+schema+".processing_consent_revocations,"+schema+".processing_profiles,"+schema+".rendition_builds,"+schema+".rendition_artifacts,"+schema+".rendition_units,"+schema+".rendition_lexical_segments,"+schema+".rendition_attachments,"+schema+".rendition_jobs,"+schema+".rendition_job_waiters,"+schema+".rendition_lexical_heads,"+schema+".rendition_lexical_superseded TO "+quotedRole+
		"; GRANT INSERT,UPDATE(attachment_id,published_at) ON "+schema+".rendition_heads TO "+quotedRole+
		"; GRANT UPDATE(provider_attempts) ON "+schema+".rendition_jobs TO "+quotedRole+
		"; GRANT UPDATE(attachment_id) ON "+schema+".rendition_job_waiters TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".embedding_vector_spaces,"+schema+".embedding_input_generations,"+schema+".embedding_generation_inputs,"+schema+".embedding_vector_sets,"+schema+".embedding_vector_rows,"+schema+".embedding_sets TO "+quotedRole+
		"; GRANT INSERT,UPDATE(embedding_set_id,vector_space_id,profile_fingerprint,published_at,fencing_token) ON "+schema+".embedding_heads TO "+quotedRole+
		"; GRANT INSERT,UPDATE(failure_code,failed_at,fencing_token,attachment_id) ON "+schema+".embedding_failures TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,claim_owner,claim_epoch,claim_count,lease_expires_at,available_at,failure_code,receipt_json,updated_at) ON "+schema+".embedding_jobs TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".page_documents,"+schema+".page_frames,"+schema+".page_recipes,"+schema+".page_images,"+schema+".email_generations,"+schema+".email_part_artifacts,"+schema+".email_attachments,"+schema+".email_body_results,"+schema+".email_document_publications,"+schema+".email_document_relations TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,results_json,failure_code,epoch,token,updated_at) ON "+schema+".page_render_jobs TO "+quotedRole+
		"; GRANT INSERT,UPDATE(attachment_id,published_at) ON "+schema+".email_heads TO "+quotedRole+
		"; GRANT DELETE ON "+schema+".email_document_publications TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".media_sources,"+schema+".media_source_versions,"+schema+".media_input_artifacts,"+schema+".media_occurrences,"+schema+".media_operations TO "+quotedRole+
		"; GRANT UPDATE(source_version_id,visible,revoked_at) ON "+schema+".media_occurrences TO "+quotedRole+
		"; GRANT UPDATE(receipt_json,updated_at) ON "+schema+".media_operations TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".visual_preview_generations TO "+quotedRole+
		"; GRANT INSERT,UPDATE(generation_id,published_at) ON "+schema+".visual_preview_heads TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(last_operation_id,destination,destination_identity,state,destination_reserved,error,retention_released,retired_at,retirement_requested) ON "+schema+".photo_checkouts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(published,base_held,observed_identity,observed_hash,observed_size,observed_mtime,observed_since,working_state,version_id,blob_hash,size) ON "+schema+".photo_checkout_entries TO "+quotedRole)
	require.NoError(t, err)

