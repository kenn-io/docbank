package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type backendColumn struct {
	Kind            string
	Required        bool
	PrimaryPosition int
}

type backendIndex struct {
	Columns []string
	Unique  bool
	Partial bool
}

type backendTable struct {
	Columns     map[string]backendColumn
	ForeignKeys []string
	Indexes     map[string]backendIndex
}

// Inspect the installed schemas, not a second copy of their DDL. Types are
// compared by logical family; engine identity and search structures differ.
// Predicate/expression SQL and deferred-FK timing need operation tests.
func checkBackendSchemaPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	sqlite := newTestStore(t)
	postgres := newPostgresQueryStore(t)
	left := describeBackendSchema(t, sqlite)
	right := describeBackendSchema(t, postgres)
	// SQLite retains numbered released layouts; disposable PostgreSQL layouts
	// identify the embedded SQL directly until retained deployments begin.
	assert.Equal(t, backendColumn{Kind: "integer", Required: true}, left["vault_metadata"].Columns["schema_version"])
	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["vault_metadata"].Columns["schema_fingerprint"])
	delete(left["vault_metadata"].Columns, "schema_version")
	delete(right["vault_metadata"].Columns, "schema_fingerprint")
	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["collection_snapshot_members"].Columns["source_collection_ids_json"])
	delete(right["collection_snapshot_members"].Columns, "source_collection_ids_json")
	assert.Equal(t, backendColumn{Kind: "text"}, right["photo_change_receipts"].Columns["face_version_id"])
	delete(right["photo_change_receipts"].Columns, "face_version_id")
	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["photo_change_receipts"].Columns["query_json"])
	delete(right["photo_change_receipts"].Columns, "query_json")
	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["mailbox_jobs"].Columns["query_json"])
	delete(right["mailbox_jobs"].Columns, "query_json")
	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["processing_profiles"].Columns["embedding_bindings_json"])
	delete(right["processing_profiles"].Columns, "embedding_bindings_json")
	assert.Equal(t, backendColumn{Kind: "integer", Required: true}, right["saved_queries"].Columns["photo_scope"])
	delete(right["saved_queries"].Columns, "photo_scope")
	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["package_import_receipts"].Columns["query_json"])
	delete(right["package_import_receipts"].Columns, "query_json")
	assert.Equal(t, backendIndex{Columns: []string{"<expression>", "receipt_id"}, Partial: true}, left["photo_change_receipts"].Indexes["photo_change_receipts_face_version"])
	assert.Equal(t, backendIndex{Columns: []string{"face_version_id", "receipt_id"}, Partial: true}, right["photo_change_receipts"].Indexes["photo_change_receipts_face_version"])
	delete(left["photo_change_receipts"].Indexes, "photo_change_receipts_face_version")
	delete(right["photo_change_receipts"].Indexes, "photo_change_receipts_face_version")
	for _, column := range []string{"receipt_source_version_id", "receipt_operation_state"} {
		assert.Equal(t, backendColumn{Kind: "bytes", Required: true}, right["media_operations"].Columns[column])
		delete(right["media_operations"].Columns, column)
	}

	assert.Equal(t, backendColumn{Kind: "text", Required: true}, right["media_input_artifacts"].Columns["input_binding_json"])
	delete(right["media_input_artifacts"].Columns, "input_binding_json")

	// Accepted provenance strings permit NUL. PostgreSQL stores bytes while
	// SQLite retains its released TEXT; logical exports remain strings.
	for table, columns := range backendByteStringColumns {
		for _, column := range columns {
			l, r := left[table].Columns[column], right[table].Columns[column]
			assert.Equal(t, "text", l.Kind)
			assert.Equal(t, "bytes", r.Kind)
			l.Kind = "bytes"
			assert.Equal(t, l, r)
			delete(left[table].Columns, column)
			delete(right[table].Columns, column)
		}
	}
	// PostgreSQL's transactional node counter implements SQLite's sqlite_sequence.
	require.Contains(t, right, "node_allocation")
	delete(right, "node_allocation")
	// SQLite's released size lookup indexes zero-padded text; PostgreSQL can
	// compare the integer directly. Both serve BrowserPhotoSizeCandidates.
	const photoSizeIndex = "content_versions_photo_size"
	assert.Equal(t, backendIndex{Columns: []string{"<expression>"}}, left["content_versions"].Indexes[photoSizeIndex])
	assert.Equal(t, backendIndex{Columns: []string{"size"}}, right["content_versions"].Indexes[photoSizeIndex])
	delete(left["content_versions"].Indexes, photoSizeIndex)
	delete(right["content_versions"].Indexes, photoSizeIndex)
	// Hidden visibility follows receipt insertion order, independent of clocks.
	// PostgreSQL's ordinal and indexes replace SQLite's implicit rowid.
	receipts := right["photo_change_receipts"]
	require.Contains(t, receipts.Columns, "receipt_order")
	delete(receipts.Columns, "receipt_order")
	assert.Equal(t, backendIndex{Columns: []string{"receipt_order"}, Unique: true}, receipts.Indexes["unique:receipt_order"])
	delete(receipts.Indexes, "unique:receipt_order")
	assert.Equal(t, backendIndex{Columns: []string{"operation", "receipt_order"}}, receipts.Indexes["photo_change_receipts_operation"])
	delete(receipts.Indexes, "photo_change_receipts_operation")
	assert.Equal(t, backendIndex{Columns: []string{"operation"}}, left["photo_change_receipts"].Indexes["photo_change_receipts_operation"])
	delete(left["photo_change_receipts"].Indexes, "photo_change_receipts_operation")

	for table, reason := range sqliteOnlyBackendTables {
		assert.Contains(t, left, table, "stale table exception: %s", reason)
		assert.NotContains(t, right, table, "remove resolved table exception: %s", reason)
		delete(left, table)
	}
	for name, extra := range postgresOnlyBackendIndexes {
		require.Contains(t, right, extra.table)
		assert.NotContains(t, left[extra.table].Indexes, name, "remove resolved index exception")
		assert.Equal(t, backendIndex{Columns: []string{extra.column}}, right[extra.table].Indexes[name], "PostgreSQL cascade index %s", name)
		delete(right[extra.table].Indexes, name)
	}
	for table, expected := range left {
		actual, ok := right[table]
		if !assert.True(t, ok, "PostgreSQL missing table %s", table) {
			continue
		}
		assert.Equal(t, expected.Columns, actual.Columns, "columns of %s", table)
		assert.Equal(t, expected.ForeignKeys, actual.ForeignKeys, "foreign keys of %s", table)
		for name, index := range expected.Indexes {
			other, ok := actual.Indexes[name]
			if !ok {
				if _, deferred := deferredBackendIndexes[name]; deferred {
					continue
				}
				t.Errorf("PostgreSQL missing index %s on %s", name, table)
				continue
			}
			assert.Equal(t, index, other, "index %s on %s", name, table)
		}
		for name := range actual.Indexes {
			_, ok := expected.Indexes[name]
			assert.True(t, ok, "SQLite missing index %s on %s", name, table)
		}
	}
	for table := range right {
		_, ok := left[table]
		assert.True(t, ok, "SQLite missing table %s", table)
	}
	// An exception must disappear when its index is ported; it cannot mask a
	// later removal of an index that was already qualified.
	for name, table := range deferredBackendIndexes {
		require.Contains(t, left, table, "stale deferred index %s", name)
		require.Contains(t, right, table, "stale deferred index %s", name)
		assert.Contains(t, left[table].Indexes, name)
		assert.NotContains(t, right[table].Indexes, name, "remove resolved exception")
	}
}

