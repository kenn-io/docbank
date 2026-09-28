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
	Type      string
	Name      string
	TableName string
	SQL       string
	Columns   []tableColumn
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

const catalogFingerprint = "dd0110407d621764e8c82b0b7a6da85e4aa641e358f251b9c3b47191397b3fe1"

//nolint:gosec // these are schema object digests, not credentials.
var catalogObjectDigests = map[string]string{
	"index:ai_failures_active_idx":                         "b41d2585cd54935b2fe695dd670f3eab9bad13b071c2b3c2151d085618ec7cb9",
	"index:ai_jobs_active_idx":                             "a804bb77bf057b5e62d41279ee3ec3097b423cff38ebe1319da60aa4e9ddc889",
	"index:ai_jobs_pending_idx":                            "dd83e19ea419af51858c0e52ea257785580ba78a4baf0222efc9fca3f808e95f",
	"index:ai_jobs_terminal_idx":                           "fd707ba636fb968c16330137898834743d339c44d1ab19b1835e411c0f19d4dd",
	"index:ai_results_active_one_per":                      "144ba9ffef0c0260a72d3a27c24c6c3627d4bd3b77993b1ee804bd10b85439b8",
	"index:ai_results_media_task_idx":                      "60a4d3fbf4490cc41f77e944768e86752e156f485dba6523086fdf51009f1ce2",
	"index:album_media_album_added_idx":                    "bcff42dbc85665b20d70d45a75a71ec6436a86225ce406da6a55b4bd9ba7c14a",
	"index:albums_owner_idx":                               "b1647b2a80a06931e74707638839cb9e3ede8b584a592a5d078b672dc915ba1c",
	"index:albums_owner_updated_idx":                       "696cf0d605a444e0b1a8114f48343496dfb7a2c96b30eba349443640d32b8e80",
	"index:assets_owner_camera_visible_idx":                "70decda26ace4edd58ff2f7c447057cce4c819d074d80d10c7011f6bf736e6c1",
	"index:assets_owner_geo_idx":                           "38c81c7221b5605a77700d9c755fae7101769de242051be8282147ecc5c6f99d",
	"index:assets_owner_imported_idx":                      "e38fa5b8212dd90ebc5c396a1197be0127ba0c6da44e3eef262a61cf4abebb59",
	"index:assets_owner_lens_visible_idx":                  "88e3272dc71406c2571961030e5891c33408b0e66fb554de6ec4714b5942a571",
	"index:assets_owner_timestamp_idx":                     "279b25ba8e77e7ce67630bae8bb62919799d980290a54a11600aeec9ae0d22f5",
	"index:assets_owner_type_idx":                          "901a90b3a563cef2caf54dd3d2f5fecab4afe6e598543fd105479148052797df",
	"index:assets_thumb_pending_idx":                       "8a14c412184a9b41071966aee7cf1d63404d12b1187f2b719fc14ef38f38e459",
	"index:assets_visible_idx":                             "6cff5061d4062a0f6d1b7d787d76485f15d73ad8e55103955edfe99cfc5b6446",
	"index:auth_hidden_failure_owner_idx":                  "ec6a37b8b483e4afd5f18a92a41a515e6baf18b5e413f4c526644227e261ff1e",
	"index:auth_hidden_session_expiry_idx":                 "c134e03e410db7e2f6a3743c1818366fd45f2f6472aa22dfc66910e457305988",
	"index:auth_hidden_session_principal_active_idx":       "30d36b3f0b019bce2f2819966483cd329377937f9053bfe3290a71cc03117100",
	"index:checkout_entries_binding_uq":                    "192ee4f0bdc46128accccfc869fef8648b8ef3061026a4dc06c2abe7b656d31c",
	"index:checkout_entries_state_idx":                     "6f7eaad804d06f49a64fb1cd27a53b56821bc932d116f193accbd9cb43e837b2",
	"index:checkout_scan_candidates_state_idx":             "7f82b42151cd78ed97b5da724df8acb237490142f8d78f7186db9b86c9311fc7",
	"index:checkouts_live_root_uq":                         "7fb220dc940dab9c329197e292e2b15503454b916cf82bbd1021c3deae9f0266",
	"index:checkouts_owner_idx":                            "675f5fa0a1daed02c65ade2e2e25413aa789a0a7e4896e80efe83b292673be5a",
	"index:content_operations_owner_sha256_uq":             "244d81e77b35e2615a1900f7f2c6266cfd4c59c8250422a7d7b1cd314fc9c5d6",
	"index:content_operations_pending_idx":                 "aa855ef2f2dcc1b369e7ca6b04d4e1f6ad87f75efbf4bc253e441dcc50076928",
	"index:embedding_generations_one_active":               "d40c9d92efea00654c521972b8c0357620c38f6f04ceba0824eb8e31c390d555",
	"index:media_embedding_ids_media_idx":                  "8c79f7dd765393d569f25969f5d82fb555db60f1264281bba369de504460f529",
	"index:media_file_relationships_target_idx":            "40cd778983137fccc8cb53ace5e9e7faf7cb7da944d4774dd3d4af2ae9d4f120",
	"index:media_files_asset_idx":                          "f63c6bd373cf0e4527c68c9933b4970d4f742100dac906131eae408cd9197fee",
	"index:media_files_current_version_uq":                 "3c8eb588ade38e00c2d5d566738b40323d44f1e4bc4cebcd4f02a2ecff63bf78",
	"index:media_files_docbank_node_uq":                    "8c342e3ed88400bf24201d41982346a990e29e62586534562c63ed4bee21f2e7",
	"index:media_files_docbank_path_uq":                    "20f1084f53cdb9250ca68afbf084a96e6040e7bdd61b50dfda9b3f842970c733",
	"index:media_files_one_primary_uq":                     "95dde92ebd979593c2442eb5ba35a5ec93c3a05c38e947beda697292133d13d4",
	"index:media_files_owner_sha256_uq":                    "3c1e5817755c5943f6c1822961c347e7d1cd0e722ca1f3c6070dfa398d847c9c",
	"index:media_tags_key_idx":                             "5c1f592c72c7fc906591a90b429093d61309ddfe3d1d6658f72287bd920571a6",
	"index:owners_storage_key_uq":                          "83d10a8b3ed35f5da49de971b910b7c90fa17402394e4736c597fd3dc32d123c",
	"index:scopes_broker_pending_idx":                      "7e4e8193758534614adcc76835288e52f9700aa4f1c09680453c42b5486f6fb2",
	"index:scopes_broker_ready_idx":                        "88af286bba0294cd87806a054c3002676cfd237ad688273f69585a66bbba19b4",
	"index:scopes_grantee_idx":                             "0f08c5e87102a36007abce9b279da4c2565417120019a78daf3e1cd81fad92dd",
	"index:scopes_owner_idx":                               "4ffd65a73dbd634c309b9431f32bb9969413f658607173a1fe67c4244b7b93ae",
	"index:version_unique":                                 "f4ee777bef37507060a2582867cf84fccdcd6eb346062f797edc423541167f07",
	"table:ai_failures":                                    "1234d2a4b66cf245d1a022670919110e30bd6fe9f666b7958b30f7b70c11fb24",
	"table:ai_jobs":                                        "ee3f5fb2872819343e6007feea250c43573e1b701be8cab9462c71653f222e8c",
	"table:ai_results":                                     "1273ee775d4857f4a99c1cb8087fbeca03ddc2bbdc19dc864d2383660d301e81",
	"table:ai_skipped":                                     "ed7ee6eea7a8aa0f61e789affbd2f7f53752f0a764a07a5d53e86c16487715a7",
	"table:album_media":                                    "df45f9d74845a5f468040573bb21f290b41a522b611a0dc4daeef9f2222479de",
	"table:albums":                                         "53ae739002fde70b84c664269edcffcf1e9cfa5193289d909a507221f65a5e00",
	"table:app_settings":                                   "6de3d48478429aface62d36e6a83f504c1b354159712677203f44fa227be56b7",
	"table:assets":                                         "a27c6811a2d2dcc6caf084b05b46d726fa09845c11c8429ce3b963acaac1a5bc",
	"table:auth_hidden_credential":                         "7707de5473e106f76ee467bd50ded07024e8983eb003cbc0446bb1b888c23705",
	"table:auth_hidden_failure":                            "8e259fec6b97910bbab334e7931fcccbcbae86b9c1ced4931a1242e1b67f6345",
	"table:auth_hidden_lockout":                            "e1a37250c3414b8afae10b9e1c08814844207ed9abc3e2b3b8c6d740276475c8",
	"table:auth_hidden_session":                            "91fa49b9536177f134e63e38200f293dc02b63adfd690a50b868e51f72ffc043",
	"table:checkout_album_selections":                      "5873ff07f7a5e7ccd2ecb57162c71623c6be137fd650ce522b2e4c8ba1c27964",
	"table:checkout_asset_selections":                      "849f1490b8fabde97e8e7d3e934f65636a59c9af98f6e98b7a91dcd641ab30ac",
	"table:checkout_entries":                               "01d0e50100350ff27da11ce597aefc5b3dc6a421b756911abba3a426b78d3eae",
	"table:checkout_scan_candidates":                       "f8dcb25ae72d384be678033b77499f80546ceb7b49ef5f45e8cc0c65c351184d",
	"table:checkout_year_selections":                       "b389306f7a47b0a04f56fd5f7d29c36786726262b2547ff290f9064d97dbe5a2",
	"table:checkouts":                                      "38c8abb20ac07ed8d3a4392035363554280ebde97d9d3f979465535fd2470002",
	"table:content_operations":                             "2c33be5e3f16dad9622a7c1f64d63fc80b9bd22a0f3934926a6b837a89ab6c30",
	"table:embedding_generations":                          "9a4cc696252b157b407b0fa90d021c140a45ef77ca5ffaeb5a04902408918d64",
	"table:media_captions":                                 "7fcf16b93e2ecbf467d7ac14607882f651d84dd0193749b86c5e062e85869305",
	"table:media_embedding_ids":                            "cd6816818671f219368e50f8516df6f8f280692eec1060281bdb05a05b2a8933",
	"table:media_file_relationships":                       "17f8b240d3f82dcbe8608b4e8b42990a0174c3506ad5b57d273c66f4a1e605ab",
	"table:media_files":                                    "9164383803cc22f13e33c231e527228a76247bfd1491628983e44b815b3ae5c9",
	"table:media_fts":                                      "42ad685e1c957fc5d77c4b3508f5b6551efdde73e08e429cbb5af37b3003e32b",
	"table:media_tags":                                     "18f6ec7bebba154be361f5ddabe8fc3825fe4354b2e67d2fb70e3bbe0617ecbd",
	"table:owners":                                         "26939bd49aafc769b64e15846682cbfe47e5bfc0602e7ec7f96516d802c6baba",
	"table:principal_display":                              "4df2fb8b222f79c2b11753c16bee647ac566d839c2ec184bf165510cff046ca5",
	"table:schema_migrations":                              "2e9f63de4032a3afe2d7a87c42580701cc36ecd3b940b594f0118fdfd719af6b",
	"table:scope_media":                                    "0e1faf96ad7a3c57efcbcb4dc0a74675ab840818887dc658194da5be471a5a13",
	"table:scopes":                                         "15d5de1f653d059e1235c163142f1b889a8db04debf06829f26dff78dc7a726e",
	"table:user_settings":                                  "30e21f239e0b3d6b64bea8988b4734cb455a24548eabe901ddbb62cefdac607e",
	"trigger:album_media_owner_consistency_insert":         "041a236ff4d5502bb9a079f857741ed2f981cc52730506dc45bef8535d254a5c",
	"trigger:album_media_owner_consistency_update":         "2b569c414f781672f297ac46282f861a3ab43b97e7fe1b65b63cb6a673ad2310",
	"trigger:assets_ready_insert":                          "081a913ab2052ea9d3970bed1ea1aaa69ab69ac65810f90424095295338cd677",
	"trigger:assets_ready_mapping_update":                  "83082f1e4f61b3319a78a6ca6593551dbd31e608f68cb0478684b806e55fe462",
	"trigger:assets_ready_primary_update":                  "be83e67877c82b3bf30b22203d24914e455fc223fbe518647cd1f928796ce417",
	"trigger:media_file_relationships_consistency_insert":  "f646f0f4e17a1696207c1ae8b895228fe3a87e11dd4ab8ca6d7274a1a3b5d885",
	"trigger:media_file_relationships_consistency_update":  "2f8e8947079063657d8d0ae82b5da7e58cd5a013b77328b54b420ec0a7f69afe",
	"trigger:media_files_coordinate_update":                "c578bf708ff36fc429596c2d35c1a10ddeb0c9ac0a57505741fbd7d14cb4913a",
	"trigger:media_files_ready_mapping_insert":             "2e59d779e2e9117ed34e805d9a7b03f9e1f141af7835d419a8d3298bb82881d9",
	"trigger:media_files_ready_mapping_update":             "711efffa4373be2be422d5c6062d468bc784e4a9dceda98de832e49c035bee10",
	"trigger:media_files_ready_primary_delete":             "a869c70e8a09e380cefc330ef97c385b9ade047844658eeb936c225f5e2d55d5",
	"trigger:media_files_ready_primary_update":             "80008b557c4a57d76880eefd460f344a33346cca11d013bb0b7a53cb8b594907",
	"trigger:media_fts_cleanup_after_delete":               "0a502c6df500dd6e8a9446bf7ffc09fb00b30f1180047b073491cc6eacbdb20d",
	"trigger:scope_media_owner_consistency_insert":         "d5bbe7221e06d2b12b78bd6a32e4b9097641211876589f63693a867d6583d598",
	"trigger:scope_media_owner_consistency_update":         "9747e9ce663de158b1a43c170d3b8405809b5c083297a6775605b260b13cd9c6",
	"trigger:scopes_target_album_owner_consistency_insert": "aa481b301eed915b24565e29f12a458bed5f1764d156f5d5cadcc7463dfc4106",
	"trigger:scopes_target_album_owner_consistency_update": "8318e6e6100b16583d1f30b9d6fc2df74d2e7443dde94ab7d86af5088a3af8c1",
}

