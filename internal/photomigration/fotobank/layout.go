package fotobank

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.kenn.io/docbank/internal/canonical"
)

type tableColumn struct {
	Name    string
	Type    string
	NotNull int64
	Default sql.NullString
	PK      int64
}

type tableLayout struct {
	Name    string
	Columns []tableColumn
}

// The catalog schema is intentionally a Go contract. Fotobank's migration
// file hash is a source build detail and isn't a database identity.
var catalogTables = map[string][]string{
	"owners":                    {"hub", "user_id", "storage_key", "display_handle", "created_at"},
	"schema_migrations":         {"version", "dirty"},
	"principal_display":         {"hub", "user_id", "handle", "cached_at"},
	"assets":                    {"id", "owner_hub", "owner_user_id", "state", "media_type", "imported_at", "timestamp", "make", "model", "lens_model", "focal_length", "shutter", "width", "height", "iso", "aperture", "duration_ms", "latitude", "longitude", "gps_at", "location_label", "source_metadata_version_id", "source_metadata_extractor_fingerprint", "source_metadata_checksum", "thumb_status", "thumb_claimed_at", "thumb_version", "thumb_updated_at", "hidden_at"},
	"media_files":               {"id", "asset_id", "owner_hub", "owner_user_id", "role", "mime_type", "original_filename", "import_source_path", "size", "docbank_node_id", "docbank_virtual_path", "current_version_id", "sha256"},
	"media_file_relationships":  {"source_file_id", "target_file_id", "kind"},
	"content_operations":        {"id", "asset_id", "file_id", "owner_hub", "owner_user_id", "status", "expected_sha256", "expected_size", "docbank_virtual_path", "docbank_node_id", "docbank_version_id", "last_error", "created_at", "updated_at"},
	"albums":                    {"id", "owner_hub", "owner_user_id", "name", "created_at", "updated_at"},
	"album_media":               {"album_id", "media_id", "added_at", "position"},
	"checkouts":                 {"id", "owner_hub", "owner_user_id", "root", "layout", "include_all", "state", "last_error", "created_at", "updated_at"},
	"checkout_asset_selections": {"checkout_id", "asset_id"},
	"checkout_album_selections": {"checkout_id", "album_id"},
	"checkout_year_selections":  {"checkout_id", "start_year", "end_year"},
	"checkout_entries":          {"checkout_id", "file_id", "relative_path", "base_version_id", "base_sha256", "base_size", "observed_size", "observed_mtime", "observed_identity", "observed_sha256", "state", "last_error", "created_at", "updated_at"},
	"checkout_scan_candidates":  {"checkout_id", "relative_path", "file_id", "observed_size", "observed_mtime", "observed_identity", "observed_sha256", "state", "first_observed_at", "last_observed_at"},
	"user_settings":             {"principal_hub", "principal_user_id", "key", "value", "updated_at"},
	"app_settings":              {"key", "value", "updated_at", "updated_by_hub", "updated_by_user_id"},
	"scopes":                    {"uuid", "owner_hub", "owner_user_id", "grantee_hub", "grantee_user_id", "target_type", "target_album_id", "allow_download", "label", "created_at", "expires_at", "revoked_at", "broker_status", "broker_registered_at", "broker_granted_at", "broker_revoked_at", "broker_last_error", "broker_attempts", "broker_next_attempt_at"},
	"scope_media":               {"scope_uuid", "media_id"},
	"auth_hidden_credential":    {"principal_hub", "principal_user_id", "passcode_hash", "created_at", "updated_at"},
	"auth_hidden_session":       {"token_sha256", "principal_hub", "principal_user_id", "issued_at", "expires_at", "revoked_at"},
	"auth_hidden_failure":       {"principal_hub", "principal_user_id", "occurred_at"},
	"auth_hidden_lockout":       {"principal_hub", "principal_user_id", "locked_until", "updated_at"},
	"ai_results":                {"id", "media_id", "task", "model_id", "prompt_version", "prompt_hash", "input_profile", "status", "generated_at"},
	"media_tags":                {"result_id", "tag_key", "tag_label", "rank"},
	"media_captions":            {"result_id", "text"},
	"ai_jobs":                   {"id", "media_id", "task", "fingerprint", "status", "attempts", "last_error", "last_error_kind", "claimed_at", "enqueued_at", "completed_at"},
	"ai_failures":               {"media_id", "task", "model_id", "prompt_version", "input_profile", "last_error", "last_error_kind", "attempt_count", "failed_at"},
	"ai_skipped":                {"media_id", "task", "reason", "recorded_at"},
	"embedding_generations":     {"id", "fingerprint", "fingerprint_hash", "model_id", "input_profile", "vec_table_name", "dimension", "state", "embedded_count", "threshold_pct", "created_at", "activated_at", "retired_at"},
	"media_embedding_ids":       {"generation_id", "media_id", "vec_id"},
}