func describeBackendSchema(t *testing.T, s *Store) map[string]backendTable {
	t.Helper()
	ctx := t.Context()
	query := `SELECT name FROM pragma_table_list WHERE schema='main' AND type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`
	if s.postgresSchema != "" {
		query = `SELECT relname FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind='r' ORDER BY relname`
	}
	var tables []string
	require.NoError(t, queryRows(ctx, s.db, query, nil, func(rows *sql.Rows) error {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		tables = append(tables, name)
		return nil
	}))
	result := make(map[string]backendTable, len(tables))
	for _, table := range tables {
		result[table] = backendTable{
			Columns:     describeBackendColumns(t, s, table),
			ForeignKeys: describeBackendForeignKeys(t, s, table),
			Indexes:     describeBackendIndexes(t, s, table),
		}
	}
	if s.postgresSchema != "" {
		var wrong []string
		require.NoError(t, queryRows(ctx, s.db, `SELECT r.relname||'.'||a.attname
   FROM pg_attribute a JOIN pg_class r ON r.oid=a.attrelid
   JOIN pg_collation c ON c.oid=a.attcollation
   WHERE r.relnamespace=current_schema()::regnamespace AND r.relkind='r'
    AND a.attnum>0 AND NOT a.attisdropped AND c.collname<>'C'`, nil, func(rows *sql.Rows) error {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			wrong = append(wrong, name)
			return nil
		}))
		assert.Empty(t, wrong, "PostgreSQL text columns must retain byte ordering")
	}
	return result
}