func catalogLayout(ctx context.Context, db *sql.DB) ([]tableLayout, error) {
	vectorTables, err := catalogVectorTables(ctx, db)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE substr(name,1,7) <> 'sqlite_' AND sql IS NOT NULL ORDER BY type,name`)
	if err != nil {
		return nil, fmt.Errorf("reading Fotobank schema objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var definitions []tableLayout
	for rows.Next() {
		var definition tableLayout
		if err := rows.Scan(&definition.Type, &definition.Name, &definition.TableName, &definition.SQL); err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	seenVectors := make(map[string]bool, len(vectorTables))
	var layouts []tableLayout
	for _, definition := range definitions {
		if vector, registered := vectorTables[definition.Name]; registered {
			if definition.Type != "table" {
				return nil, fmt.Errorf("registered vector table %q has sqlite_schema type %q, expected table", definition.Name, definition.Type)
			}
			if !vectorDefinitionMatches(definition.Name, vector.Dimension, definition.SQL) {
				return nil, fmt.Errorf("registered vector table %q has invalid vec0 definition", definition.Name)
			}
			seenVectors[definition.Name] = true
			continue
		}
		if isIgnoredTable(definition.Type, definition.Name) {
			continue
		}
		if base, ok := vectorShadowBase(definition.Name); ok {
			if definition.Type == "table" {
				if _, registered := vectorTables[base]; registered {
					continue
				}
			}
		}
		if isVecDefinitionSQL(definition.SQL) {
			return nil, fmt.Errorf("unregistered vector table %q", definition.Name)
		}
		if definition.Type == "table" {
			if _, known := catalogTables[definition.Name]; known {
				definition.Columns, err = tableColumns(ctx, db, definition.Name)
				if err != nil {
					return nil, err
				}
			}
		}
		layouts = append(layouts, definition)
	}
	var missingVectors []string
	for name := range vectorTables {
		if !seenVectors[name] {
			missingVectors = append(missingVectors, name)
		}
	}
	if len(missingVectors) > 0 {
		sort.Strings(missingVectors)
		return nil, fmt.Errorf("registered vector table %s is missing", strings.Join(missingVectors, ","))
	}
	return layouts, nil
}

type vectorTable struct {
	Dimension int64
}

func catalogVectorTables(ctx context.Context, db *sql.DB) (map[string]vectorTable, error) {
	rows, err := db.QueryContext(ctx, `SELECT vec_table_name,dimension FROM embedding_generations WHERE vec_table_name IS NOT NULL ORDER BY vec_table_name`)
	if err != nil {
		return nil, fmt.Errorf("reading Fotobank vector table metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]vectorTable)
	for rows.Next() {
		var name string
		var dimension int64
		if err := rows.Scan(&name, &dimension); err != nil {
			return nil, err
		}
		if !vecName.MatchString(name) {
			return nil, fmt.Errorf("invalid registered vector table name %q", name)
		}
		if dimension <= 0 {
			return nil, fmt.Errorf("invalid dimension %d for registered vector table %q", dimension, name)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate registered vector table %q", name)
		}
		result[name] = vectorTable{Dimension: dimension}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func vectorDefinitionMatches(name string, dimension int64, sqlText string) bool {
	expected := fmt.Sprintf("CREATE VIRTUAL TABLE %s USING vec0(vec_id INTEGER PRIMARY KEY, embedding FLOAT[%d])", name, dimension)
	return strings.EqualFold(normalizeSchemaSQL(sqlText), normalizeSchemaSQL(expected))
}

func isVecDefinitionSQL(sqlText string) bool {
	normalized := strings.ToLower(normalizeSchemaSQL(sqlText))
	return strings.HasPrefix(normalized, "create virtual table ") && strings.Contains(normalized, " using vec0(")
}

func normalizeSchemaSQL(sqlText string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(sqlText)), " ")
}

func vectorShadowBase(name string) (string, bool) {
	for _, suffix := range []string{"_chunks", "_rowids", "_vector_chunks00", "_info"} {
		if before, ok := strings.CutSuffix(name, suffix); ok {
			return before, true
		}
	}
	return "", false
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
	rows := make([][]string, 0, len(layouts))
	for _, layout := range layouts {
		rows = append(rows, []string{layout.Type, layout.Name, layout.TableName, layout.SQL})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i][0] != rows[j][0] {
			return rows[i][0] < rows[j][0]
		}
		return rows[i][1] < rows[j][1]
	})
	encoded, _ := canonical.Marshal(rows)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func objectKey(layout tableLayout) string {
	return layout.Type + ":" + layout.Name
}

func objectDigest(layout tableLayout) string {
	encoded, _ := canonical.Marshal([]string{layout.Type, layout.Name, layout.TableName, layout.SQL})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

var ftsShadowTables = map[string]struct{}{
	"media_fts_config":  {},
	"media_fts_content": {},
	"media_fts_data":    {},
	"media_fts_docsize": {},
	"media_fts_idx":     {},
}

func isIgnoredTable(typ, name string) bool {
	if typ != "table" {
		return false
	}
	_, ok := ftsShadowTables[name]
	return ok
}
func sameStrings(left, right []string) bool {
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func quoteIdent(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