var vecName = regexp.MustCompile(`^media_embeddings_g[0-9]+$`)

const catalogFingerprint = "8e60d54091ad032c8b45375e5a9abefb20b691d9fcd9b6a9caf6f22ad1a192c8"

var catalogTableMetadata = map[string]string{
	"ai_failures":               "attempt_count:INTEGER:1::0,failed_at:TIMESTAMP:1::0,input_profile:TEXT:1::5,last_error:TEXT:1::0,last_error_kind:TEXT:1::0,media_id:UUID:1::1,model_id:TEXT:1::3,prompt_version:TEXT:1::4,task:TEXT:1::2",
	"ai_jobs":                   "attempts:INTEGER:1:0:0,claimed_at:TIMESTAMP:0::0,completed_at:TIMESTAMP:0::0,enqueued_at:TIMESTAMP:1::0,fingerprint:TEXT:1::0,id:UUID:0::1,last_error:TEXT:0::0,last_error_kind:TEXT:0::0,media_id:UUID:1::0,status:TEXT:1::0,task:TEXT:1::0",
	"ai_results":                "generated_at:TIMESTAMP:1::0,id:UUID:0::1,input_profile:TEXT:1::0,media_id:UUID:1::0,model_id:TEXT:1::0,prompt_hash:TEXT:1::0,prompt_version:TEXT:1::0,status:TEXT:1::0,task:TEXT:1::0",
	"ai_skipped":                "media_id:UUID:1::1,reason:TEXT:1::0,recorded_at:TIMESTAMP:1::0,task:TEXT:1::2",
	"album_media":               "added_at:TIMESTAMP:1::0,album_id:UUID:1::1,media_id:UUID:1::2,position:INTEGER:0::0",
	"albums":                    "created_at:TIMESTAMP:1::0,id:UUID:0::1,name:TEXT:1::0,owner_hub:TEXT:1::0,owner_user_id:TEXT:1::0,updated_at:TIMESTAMP:1::0",
	"app_settings":              "key:TEXT:0::1,updated_at:TIMESTAMP:1::0,updated_by_hub:TEXT:0::0,updated_by_user_id:TEXT:0::0,value:TEXT:1::0",
	"assets":                    "aperture:REAL:0::0,duration_ms:INTEGER:0::0,focal_length:TEXT:0::0,gps_at:TIMESTAMP:0::0,height:INTEGER:0::0,hidden_at:TIMESTAMP:0::0,id:UUID:0::1,imported_at:TIMESTAMP:1::0,iso:INTEGER:0::0,latitude:REAL:0::0,lens_model:TEXT:0::0,location_label:TEXT:0::0,longitude:REAL:0::0,make:TEXT:0::0,media_type:TEXT:1::0,model:TEXT:0::0,owner_hub:TEXT:1::0,owner_user_id:TEXT:1::0,shutter:TEXT:0::0,source_metadata_checksum:TEXT:0::0,source_metadata_extractor_fingerprint:TEXT:0::0,source_metadata_version_id:TEXT:0::0,state:TEXT:1::0,thumb_claimed_at:TIMESTAMP:0::0,thumb_status:TEXT:1::0,thumb_updated_at:TIMESTAMP:0::0,thumb_version:INTEGER:1:0:0,timestamp:TIMESTAMP:0::0,width:INTEGER:0::0",
	"auth_hidden_credential":    "created_at:TIMESTAMP:1::0,passcode_hash:TEXT:1::0,principal_hub:TEXT:1::1,principal_user_id:TEXT:1::2,updated_at:TIMESTAMP:1::0",
	"auth_hidden_failure":       "occurred_at:TIMESTAMP:1::0,principal_hub:TEXT:1::0,principal_user_id:TEXT:1::0",
	"auth_hidden_lockout":       "locked_until:TIMESTAMP:1::0,principal_hub:TEXT:1::1,principal_user_id:TEXT:1::2,updated_at:TIMESTAMP:1::0",
	"auth_hidden_session":       "expires_at:TIMESTAMP:1::0,issued_at:TIMESTAMP:1::0,principal_hub:TEXT:1::0,principal_user_id:TEXT:1::0,revoked_at:TIMESTAMP:0::0,token_sha256:BLOB:1::1",
	"checkout_album_selections": "album_id:UUID:1::2,checkout_id:UUID:1::1",
	"checkout_asset_selections": "asset_id:UUID:1::2,checkout_id:UUID:1::1",
	"checkout_entries":          "base_sha256:TEXT:1::0,base_size:INTEGER:1::0,base_version_id:TEXT:1::0,checkout_id:UUID:1::1,created_at:TIMESTAMP:1::0,file_id:UUID:1::2,last_error:TEXT:0::0,observed_identity:TEXT:1::0,observed_mtime:TIMESTAMP:1::0,observed_sha256:TEXT:1::0,observed_size:INTEGER:1::0,relative_path:TEXT:1::0,state:TEXT:1::0,updated_at:TIMESTAMP:1::0",
	"checkout_scan_candidates":  "checkout_id:UUID:1::1,file_id:UUID:0::0,first_observed_at:TIMESTAMP:1::0,last_observed_at:TIMESTAMP:1::0,observed_identity:TEXT:1::0,observed_mtime:TIMESTAMP:1::0,observed_sha256:TEXT:0::0,observed_size:INTEGER:1::0,relative_path:TEXT:1::2,state:TEXT:1::0",
	"checkout_year_selections":  "checkout_id:UUID:1::1,end_year:INTEGER:1::3,start_year:INTEGER:1::2",
	"checkouts":                 "created_at:TIMESTAMP:1::0,id:UUID:0::1,include_all:INTEGER:1:0:0,last_error:TEXT:0::0,layout:TEXT:1::0,owner_hub:TEXT:1::0,owner_user_id:TEXT:1::0,root:TEXT:1::0,state:TEXT:1::0,updated_at:TIMESTAMP:1::0",
	"content_operations":        "asset_id:UUID:1::0,created_at:TIMESTAMP:1::0,docbank_node_id:INTEGER:0::0,docbank_version_id:TEXT:0::0,docbank_virtual_path:TEXT:1::0,expected_sha256:TEXT:1::0,expected_size:INTEGER:1::0,file_id:UUID:1::0,id:UUID:0::1,last_error:TEXT:0::0,owner_hub:TEXT:1::0,owner_user_id:TEXT:1::0,status:TEXT:1::0,updated_at:TIMESTAMP:1::0",
	"embedding_generations":     "activated_at:TIMESTAMP:0::0,created_at:TIMESTAMP:1::0,dimension:INTEGER:1::0,embedded_count:INTEGER:1:0:0,fingerprint:TEXT:1::0,fingerprint_hash:TEXT:1::0,id:INTEGER:0::1,input_profile:TEXT:1::0,model_id:TEXT:1::0,retired_at:TIMESTAMP:0::0,state:TEXT:1::0,threshold_pct:INTEGER:1:95:0,vec_table_name:TEXT:1::0",
	"media_captions":            "result_id:UUID:0::1,text:TEXT:1::0",
	"media_embedding_ids":       "generation_id:INTEGER:1::1,media_id:UUID:1::2,vec_id:INTEGER:1::0",
	"media_file_relationships":  "kind:TEXT:1::3,source_file_id:UUID:1::1,target_file_id:UUID:1::2",
	"media_files":               "asset_id:UUID:1::0,current_version_id:TEXT:0::0,docbank_node_id:INTEGER:0::0,docbank_virtual_path:TEXT:0::0,id:UUID:0::1,import_source_path:TEXT:1:'':0,mime_type:TEXT:1::0,original_filename:TEXT:1::0,owner_hub:TEXT:1::0,owner_user_id:TEXT:1::0,role:TEXT:1::0,sha256:TEXT:0::0,size:INTEGER:1::0",
	"media_tags":                "rank:INTEGER:1::0,result_id:UUID:1::1,tag_key:TEXT:1::2,tag_label:TEXT:1::0",
	"owners":                    "created_at:TIMESTAMP:1::0,display_handle:TEXT:0::0,hub:TEXT:1::1,storage_key:TEXT:1::0,user_id:TEXT:1::2",
	"principal_display":         "cached_at:TIMESTAMP:1::0,handle:TEXT:0::0,hub:TEXT:1::1,user_id:TEXT:1::2",
	"schema_migrations":         "dirty:bool:0::0,version:uint64:0::0",
	"scope_media":               "media_id:UUID:1::2,scope_uuid:UUID:1::1",
	"scopes":                    "allow_download:BOOLEAN:1:0:0,broker_attempts:INTEGER:1:0:0,broker_granted_at:TIMESTAMP:0::0,broker_last_error:TEXT:0::0,broker_next_attempt_at:TIMESTAMP:0::0,broker_registered_at:TIMESTAMP:0::0,broker_revoked_at:TIMESTAMP:0::0,broker_status:TEXT:1::0,created_at:TIMESTAMP:1::0,expires_at:TIMESTAMP:0::0,grantee_hub:TEXT:1::0,grantee_user_id:TEXT:1::0,label:TEXT:0::0,owner_hub:TEXT:1::0,owner_user_id:TEXT:1::0,revoked_at:TIMESTAMP:0::0,target_album_id:UUID:0::0,target_type:TEXT:1::0,uuid:UUID:0::1",
	"user_settings":             "key:TEXT:1::3,principal_hub:TEXT:1::1,principal_user_id:TEXT:1::2,updated_at:TIMESTAMP:1::0,value:TEXT:1::0",
}