func describeBackendColumns(t *testing.T, s *Store, table string) map[string]backendColumn {
	t.Helper()
	query := `SELECT name,type,"notnull",pk FROM pragma_table_info($1)`
	if s.postgresSchema != "" {
		query = `SELECT a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,
   COALESCE(array_position(c.conkey,a.attnum),0)
   FROM pg_attribute a JOIN pg_class r ON r.oid=a.attrelid
   LEFT JOIN pg_constraint c ON c.conrelid=r.oid AND c.contype='p'
   WHERE r.relnamespace=current_schema()::regnamespace AND r.relname=$1
    AND a.attnum>0 AND NOT a.attisdropped`
	}
	columns := map[string]backendColumn{}
	require.NoError(t, queryRows(t.Context(), s.db, query, []any{table}, func(rows *sql.Rows) error {
		var name string
		var column backendColumn
		if err := rows.Scan(&name, &column.Kind, &column.Required, &column.PrimaryPosition); err != nil {
			return err
		}
		switch strings.ToLower(column.Kind) {
		case "integer", "bigint", "smallint":
			column.Kind = "integer"
		case "text", "uuid":
			column.Kind = "text"
		case "blob", "bytea":
			column.Kind = "bytes"
		case "real", "double precision":
			column.Kind = "real"
		default:
			return fmt.Errorf("unclassified column type %s.%s: %s", table, name, column.Kind)
		}
		// SQLite rowid-table TEXT primary keys report nullable; Go never writes a
		// null identity. WITHOUT ROWID and PostgreSQL enforce the logical rule.
		if column.PrimaryPosition != 0 {
			column.Required = true
		}
		columns[name] = column
		return nil
	}))
	return columns
}

func describeBackendForeignKeys(t *testing.T, s *Store, table string) []string {
	t.Helper()
	query := `SELECT CAST(id AS TEXT),"from","table","to",on_delete,on_update FROM pragma_foreign_key_list($1) ORDER BY id,seq`
	if s.postgresSchema != "" {
		query = `SELECT c.oid::text,a.attname,rr.relname,ra.attname,c.confdeltype::text,c.confupdtype::text
   FROM pg_constraint c JOIN pg_class r ON r.oid=c.conrelid JOIN pg_class rr ON rr.oid=c.confrelid
   JOIN LATERAL unnest(c.conkey,c.confkey) WITH ORDINALITY AS k(local_id,remote_id,position) ON true
   JOIN pg_attribute a ON a.attrelid=r.oid AND a.attnum=k.local_id
   JOIN pg_attribute ra ON ra.attrelid=rr.oid AND ra.attnum=k.remote_id
   WHERE r.relnamespace=current_schema()::regnamespace AND r.relname=$1 AND c.contype='f'
   ORDER BY c.oid,k.position`
	}
	groups := map[string][]string{}
	require.NoError(t, queryRows(t.Context(), s.db, query, []any{table}, func(rows *sql.Rows) error {
		var id, from, target, to, onDelete, onUpdate string
		if err := rows.Scan(&id, &from, &target, &to, &onDelete, &onUpdate); err != nil {
			return err
		}
		action := func(value string) string {
			switch value {
			case "a":
				return "NO ACTION"
			case "r":
				return "RESTRICT"
			case "c":
				return "CASCADE"
			case "n":
				return "SET NULL"
			case "d":
				return "SET DEFAULT"
			}
			return value
		}
		groups[id] = append(groups[id], from+"->"+target+"."+to+" delete="+action(onDelete)+" update="+action(onUpdate))
		return nil
	}))
	var keys []string
	for _, group := range groups {
		keys = append(keys, strings.Join(group, ";"))
	}
	slices.Sort(keys)
	return keys
}

