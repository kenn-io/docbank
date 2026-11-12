package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
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
	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(lifecycle) ON "+schema+".blob_stores TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".storage_operation_stores TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".storage_operation_cleanup TO "+quotedRole)
	require.NoError(t, err)

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

	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(canonical_json,state,expires_at) ON "+schema+".export_sources TO "+quotedRole+
		"; GRANT INSERT ON "+schema+".export_chunks,"+schema+".export_members,"+schema+".export_documents,"+schema+".export_role_roots TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(canonical_json,expires_at) ON "+schema+".export_plans TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(canonical_json,state,expires_at,epoch,token,archive_name) ON "+schema+".export_jobs TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".collection_snapshots,"+schema+".collection_snapshot_members,"+schema+".collection_snapshot_representations,"+schema+".packages,"+schema+".package_volumes,"+schema+".bates_namespaces,"+schema+".bates_namespace_cursors,"+schema+".bates_allocations,"+schema+".bates_page_labels,"+schema+".bates_artifacts,"+schema+".bates_artifact_pages TO "+quotedRole+
		"; GRANT UPDATE(next_sequence) ON "+schema+".bates_namespace_cursors TO "+quotedRole+
		"; GRANT UPDATE(state,committed_at) ON "+schema+".bates_allocations TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".package_preflights TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".package_records,"+schema+".package_labels,"+schema+".package_import_receipts,"+schema+".package_import_jobs TO "+quotedRole+
		"; GRANT UPDATE(state,snapshot_id,completed_at) ON "+schema+".packages TO "+quotedRole+
		"; GRANT UPDATE(state,epoch,token,claim_owner,lease_expires_at,updated_at) ON "+schema+".package_import_jobs TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,DELETE,UPDATE(display_name,display_name_folded,state,revision,updated_at,key_face_id) ON "+schema+".persons TO "+quotedRole+
		"; GRANT INSERT,UPDATE(kind,display_name,display_name_folded,legal_name,country,own,person_id,revision,updated_at,retired_at) ON "+schema+".parties TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(title,contract_type,status,starts_on,expires_on,signed_on,summary,purpose,consideration,termination,revision,updated_at) ON "+schema+".contracts TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".contract_parties TO "+quotedRole+
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
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(name,starred,revision,cover_asset_id,updated_at,deleted_at) ON "+schema+".photo_sets TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_set_members TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(preference,revision,updated_at,burst_gap_seconds,backup_interval_seconds,backup_keep_last,hidden_unlock_lifetime_seconds,representative_sequence) ON "+schema+".photo_library_settings TO "+quotedRole+
		"; GRANT DELETE,UPDATE(passcode_hash) ON "+schema+".photo_hidden_credentials TO "+quotedRole+
		"; GRANT INSERT,DELETE ON "+schema+".photo_hidden_failures TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(locked_until) ON "+schema+".photo_hidden_lockout TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(state,focus,blur,brightness,color_red,color_green,color_blue,framing,aesthetics,face_reports_json,face_inputs) ON "+schema+".photo_quality_signals TO "+quotedRole)
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
		"; GRANT INSERT,UPDATE(hidden_at,excluded_at,display_file_id,display_override_file_id,representative_rank,revision,updated_at) ON "+schema+".photo_assets TO "+quotedRole+
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
	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".document_people_generations,"+schema+".document_people TO "+quotedRole+
		"; GRANT INSERT,UPDATE(event_generation_id,inputs_sha256,generation_id,unresolved_actors,suppressed_actors,candidate_overflow,resolver_fingerprint,binding_epoch,node_revision,edge_count,state,failure_reason,published_at) ON "+schema+".document_people_heads TO "+quotedRole+
		"; GRANT UPDATE(resolver_fingerprint) ON "+schema+".document_people_state TO "+quotedRole+
		"; GRANT INSERT,UPDATE(state,scanned,published,failed,updated_at,finished_at) ON "+schema+".document_people_builds TO "+quotedRole)
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

	_, err = setup.ExecContext(ctx, "GRANT INSERT ON "+schema+".photo_description_explicit_waiters,"+schema+".photo_face_scans TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(producer_fingerprint,caption,tags_json,visible_text,rotation,dismissed,awaiting,caption_resolved,resolved_tags_json) ON "+schema+".photo_suggestions TO "+quotedRole+
		"; GRANT INSERT,DELETE,UPDATE(x,y,width,height,roll,facing,eyes_open,smile,detector_fingerprint,revision) ON "+schema+".photo_faces TO "+quotedRole)
	require.NoError(t, err)

	_, err = setup.ExecContext(ctx, "GRANT INSERT,UPDATE(last_operation_id,destination,destination_identity,state,destination_reserved,error,retention_released,retired_at,retirement_requested) ON "+schema+".photo_checkouts TO "+quotedRole+
		"; GRANT INSERT,UPDATE(published,base_held,observed_identity,observed_hash,observed_size,observed_mtime,observed_since,working_state,version_id,blob_hash,size) ON "+schema+".photo_checkout_entries TO "+quotedRole)
	require.NoError(t, err)

	config, err := pgx.ParseConfig(os.Getenv("DOCBANK_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	require.NoError(t, setup.QueryRowContext(ctx, `SELECT current_database()`).Scan(&config.Database))
	config.User, config.Password = role, "fixture-query-only"
	config.RuntimeParams["search_path"] = schema + ",pg_catalog"
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	actual, err := pgschema.Validate(ctx, db, cfg)
	require.NoError(t, err)
	require.Equal(t, id, actual)
	s := &Store{db: db, writeDB: db, postgresSchema: cfg.Schema, providerEgressMu: waitlock.New(), photoHiddenSessions: make(map[string]time.Time)}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT id,(SELECT vault_uid FROM vault_metadata) FROM nodes WHERE parent_id IS NULL`).Scan(&s.rootID, &s.vaultID))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT store_id FROM blob_stores WHERE role='primary'`).Scan(&s.primaryStoreID))
	return s
}