func catalogLayout(ctx context.Context, db *sql.DB) ([]tableLayout, error) {
	vectorTables, err := catalogVectorTables(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT name,sql FROM sqlite_master WHERE type IN ('table','view','virtual table') AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("reading Fotobank table layout: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type tableDefinition struct{ Name, SQL string }
	var definitions []tableDefinition
	for rows.Next() {
		var name, sqlText string
		if err := rows.Scan(&name, &sqlText); err != nil {
			return nil, err
		}
		definitions = append(definitions, tableDefinition{Name: name, SQL: sqlText})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var layouts []tableLayout
	for _, definition := range definitions {
		if isIgnoredTable(definition.Name) || ignoredVectorTable(definition.Name, definition.SQL, vectorTables) {
			continue
		}
		if strings.Contains(strings.ToLower(definition.SQL), "using vec0") {
			layouts = append(layouts, tableLayout{Name: definition.Name})
			continue
		}
		columns, err := tableColumns(ctx, db, definition.Name)
		if err != nil {
			return nil, err
		}
		layouts = append(layouts, tableLayout{Name: definition.Name, Columns: columns})
	}
	return layouts, nil
}

func catalogVectorTables(ctx context.Context, db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT vec_table_name FROM embedding_generations WHERE vec_table_name IS NOT NULL ORDER BY vec_table_name`)
	if err != nil {
		return nil, fmt.Errorf("reading Fotobank vector table metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if vecName.MatchString(name) {
			result[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func ignoredVectorTable(name, sqlText string, vectorTables map[string]struct{}) bool {
	if vecName.MatchString(name) {
		_, known := vectorTables[name]
		return known && strings.Contains(strings.ToLower(sqlText), "using vec0")
	}
	for _, suffix := range []string{"_chunks", "_rowids", "_vector_chunks00", "_info"} {
		if before, ok := strings.CutSuffix(name, suffix); ok {
			base := before
			_, known := vectorTables[base]
			return known
		}
	}
	return false
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]tableColumn, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdent(table)+`)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []tableColumn
	for rows.Next() {
		var cid int64
		var name, typ string
		var notNull, pk int64
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		defaultText := ""
		if defaultValue != nil {
			switch value := defaultValue.(type) {
			case []byte:
				defaultText = string(value)
			default:
				defaultText = fmt.Sprint(value)
			}
		}
		columns = append(columns, tableColumn{Name: name, Type: typ, NotNull: notNull, Default: sql.NullString{String: defaultText, Valid: defaultValue != nil}, PK: pk})
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].Name < columns[j].Name })
	return columns, rows.Err()
}

func columnNames(columns []tableColumn) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.Name)
	}
	return names
}

func encodeColumns(columns []tableColumn) string {
	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		defaultText := ""
		if column.Default.Valid {
			defaultText = column.Default.String
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%d:%s:%d", column.Name, column.Type, column.NotNull, defaultText, column.PK))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func layoutFingerprint(layouts []tableLayout) string {
	rows := make([]string, 0, len(layouts))
	for _, layout := range layouts {
		rows = append(rows, layout.Name+"|"+encodeColumns(layout.Columns))
	}
	sort.Strings(rows)
	hash, _ := canonical.Marshal(rows)
	digest := sha256.Sum256(hash)
	return hex.EncodeToString(digest[:])
}

func isIgnoredTable(name string) bool {
	return name == "media_fts" || strings.HasPrefix(name, "media_fts_")
}

func sameStrings(left, right []string) bool {
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func quoteIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