func describeBackendIndexes(t *testing.T, s *Store, table string) map[string]backendIndex {
	t.Helper()
	if s.postgresSchema != "" {
		return describePostgresIndexes(t, s, table)
	}
	type indexHeader struct {
		name    string
		unique  bool
		origin  string
		partial bool
	}
	var headers []indexHeader
	require.NoError(t, queryRows(t.Context(), s.db,
		`SELECT name,"unique",origin,partial FROM pragma_index_list($1) WHERE origin<>'pk'`, []any{table}, func(rows *sql.Rows) error {
			var h indexHeader
			if err := rows.Scan(&h.name, &h.unique, &h.origin, &h.partial); err != nil {
				return err
			}
			headers = append(headers, h)
			return nil
		}))
	indexes := map[string]backendIndex{}
	for _, h := range headers {
		index := backendIndex{Unique: h.unique, Partial: h.partial}
		require.NoError(t, queryRows(t.Context(), s.db,
			`SELECT COALESCE(name,'<expression>'),"desc" FROM pragma_index_xinfo($1) WHERE "key"=1 ORDER BY seqno`, []any{h.name}, func(rows *sql.Rows) error {
				var name string
				var descending bool
				if err := rows.Scan(&name, &descending); err != nil {
					return err
				}
				if descending {
					name += " DESC"
				}
				index.Columns = append(index.Columns, name)
				return nil
			}))
		name := h.name
		if h.origin == "u" {
			name = "unique:" + strings.Join(index.Columns, ",")
		}
		indexes[name] = index
	}
	return indexes
}

func describePostgresIndexes(t *testing.T, s *Store, table string) map[string]backendIndex {
	t.Helper()
	indexes := map[string]backendIndex{}
	require.NoError(t, queryRows(t.Context(), s.db, `
  SELECT ir.relname, i.indisunique, i.indpred IS NOT NULL, c.contype IS NOT NULL,
   COALESCE(a.attname,'<expression>'), (i.indoption[k.position-1] & 1)=1
  FROM pg_index i JOIN pg_class r ON r.oid=i.indrelid JOIN pg_class ir ON ir.oid=i.indexrelid
  LEFT JOIN pg_constraint c ON c.conindid=i.indexrelid AND c.contype='u'
  JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum,position) ON k.position<=i.indnkeyatts
  LEFT JOIN pg_attribute a ON a.attrelid=r.oid AND a.attnum=k.attnum
  WHERE r.relnamespace=current_schema()::regnamespace AND r.relname=$1 AND NOT i.indisprimary
  ORDER BY ir.relname,k.position`, []any{table}, func(rows *sql.Rows) error {
		var name, column string
		var unique, partial, constraint, descending bool
		if err := rows.Scan(&name, &unique, &partial, &constraint, &column, &descending); err != nil {
			return err
		}
		// Prefix now, replace with ordered columns after all rows are collected.
		if constraint {
			name = "constraint:" + name
		}
		index := indexes[name]
		index.Unique = unique
		index.Partial = partial
		if descending {
			column += " DESC"
		}
		index.Columns = append(index.Columns, column)
		indexes[name] = index
		return nil
	}))
	for name, index := range indexes {
		if strings.HasPrefix(name, "constraint:") {
			delete(indexes, name)
			indexes["unique:"+strings.Join(index.Columns, ",")] = index
		}
	}
	return indexes
}