func checkNodeQueriesSQLite(t *testing.T) {
	t.Helper()
	t.Parallel()
	checkNodeQueries(t, newTestStore(t))
}

func checkNodeQueriesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	checkNodeQueries(t, s)
	t.Run("directory snapshot", func(t *testing.T) { checkDirectorySnapshotPostgres(t, s) })
}

func checkDirectorySnapshotPostgres(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	const recordedAt = "2026-01-01T00:00:00.000000000Z"
	tx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	dirID, err := s.insertNodeTx(ctx, tx, s.RootID(), "snapshot-before", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	childID, err := s.insertNodeTx(ctx, tx, dirID, "original", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	// Reuse this test's database with a pgx tracer to commit after the first
	// SELECT. Every query still runs against PostgreSQL, including the real
	// DirectoryChildrenPage transaction and its subsequent path/count/page.
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var config *pgx.ConnConfig
	err = conn.Raw(func(driverConn any) error {
		pgConn, ok := driverConn.(*stdlib.Conn)
		require.True(t, ok)
		config = pgConn.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	var writeErr error
	config.Tracer = &afterFirstSelect{run: func() {
		writeErr = s.withStorageTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `UPDATE nodes SET name='snapshot-after' WHERE id=$1`, dirID); err != nil {
				return err
			}
			_, err := s.insertNodeTx(ctx, tx, dirID, "added", nodeKindDir, nil, recordedAt)
			return err
		})
	}}
	readDB := stdlib.OpenDB(*config)
	t.Cleanup(func() { require.NoError(t, readDB.Close()) })
	reader := &Store{db: readDB, rootID: s.RootID()}
	page, err := reader.DirectoryChildrenPage(ctx, dirID, 10, 0)
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.Equal(t, "snapshot-before", page.Directory.Node.Name)
	require.Equal(t, "/snapshot-before", page.Directory.Path)
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Children, 1)
	require.Equal(t, childID, page.Children[0].ID)

	after, err := s.DirectoryChildrenPage(ctx, dirID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, "snapshot-after", after.Directory.Node.Name)
	require.Equal(t, "/snapshot-after", after.Directory.Path)
	require.Equal(t, 2, after.Total)
	require.Len(t, after.Children, 2)
	require.Equal(t, "added", after.Children[0].Name)
	require.Equal(t, childID, after.Children[1].ID)
}

