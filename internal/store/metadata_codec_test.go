package store

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docsqlite "go.kenn.io/docbank/sqlite"
)

type metadataCodecScratchKind string

type metadataCodecScratch struct {
	Type      string                   `json:"type"`
	Name      string                   `json:"name" db:"name"`
	Note      *string                  `json:"note" db:"note"`
	Count     *int64                   `json:"count" db:"count"`
	Kind      metadataCodecScratchKind `json:"kind" db:"kind"`
	Enabled   bool                     `json:"enabled" db:"enabled"`
	Tags      []string                 `json:"tags" db:"tags_json,json"`
	Raw       jsontext.Value           `json:"raw" db:"raw_json,json"`
	Blob      jsontext.Value           `json:"blob" db:"blob"`
	State     string                   `json:"state"`
	Singleton int                      `json:"-" db:"singleton"`
	Optional  *string                  `json:"optional,omitempty" db:"optional"`
}

func TestMetadataTableCodec(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := defaultSQLiteDriver().Open(filepath.Join(t.TempDir(), "codec.db"),
		docsqlite.OpenOptions{Access: docsqlite.Create, TransactionMode: docsqlite.Immediate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `CREATE TABLE scratch(name TEXT NOT NULL, note TEXT, count INTEGER,
		kind TEXT NOT NULL, enabled INTEGER NOT NULL, tags_json TEXT NOT NULL, raw_json TEXT NOT NULL,
		blob BLOB NOT NULL, singleton INTEGER NOT NULL, optional TEXT)`)
	require.NoError(t, err)
	table := newMetadataTable(metadataTable[metadataCodecScratch]{
		record: metadataCodecScratch{Type: "scratch_record", State: "template", Singleton: 1},
		table:  "scratch", suffix: "ORDER BY name", checkExport: true,
		validate: func(v metadataCodecScratch) error {
			if v.Name == "rejected" {
				return errors.New("rejected scratch")
			}
			return nil
		}})

	required, nullable := table.fields()
	assert.Equal(t, strings.Fields("type name note count kind enabled tags raw blob state"), required)
	assert.Equal(t, map[string]bool{"note": true, "count": true}, nullable)
	assert.Equal(t, "scratch_record", table.kind())
	assert.Equal(t, "name,note,count,kind,enabled,tags_json,raw_json,blob,singleton,optional", table.plan.list)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, line := range []string{
		`{"type":"scratch_record","name":"b","note":null,"count":null,"kind":"k","enabled":false,"tags":[ "z", "a" ],"raw":{"b":1, "a":2},"blob":"x","state":"template"}`,
		`{"type":"scratch_record","name":"a","note":"n","count":7,"kind":"k","enabled":true,"tags":[],"raw":[1],"blob":{"k":1},"state":"template"}`,
	} {
		require.NoError(t, table.importRecord(ctx, tx, jsontext.Value(line)))
	}
	require.EqualError(t, table.importRecord(ctx, tx, jsontext.Value(
		`{"type":"scratch_record","name":"rejected","note":null,"count":null,"kind":"k","enabled":false,"tags":[],"raw":1,"blob":1,"state":"template"}`)),
		"rejected scratch")
	require.NoError(t, tx.Commit())

	var stored []string
	rows, err := db.QueryContext(ctx, `SELECT name||'|'||tags_json||'|'||raw_json||'|'||typeof(blob)||'|'||singleton||'|'||typeof(note) FROM scratch ORDER BY name`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var row string
		require.NoError(t, rows.Scan(&row))
		stored = append(stored, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{`a|[]|[1]|blob|1|text`, `b|["z","a"]|{"b":1, "a":2}|blob|1|null`}, stored)

	var exported []metadataCodecScratch
	write := func(value any) error {
		record, ok := value.(metadataCodecScratch)
		require.True(t, ok)
		exported = append(exported, record)
		return nil
	}
	require.NoError(t, table.export(ctx, db, write))
	require.Len(t, exported, 2)
	assert.Equal(t, metadataCodecScratch{Type: "scratch_record", Name: "a", Note: new("n"), Count: new(int64(7)),
		Kind: "k", Enabled: true, Tags: []string{}, Raw: jsontext.Value(`[1]`), Blob: jsontext.Value(`{"k":1}`),
		State: "template", Singleton: 1}, exported[0])
	assert.Nil(t, exported[1].Note)
	assert.Nil(t, exported[1].Count)
	assert.Equal(t, jsontext.Value(`{"b":1, "a":2}`), exported[1].Raw)
	require.NoError(t, table.validateRows(ctx, db))

	_, err = db.ExecContext(ctx, `INSERT INTO scratch VALUES('rejected',NULL,NULL,'k',0,'[]','1',X'31',1,NULL)`)
	require.NoError(t, err)
	exported = nil
	require.EqualError(t, table.export(ctx, db, write),
		"validating scratch record metadata for export: rejected scratch")
	assert.Len(t, exported, 2)
	require.EqualError(t, table.validateRows(ctx, db),
		"validating scratch record metadata for export: rejected scratch")

	type duplicate struct {
		Type string `json:"type"`
		A    string `json:"a" db:"x"`
		B    string `json:"b" db:"x"`
	}
	type badOption struct {
		Type string `json:"type"`
		A    string `json:"a" db:"x,text"`
	}
	assert.Panics(t, func() { newMetadataTable(metadataTable[metadataCodecScratch]{table: "scratch"}) })
	assert.Panics(t, func() { newMetadataTable(metadataTable[duplicate]{record: duplicate{Type: "d"}, table: "d"}) })
	assert.Panics(t, func() { newMetadataTable(metadataTable[badOption]{record: badOption{Type: "b"}, table: "b"}) })
	assert.Panics(t, func() {
		newMetadataTable(metadataTable[metadataCodecScratch]{record: metadataCodecScratch{Type: "scratch_record"}})
	})
}

func TestMetadataCodecFieldParity(t *testing.T) {
	t.Parallel()
	require.Len(t, metadataCodecs, len(metadataCodecBaseRequiredFields))
	for kind, fields := range metadataCodecBaseRequiredFields {
		codec, ok := metadataCodecs[kind]
		require.True(t, ok, kind)
		required, nullable := codec.fields()
		assert.Equal(t, strings.Fields(fields), required, kind)
		assert.ElementsMatch(t, strings.Fields(metadataCodecBaseNullableFields[kind]), slices.Collect(maps.Keys(nullable)), kind)
		assert.Equal(t, required, metadataRequiredFields[kind], kind)
		assert.ElementsMatch(t, slices.Collect(maps.Keys(nullable)), slices.Collect(maps.Keys(metadataNullableFields[kind])), kind)
	}
	for kind, fields := range metadataRequiredFields {
		if _, ok := metadataCodecs[kind]; !ok {
			assert.Equal(t, mediaMetadataRequiredFields[kind], fields, "hand-kept kind %s", kind)
		}
	}
	for kind, fields := range metadataNullableFields {
		if _, ok := metadataCodecs[kind]; !ok {
			assert.Equal(t, mediaMetadataNullableFields[kind], fields, "hand-kept kind %s", kind)
		}
	}
	assert.Len(t, metadataRequiredFields, len(metadataCodecs)+len(mediaMetadataRequiredFields))
}

func TestMetadataCodecPristineTableParity(t *testing.T) {
	t.Parallel()
	var derived []string
	for term := range strings.SplitSeq(pristineMetadataTables, " + ") {
		derived = append(derived, strings.TrimSuffix(strings.TrimPrefix(term, "(SELECT COUNT(*) FROM "), ")"))
	}
	assert.ElementsMatch(t, strings.Fields(metadataCodecBasePristineTables), derived)
	s := newTestStore(t)
	for _, table := range derived {
		var exists bool
		require.NoError(t, s.db.QueryRowContext(t.Context(),
			`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name=?)`, table).Scan(&exists))
		assert.True(t, exists, table)
	}
}

func TestMetadataCodecCheckedExportKinds(t *testing.T) {
	t.Parallel()
	var checked []string
	for kind, codec := range metadataCodecs {
		if reflect.ValueOf(codec).Elem().FieldByName("checkExport").Bool() {
			checked = append(checked, kind)
		}
	}
	assert.ElementsMatch(t, strings.Fields(`node content_version ingest provenance watch_source tag node_tag
		collection_label provenance_version_binding saved_query batch_tag_receipt photo_asset photo_file
		photo_library_settings photo_change_receipt person person_identity person_external_identity
		person_external_uid_alias person_alias person_merge person_split custodian_assignment
		person_document_assertion person_match_candidate rendition_job_waiter`), checked)
}

func TestMetadataCodecGoldenCoversEveryKind(t *testing.T) {
	t.Parallel()
	kinds := map[string]bool{}
	for _, path := range []string{metadataCodecGoldenPath} {
		golden, err := os.ReadFile(path)
		require.NoError(t, err)
		for line := range bytes.SplitSeq(bytes.TrimSpace(golden), []byte{'\n'}) {
			kinds[metadataCodecGoldenLineType(t, line)] = true
		}
	}
	for kind := range metadataCodecs {
		assert.True(t, kinds[kind], "golden lacks %s", kind)
	}
}

func TestMetadataCodecEmbeddingSchemaParity(t *testing.T) {
	t.Parallel()
	for _, codec := range embeddingMetadataTables {
		list := reflect.ValueOf(codec).Elem().FieldByName("plan").Elem().FieldByName("list").String()
		if list == "" {
			continue
		}
		index := slices.IndexFunc(embeddingCatalogSchema, func(schema embeddingCatalogTableSchema) bool {
			return schema.name == codec.sqlTable()
		})
		require.GreaterOrEqual(t, index, 0, codec.kind())
		assert.ElementsMatch(t, embeddingCatalogSchema[index].columns, strings.Split(list, ","), codec.kind())
	}
}

// metadataCodecBaseRequiredFields and metadataCodecBaseNullableFields copy the
// hand-kept field maps at 0af2361e for every registered kind.
var metadataCodecBaseRequiredFields = map[string]string{
	"audit_authority":                    "type lineage_id operation_sequence_high_water allocation_genesis_digest allocation_entry_count allocation_head",
	"audit_membership":                   "type scope_id node_id baseline_digest",
	"audit_record":                       "type digest record",
	"audit_scope":                        "type scope_id target_node_id enable_operation_id entry_count chain_head",
	"batch_tag_receipt":                  "type operation_id request_digest receipt_json",
	"bates_allocation":                   "type allocation_id operation_id namespace_id snapshot_id request_sha256 recipe_sha256 start_sequence end_sequence state created_at committed_at",
	"bates_artifact":                     "type artifact_id allocation_id blob_sha256 size media_type page_count recipe_json manifest_sha256 state created_at",
	"bates_artifact_page":                "type artifact_id ordinal occurrence_id source_blob_sha256 source_page output_page label",
	"bates_namespace":                    "type namespace_id prefix suffix padding created_at",
	"bates_namespace_cursor":             "type namespace_id next_sequence",
	"bates_page_label":                   "type allocation_id ordinal namespace_id sequence occurrence_id source_page output_page label",
	"blob":                               "type hash size created_at",
	"blob_checksum":                      "type blob_sha256 md5",
	"collection_label":                   "type ingest_id label revision updated_at",
	"collection_snapshot":                "type snapshot_id vault_id canonical_json checksum",
	"collection_snapshot_member":         "type snapshot_id ordinal canonical_json checksum",
	"collection_snapshot_representation": "type snapshot_id occurrence_id role ordinal canonical_json checksum",
	"content_version":                    "type version_id node_id blob_hash size mime_type recorded_at node_revision introduced_operation_id transition_kind source_version_id",
	"current_rendition_root":             "type root_id root_kind target_kind target_id fencing_token recorded_at active released_at",
	"custodian_assignment":               "type assignment_id scope_kind ingest_id package_id package_record_id node_id content_version_id person_id raw_label raw_label_folded rank basis source_ref revision recorded_at retired_at",
	"derivative_purge_suppression":       "type source_sha256 profile_fingerprint build_id purged_at active superseded_at superseding_build_id",
	"email_attachment":                   "type attachment_id content_version_id generation_id attached_at",
	"email_body_result":                  "type email_attachment_id body_recipe_fingerprint state part_path rendition_attachment_id reason",
	"email_document_publication":         "type request receipt",
	"email_generation":                   "type generation_id source_sha256 source_size recipe_fingerprint canonical_json checksum created_at",
	"email_head":                         "type content_version_id attachment_id published_at",
	"email_part_artifact":                "type generation_id part_path role blob_hash size",
	"embedding_failure":                  "type content_version_id profile_fingerprint binding_id input_kind failure_code failed_at fencing_token attachment_id",
	"embedding_generation_input":         "type generation_id input_id order rendered_checksum",
	"embedding_head":                     "type content_version_id binding_id input_kind embedding_set_id vector_space_id profile_fingerprint published_at fencing_token",
	"embedding_input_generation":         "type generation_id generation_blob_hash generation_encoded_size generation_checksum source_version_id profile_fingerprint evidence_fingerprint tokenizer_fingerprint chunk_policy_fingerprint formatter_fingerprint attachment_context_fingerprint attachment_id input_count created_at",
	"embedding_set":                      "type embedding_set_id vault_id binding_id input_kind content_version_id profile_fingerprint embedding_input_fingerprint vector_space_id generation_id vector_set_id created_at",
	"embedding_vector_row":               "type vector_set_id row_id order input_id dimensions checksum",
	"embedding_vector_set":               "type vector_set_id contract_version vector_space_id payload_blob_hash payload_size payload_checksum manifest_checksum row_count dimensions",
	"embedding_vector_space":             "type vector_space_id contract_version descriptor_json provider_descriptor provider_revision descriptor_fingerprint compatibility_id dimensions metric normalization scalar_encoding document_formatter query_formatter model_input_fingerprint",
	"export_authority":                   "type kind id ordinal retain_until canonical_json checksum",
	"extracted_text":                     "type blob_hash extractor extractor_version status error attempts text extracted_at",
	"ingest":                             "type ingest_id started_at source_kind source_desc",
	"mailbox_archive":                    "type archive",
	"mailbox_container":                  "type container",
	"mailbox_job":                        "type job",
	"mailbox_occurrence":                 "type occurrence",
	"mailbox_transfer_head":              "type archive_id reference receipt_id",
	"mailbox_transfer_receipt":           "type receipt",
	"node":                               "type id parent_id name kind current_version_id revision created_at modified_at trashed_at trash_parent trash_name",
	"node_tag":                           "type node_id tag_id",
	"package":                            "type package_id canonical_json checksum",
	"package_import_head":                "type canonical_json checksum",
	"package_import_job":                 "type canonical_json checksum",
	"package_import_receipt":             "type canonical_json checksum",
	"package_label":                      "type canonical_json checksum",
	"package_record":                     "type canonical_json checksum",
	"package_volume":                     "type package_id ordinal canonical_json checksum",
	"page_document":                      "type canonical_json checksum",
	"page_image":                         "type canonical_json checksum",
	"page_recipe":                        "type canonical_json checksum",
	"page_render_job":                    "type canonical_json checksum",
	"person":                             "type person_id display_name display_name_folded origin state revision created_at updated_at",
	"person_alias":                       "type retired_person_id surviving_person_id reason retired_at",
	"person_document_assertion":          "type assertion_id content_version_id person_id role action note recorded_at revision",
	"person_external_identity":           "type person_id system archive_id uid uid_kind uid_state last_seen_revision display_name_snapshot linked_at updated_at",
	"person_external_uid_alias":          "type system archive_id retired_uid surviving_uid observed_at",
	"person_identity":                    "type identity_id person_id kind value_normalized value_display scope_kind scope_value normalization origin evidence_kind evidence_id confidence recorded_at",
	"person_match_candidate":             "type candidate_id actor_key display_name suggested_person_id reason evidence_json evidence_sha256 occurrence_count revision state decided_person_id created_at decided_at",
	"person_merge":                       "type merge_id operation_id request_sha256 survivor_person_id absorbed_person_id absorbed_display_name moved_json survivor_revision_before survivor_revision_after created_at",
	"person_split":                       "type operation_id request_sha256 receipt_json created_at",
	"photo_asset":                        "type asset_id kind revision excluded_at display_file_id display_override_file_id created_at updated_at",
	"photo_change_receipt":               "type receipt_id operation asset_id settings_key before_revision after_revision before_json after_json created_at",
	"photo_file":                         "type file_id asset_id node_id role sidecar_of_file_id created_at",
	"photo_library_settings":             "type preference revision updated_at",
	"processing_consent_grant":           "type grant_id consent_set_id vault_id incarnation_id principal scope profile_fingerprint disclosure_fingerprint input_classes retained_artifact_classes revocation_fence issued_at expires_at",
	"processing_consent_revocation":      "type revocation_id vault_id incarnation_id principal scope fence revoked_at",
	"processing_incarnation":             "type incarnation_id created_at",
	"processing_profile":                 "type profile_fingerprint canonical_profile rendition_request_fingerprint evidence_lexical_fingerprint retention_disclosure_fingerprint attachment_policy_fingerprint consent_fingerprint rendition_disclosure_fingerprint trust_boundary",
	"provenance":                         "type identity node_id ingest_id original_path original_mtime supersedes",
	"provenance_version_binding":         "type provenance_identity content_version_id observed_at basis_ref",
	"rendition_artifact":                 "type build_id artifact_id role blob_hash size checksum state",
	"rendition_attachment":               "type attachment_id vault_id content_version_id build_id processing_profile_fingerprint retention_disclosure_fingerprint attachment_policy_fingerprint consent_fingerprint rendition_disclosure_fingerprint trust_boundary attached_at",
	"rendition_build":                    "type build_id vault_id source_sha256 rendition_request_fingerprint evidence_lexical_fingerprint captured_artifact_policy_fingerprint captured_artifact_policy authorization_checksum provider_operation_id provider_receipt evidence_checksum rendition_checksum markdown_checksum completeness partial_success truncated warnings completed_at declared_artifact_count unit_count lexical_segment_count",
	"rendition_head":                     "type content_version_id processing_profile_fingerprint attachment_id published_at",
	"rendition_job":                      "type job_id vault_id source_sha256 rendition_request_fingerprint evidence_lexical_fingerprint captured_artifact_policy_fingerprint captured_artifact_policy execution_identity_fingerprint execution_identity execution_snapshot state phase claim_owner claim_epoch lease_expires_at available_at provider_started provider_attempts provider_resume_handle selected_waiter_id authorization_grant_id authorization_incarnation_id authorization_revocation_fence lexical_generation_id failure_code created_at updated_at",
	"rendition_job_waiter":               "type waiter_id job_id content_version_id profile_fingerprint principal scope disclosure_fingerprint input_classes retained_classes authorization_grant_id authorization_incarnation_id authorization_revocation_fence state attachment_id created_at updated_at",
	"rendition_lexical_generation":       "type generation_id segment_count manifest_digest build_ids build_digest built_at headed",
	"rendition_lexical_segment":          "type build_id segment_id unit_id order char_start char_end checksum text",
	"rendition_unit":                     "type build_id unit_id evidence_unit_id order checksum heading_path locator",
	"saved_query":                        "type saved_query_id name description kind payload fingerprint revision created_at updated_at",
	"saved_query_run":                    "type run_id saved_query_id saved_query_revision query_fingerprint snapshot_id member_hash total total_bytes ran_at expires_at previous_run_id previous_member_hash previous_total previous_query_fingerprint",
	"source_metadata_generation":         "type generation_id source_sha256 contract_version extractor_fingerprint canonical_json checksum created_at",
	"source_metadata_head":               "type source_sha256 generation_id published_at",
	"tag":                                "type tag_id name revision",
	"term_report_history":                "type id parent_id observed_at request_json summary_json",
	"visual_preview_generation":          "type generation_id vault_id content_version_id source_sha256 contract_version recipe_fingerprint canonical_result checksum created_at",
	"visual_preview_head":                "type content_version_id generation_id published_at",
	"watch_source":                       "type watch_name source_ref node_id blob_hash size",
}

var metadataCodecBaseNullableFields = map[string]string{
	"bates_allocation":             "committed_at",
	"collection_label":             "label",
	"content_version":              "mime_type source_version_id",
	"current_rendition_root":       "released_at",
	"custodian_assignment":         "content_version_id ingest_id node_id package_id package_record_id person_id retired_at",
	"derivative_purge_suppression": "superseded_at superseding_build_id",
	"email_body_result":            "part_path reason rendition_attachment_id",
	"embedding_input_generation":   "attachment_id",
	"extracted_text":               "error text",
	"node":                         "current_version_id parent_id trash_name trash_parent trashed_at",
	"person_alias":                 "surviving_person_id",
	"person_external_identity":     "last_seen_revision",
	"person_match_candidate":       "decided_at decided_person_id suggested_person_id",
	"photo_asset":                  "display_file_id display_override_file_id excluded_at",
	"photo_change_receipt":         "asset_id settings_key",
	"photo_file":                   "sidecar_of_file_id",
	"photo_library_settings":       "preference",
	"processing_consent_grant":     "expires_at",
	"provenance":                   "original_mtime supersedes",
	"rendition_job":                "authorization_grant_id authorization_incarnation_id authorization_revocation_fence claim_owner execution_snapshot failure_code lease_expires_at lexical_generation_id provider_resume_handle selected_waiter_id",
	"saved_query_run":              "previous_member_hash previous_query_fingerprint previous_run_id previous_total",
}

// metadataCodecBasePristineTables holds the plain table names the pristine
// check at 0af2361e counted: metadata.go:1047-1173 plus package_preflights.
var metadataCodecBasePristineTables = `audit_authority audit_baselines audit_memberships audit_records audit_scopes batch_tag_receipts
bates_allocations bates_artifact_pages bates_artifacts bates_namespace_cursors bates_namespaces
bates_page_labels blob_checksums blobs collection_labels collection_snapshot_members
collection_snapshot_representations collection_snapshots content_fts content_versions
current_rendition_roots custodian_assignments derivative_blob_purge_pending
derivative_pack_purge_pending derivative_purge_suppressions document_event_actors
document_event_attempts document_event_builds document_event_dirty document_event_generations
document_event_heads document_event_primaries document_event_state document_events document_people
document_people_builds document_people_generations document_people_heads email_attachments
email_body_results email_document_publications email_document_relations email_generations
email_heads email_part_artifacts embedding_failures embedding_generation_inputs embedding_heads
embedding_input_generations embedding_sets embedding_vector_rows embedding_vector_sets
embedding_vector_spaces export_jobs export_plans export_sources extracted_text ingests
mailbox_archives mailbox_chunks mailbox_containers mailbox_jobs mailbox_occurrences
mailbox_transfer_heads mailbox_transfer_receipts media_acquisitions media_input_artifacts
media_occurrences media_operations media_protected_refs media_source_heads media_source_versions
media_sources media_visibility_fences node_tags package_import_heads package_import_jobs
package_import_receipts package_labels package_preflights package_records package_volumes packages
page_documents page_frames page_images page_recipes page_render_jobs person_aliases
person_document_assertions person_external_identities person_external_uid_aliases person_identities
person_match_candidates person_merges person_splits persons photo_assets photo_change_receipts
photo_files photo_library_settings processing_consent_grants processing_consent_revocations
processing_profiles provenance provenance_version_bindings rendition_artifacts
rendition_attachments rendition_blob_staging rendition_builds rendition_heads rendition_job_waiters
rendition_jobs rendition_lexical_segments rendition_units saved_queries saved_query_runs
source_metadata_generations source_metadata_heads tags term_report_history text_extraction_queue
text_searchable_versions vector_index_build_jobs vector_index_generations vector_index_heads
vector_index_reader_leases vector_index_unavailable_coverage visual_preview_generations
visual_preview_heads watch_sources`