// Accepted UTF-8 evidence and names need byte storage on PostgreSQL. This list
// makes those intentional schema differences visible to the drift check.
var backendByteStringColumns = map[string][]string{
	"person_identities":          {"evidence_id"},
	"mailbox_containers":         {"id", "owner"},
	"mailbox_chunks":             {"container_id"},
	"mailbox_jobs":               {"id", "owner", "container_id"},
	"mailbox_occurrences":        {"job_id"},
	"mailbox_transfer_receipts":  {"archive_id", "source_ref"},
	"media_input_artifacts":      {"source_id", "source_version_id", "occurrence_id", "input_id", "origin", "provider", "language"},
	"media_source_versions":      {"source_id", "source_version_id"},
	"person_document_assertions": {"note"},
	"document_people":            {"raw_label"},
	"mailbox_archives":           {"description", "id", "owner"},
	"media_sources":              {"provider", "origin_scope", "source_id"},
	"photo_technical_metadata":   {"camera_make", "camera_model", "body_serial_number", "lens_make", "lens_model", "camera_make_folded", "camera_model_folded", "lens_make_folded", "lens_model_folded", "capture_time_raw", "location_label"},
	"ingests":                    {"source_kind", "source_desc"},
	"provenance":                 {"original_path"},
	"photo_source_claims":        {"original_path"},
	"persons":                    {"display_name", "display_name_folded"},
	"person_merges":              {"absorbed_display_name"},
	"person_external_identities": {"display_name_snapshot"},
	"person_match_candidates":    {"display_name"},
	"custodian_assignments":      {"raw_label", "raw_label_folded", "source_ref"},

	"extracted_text":                      {"extractor", "error", "text"},
	"media_occurrences":                   {"caller_principal", "caller_occurrence_ref", "caller_revision", "caller_filename", "caller_person_ref", "speaker_label", "source_id", "source_version_id", "occurrence_id"},
	"media_operations":                    {"principal", "source_id"},
	"processing_consent_grants":           {"principal", "scope"},
	"processing_consent_revocations":      {"principal", "scope"},
	"rendition_job_waiters":               {"principal", "scope"},
	"rendition_jobs":                      {"claim_owner"},
	"embedding_jobs":                      {"principal", "scope", "claim_owner"},
	"collection_snapshot_members":         {"occurrence_id", "family_id", "parent_occurrence_id", "display_name", "document_kind"},
	"collection_snapshot_representations": {"occurrence_id", "role", "media_type"},
	"packages":                            {"party_label", "relation", "produced_on"},
	"package_volumes":                     {"volume_name", "declared_root", "mapped_root"},
	"package_records":                     {"load_file", "occurrence_id"},
	"package_labels":                      {"label_set", "label", "occurrence_id", "artifact_id"},
	"package_preflights":                  {"owner", "source_ref", "source_locator"},
	"package_import_jobs":                 {"owner", "claim_owner"},
	"processing_profiles":                 {"trust_boundary"},
	"rendition_attachments":               {"trust_boundary"},
	"rendition_builds":                    {"provider_operation_id"},
	"rendition_artifacts":                 {"artifact_id"},
	"rendition_units":                     {"unit_id", "evidence_unit_id"},
	"rendition_lexical_segments":          {"text", "segment_id", "unit_id"},
	"rendition_lexical_index":             {"text", "segment_id"},
	"embedding_vector_spaces":             {"provider_descriptor", "provider_revision", "compatibility_id", "document_formatter", "query_formatter"},
	"embedding_generation_inputs":         {"input_id"},
	"embedding_vector_rows":               {"row_id", "input_id"},
	"visual_preview_generations":          {"failure_detail"},
	"bates_page_labels":                   {"occurrence_id"},
	"bates_artifact_pages":                {"occurrence_id"},
	"package_import_receipts":             {"occurrence_id"},
	"watch_sources":                       {"watch_name", "source_ref"},
	"watch_source_absences":               {"watch_name", "source_ref"},
	"current_rendition_roots":             {"root_id"},
	"document_events":                     {"source_key", "source_kind_raw", "raw_value", "zone_text", "evidence_id"},
	"document_event_actors":               {"display_name", "address", "evidence_id"},
	"document_event_primaries":            {"reason"},
	"content_versions":                    {"mime_type"},
}