type afterFirstSelect struct {
	once sync.Once
	run  func()
}

func (*afterFirstSelect) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (trace *afterFirstSelect) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil && data.CommandTag.Select() {
		trace.once.Do(trace.run)
	}
}

func checkNodeQueries(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	const recordedAt = "2026-01-01T00:00:00.000000000Z"
	const versionID = "11111111-1111-4111-8111-111111111111"
	const operationID = "22222222-2222-4222-8222-222222222222"
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const md5 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tx, err := s.writeDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	dirID, err := s.insertNodeTx(ctx, tx, s.RootID(), "café", nodeKindDir, nil, recordedAt)
	require.NoError(t, err)
	fileID, err := s.insertNodeTx(ctx, tx, dirID, "report.pdf", nodeKindFile, new(versionID), recordedAt)
	require.NoError(t, err)
	require.NotEqual(t, dirID, fileID)
	_, err = tx.ExecContext(ctx, `INSERT INTO blobs(hash, size, created_at) VALUES ($1, 17, $2)`, hash, recordedAt)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO blob_checksums(blob_sha256, md5) VALUES ($1, $2)`, hash, md5)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO content_versions
		(version_id, node_id, blob_hash, size, mime_type, recorded_at, node_revision, introduced_operation_id, transition_kind)
		VALUES ($1, $2, $3, 17, 'application/pdf', $4, 1, $5, 'content_create')`,
		versionID, fileID, hash, recordedAt, operationID)
	require.NoError(t, err)
	inside, err := nodeByIDTx(t.Context(), tx, fileID)
	require.NoError(t, err)
	require.Equal(t, fileID, inside.ID)
	require.NoError(t, tx.Commit())

	dir, err := s.ChildByName(ctx, s.RootID(), "cafe\u0301")
	require.NoError(t, err)
	require.Equal(t, Node{ID: dirID, ParentID: new(s.RootID()), Name: "café", Kind: nodeKindDir,
		Revision: 1, CreatedAt: recordedAt, ModifiedAt: recordedAt}, dir)
	file, err := s.NodeByID(ctx, fileID)
	require.NoError(t, err)
	require.Equal(t, Node{ID: fileID, ParentID: new(dirID), Name: "report.pdf", Kind: nodeKindFile,
		CurrentVersionID: versionID, BlobHash: hash, MD5: md5, Size: 17, MimeType: "application/pdf",
		Revision: 1, CreatedAt: recordedAt, ModifiedAt: recordedAt}, file)
	t.Run("path queries", func(t *testing.T) { checkPathQueries(t, s, dir, file) })

	t.Run("canonical paths and directory pages", func(t *testing.T) {
		view, err := s.NodeViewByID(ctx, fileID)
		require.NoError(t, err)
		require.Equal(t, NodeView{Node: file, Path: "/café/report.pdf"}, view)
		rootPath, err := s.Path(ctx, s.RootID())
		require.NoError(t, err)
		require.Equal(t, "/", rootPath)

		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		folderID, err := s.insertNodeTx(ctx, tx, dirID, "z-folder", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		trashID, err := s.insertNodeTx(ctx, tx, dirID, "discarded-folder", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE nodes SET trashed_at=$1 WHERE id=$2`, recordedAt, trashID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		first, err := s.DirectoryChildrenPage(ctx, dirID, 1, 0)
		require.NoError(t, err)
		require.Equal(t, NodeView{Node: dir, Path: "/café"}, first.Directory)
		require.Equal(t, 2, first.Total)
		require.Len(t, first.Children, 1)
		require.Equal(t, folderID, first.Children[0].ID)
		last, total, err := s.ChildrenPage(ctx, dirID, 1, 1)
		require.NoError(t, err)
		require.Equal(t, 2, total)
		require.Equal(t, []Node{file}, last)
		empty, err := s.DirectoryChildrenPage(ctx, dirID, 1, 2)
		require.NoError(t, err)
		require.Equal(t, 2, empty.Total)
		require.Empty(t, empty.Children)

		_, err = s.DirectoryChildrenPage(ctx, fileID, 1, 0)
		require.ErrorIs(t, err, ErrNotDir)
		_, err = s.DirectoryChildrenPage(ctx, trashID, 1, 0)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.NodeViewByID(ctx, 1<<62)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.Path(ctx, 1<<62)
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("byte-ordered directory pages", func(t *testing.T) {
		var orderedDir int64
		err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
			var err error
			orderedDir, err = s.insertNodeTx(ctx, tx, s.RootID(), "ordered", nodeKindDir, nil, recordedAt)
			if err != nil {
				return err
			}
			for _, name := range []string{"b", "_x", "é", "Z", "a", "B"} {
				if _, err := s.insertNodeTx(ctx, tx, orderedDir, name, nodeKindDir, nil, recordedAt); err != nil {
					return err
				}
			}
			return nil
		})
		require.NoError(t, err)
		var names []string
		for offset := 0; offset < 6; offset += 2 {
			page, err := s.DirectoryChildrenPage(ctx, orderedDir, 2, offset)
			require.NoError(t, err)
			require.Equal(t, 6, page.Total)
			for _, child := range page.Children {
				names = append(names, child.Name)
			}
		}
		require.Equal(t, []string{"B", "Z", "_x", "a", "b", "é"}, names)
	})
	t.Run("node and path share a snapshot", func(t *testing.T) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		parentID, err := s.insertNodeTx(ctx, tx, s.RootID(), "before", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		nodeID, err := s.insertNodeTx(ctx, tx, parentID, "old", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		// Commit on a separate connection after node resolution, before the
		// production nodeView reads its path. No timing race is required.
		view, err := s.nodeView(ctx, func(read *sql.Tx) (Node, error) {
			node, err := s.nodeByPath(ctx, read, "/before/old")
			if err != nil {
				return Node{}, err
			}
			_, err = s.writeDB.ExecContext(ctx, `UPDATE nodes
				SET name=CASE WHEN id=$1 THEN 'after' ELSE 'new' END
				WHERE id=$1 OR id=$2`, parentID, nodeID)
			return node, err
		})
		require.NoError(t, err)
		require.Equal(t, "old", view.Node.Name)
		require.Equal(t, "/before/old", view.Path)

		after, err := s.NodeViewByPath(ctx, "/after/new")
		require.NoError(t, err)
		require.Equal(t, "new", after.Node.Name)
		require.Equal(t, "/after/new", after.Path)
	})
	t.Run("rollback", func(t *testing.T) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		id, err := s.insertNodeTx(ctx, tx, dirID, "discarded", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		_, err = s.NodeByID(ctx, id)
		require.ErrorIs(t, err, ErrNotFound)
		_, err = s.ChildByName(ctx, dirID, "discarded")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("trashed name can be reused", func(t *testing.T) {
		_, err := s.writeDB.ExecContext(ctx, `UPDATE nodes SET trashed_at=$1 WHERE id=$2`, recordedAt, fileID)
		require.NoError(t, err)
		_, err = s.ChildByName(ctx, dirID, "report.pdf")
		require.ErrorIs(t, err, ErrNotFound)
		trashed, err := s.NodeByID(ctx, fileID)
		require.NoError(t, err)
		require.Equal(t, new(recordedAt), trashed.TrashedAt)
		tx, err := s.writeDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		id, err := s.insertNodeTx(ctx, tx, dirID, "report.pdf", nodeKindDir, nil, recordedAt)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		live, err := s.ChildByName(ctx, dirID, "report.pdf")
		require.NoError(t, err)
		require.Equal(t, id, live.ID)
		require.NotEqual(t, fileID, live.ID)
	})
}
