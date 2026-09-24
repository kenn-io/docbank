package mcp

import (
	"maps"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

const (
	jsonSchemaDraft       = "https://json-schema.org/draft/2020-12/schema"
	packageIDField        = "package_id"
	stateField            = "state"
	maxToolResponseBytes  = 1 << 20
	maxToolErrorBytes     = 1024
	maxPathBytes          = 16 << 10
	maxCursorBytes        = api.MaxDocumentCursorBytes
	maxPathCharacters     = 16 << 10
	maxCursorCharacters   = maxCursorBytes
	maxRenditionChars     = 16_000
	defaultRenditionChars = 8_000
	maxPackageDiagnostics = 250
)

type schema = map[string]any

func objectSchema(properties schema, required ...string) schema {
	result := schema{
		"type":                 "object", //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) != 0 {
		result["required"] = required
	}
	return result
}

func rootObjectSchema(properties schema, required ...string) schema {
	result := objectSchema(properties, required...)
	result["$schema"] = jsonSchemaDraft
	return result
}

func stringSchema(maxLength int) schema {
	result := schema{"type": "string"} //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
	if maxLength > 0 {
		result["maxLength"] = maxLength
	}
	return result
}

func enumSchema(values ...string) schema { return schema{"type": "string", "enum": values} }

func booleanSchema() schema { return schema{"type": "boolean"} }

func integerSchema(minimum, maximum int64) schema {
	result := schema{"type": "integer", "minimum": minimum}
	if maximum > 0 {
		result["maximum"] = maximum
	}
	return result
}

func arraySchema(items schema, maximum int) schema {
	result := schema{"type": "array", "items": items} //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
	if maximum > 0 {
		result["maxItems"] = maximum
	}
	return result
}

func uuidSchema() schema {
	return schema{
		"type": "string", "format": "uuid", "minLength": 36, "maxLength": 36, //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
		"pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$", //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
	}
}

func sha256Schema() schema {
	return schema{"type": "string", "pattern": "^[0-9a-f]{64}$", "minLength": 64, "maxLength": 64}
}

func dateTimeSchema() schema {
	return schema{
		"type": "string", "format": "date-time", "maxLength": 64,
		"pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.,][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$",
	}
}

func cursorSchema() schema {
	return schema{
		"type": "string", "maxLength": maxCursorCharacters,
		"pattern": "^[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+$",
	}
}

func packageImportOutputSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"operation_id": uuidSchema(), mcpJobIDField: uuidSchema(), packageIDField: uuidSchema(),
		"preflight_id": uuidSchema(), stateField: enumSchema("queued", "running", "complete", "partial", "failed", "cancelled"),
		"committed": integerSchema(0, 100_000), "total": integerSchema(1, 100_000),
		"gap_count": integerSchema(0, 100_000), "gaps": arraySchema(stringSchema(4096), 100),
		"created_at": dateTimeSchema(), "updated_at": dateTimeSchema(),
	}), cacheRequired("operation_id", mcpJobIDField, packageIDField, "preflight_id", stateField, "committed", "total", "gap_count", "created_at", "updated_at")...)
}

func getPackageImportSchemas() (schema, schema) {
	return rootObjectSchema(schema{"operation_id": uuidSchema()}, "operation_id"), packageImportOutputSchema()
}

func startPackageImportSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"preflight_id": uuidSchema(),
		"into":         schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters, "pattern": "^/"},
		"name":         schema{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9._-]+$"},
		"party":        stringSchema(64), "operation_id": uuidSchema(),
		"accept_partial": booleanSchema(), "index_supplied_text": booleanSchema(),
	}, "preflight_id", "into", "name", "operation_id", "accept_partial", "index_supplied_text"), packageImportOutputSchema()
}

func packageDiagnosticSchema() schema {
	return objectSchema(schema{
		"code": stringSchema(128), "severity": enumSchema("warning", "error", "blocking"),
		"load_file": stringSchema(4096), "row_id": stringSchema(loadfile.MaxFieldValueBytes), "row_ordinal": integerSchema(1, 0),
		"column": stringSchema(4096), "detail": stringSchema(1 << 16),
	}, "code", "severity")
}

func packagePreflightOutputSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"preflight_id": uuidSchema(), "source_kind": enumSchema("root", "container"), "source_ref": stringSchema(4096),
		"profile_sha256": sha256Schema(), "mapping_sha256": sha256Schema(), "manifest_sha256": sha256Schema(),
		"volumes": arraySchema(schema{"type": "object"}, 64), "records": integerSchema(0, 100_000),
		"pages": integerSchema(0, 1_000_000), "diagnostic_count": integerSchema(0, 0),
		"diagnostics": arraySchema(packageDiagnosticSchema(), maxPackageDiagnostics), "blocking": booleanSchema(),
		"created_at": dateTimeSchema(), "expires_at": dateTimeSchema(),
	}), cacheRequired("preflight_id", "source_kind", "source_ref", "profile_sha256", "mapping_sha256",
		"manifest_sha256", "volumes", "records", "pages", "diagnostic_count", "diagnostics", "blocking", "created_at", "expires_at")...)
}

func preflightLoadFilePackageSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"source_path":      schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters},
		"profile":          schema{"type": "string", "minLength": 1, "maxLength": 128},
		"page_map_profile": stringSchema(128), "encoding": schema{"type": "string", "minLength": 1, "maxLength": 64},
		"mapping_json": stringSchema(256 << 10),
	}, "source_path", "profile", "encoding"), packagePreflightOutputSchema()
}

func getPackagePreflightSchemas() (schema, schema) {
	return rootObjectSchema(schema{"preflight_id": uuidSchema()}, "preflight_id"), packagePreflightOutputSchema()
}

func listPackagePreflightDiagnosticsSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"preflight_id": uuidSchema(), "cursor": stringSchema(maxCursorCharacters), "limit": integerSchema(1, maxPackageDiagnostics),
		}, "preflight_id"), rootObjectSchema(withPrivateCache(schema{
			"diagnostics": arraySchema(packageDiagnosticSchema(), maxPackageDiagnostics), "total": integerSchema(0, 0),
			"next_cursor": stringSchema(maxCursorCharacters),
		}), cacheRequired("diagnostics", "total")...)
}

func custodianAssignmentSchema() schema {
	return objectSchema(custodianAssignmentProperties(), "assignment_id", "scope_kind", "raw_label", "rank", "basis", "source_ref", "revision", "recorded_at")
}

func custodianAssignmentProperties() schema {
	return schema{
		"assignment_id": uuidSchema(), "scope_kind": enumSchema("package", "collection", "document"),
		packageIDField: uuidSchema(), "package_record_id": sha256Schema(), "person_id": uuidSchema(),
		"raw_label": stringSchema(200), "rank": enumSchema("primary", "additional"),
		"basis":      enumSchema("operator_assigned", "package_column", "transfer_record"),
		"source_ref": stringSchema(512), "revision": integerSchema(1, 0), "recorded_at": dateTimeSchema(),
	}
}

func custodianPageOutputSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"items": arraySchema(custodianAssignmentSchema(), 250), "total": integerSchema(0, 0),
		"next_cursor": stringSchema(maxCursorCharacters),
	}), cacheRequired("items", "total")...)
}

func listPackageCustodiansSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		packageIDField: uuidSchema(), "row_id": schema{"type": "string", "minLength": 64, "maxLength": 64, "pattern": "^[0-9a-f]{64}$",
			"description": "Return claims for this record only. Omit to return package-level defaults only."},
		"unresolved_only": booleanSchema(), "cursor": stringSchema(maxCursorCharacters), "limit": integerSchema(1, 250),
	}, packageIDField), custodianPageOutputSchema()
}

func findPeopleSchemas() (schema, schema) {
	person := objectSchema(schema{
		"person_id": uuidSchema(), "display_name": stringSchema(200), stateField: enumSchema("provisional", "curated"),
		"revision": integerSchema(1, 0),
	}, "person_id", "display_name", stateField, "revision")
	return rootObjectSchema(schema{
			"query": stringSchema(200), "cursor": stringSchema(maxCursorCharacters), "limit": integerSchema(1, 250),
		}), rootObjectSchema(withPrivateCache(schema{
			"items": arraySchema(person, 250), "next_cursor": stringSchema(maxCursorCharacters),
		}), cacheRequired("items")...)
}

func resolvePackageCustodianSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"assignment_id": uuidSchema(), "person_id": uuidSchema(), "if_match_revision": integerSchema(1, 0),
		}, "assignment_id", "person_id", "if_match_revision"), rootObjectSchema(withPrivateCache(custodianAssignmentProperties()),
			cacheRequired("assignment_id", "scope_kind", "raw_label", "rank", "basis", "source_ref", "revision", "recorded_at")...)
}

func assignPackageCustodianSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			packageIDField: uuidSchema(), "row_id": schema{"type": "string", "minLength": 64, "maxLength": 64, "pattern": "^[0-9a-f]{64}$"},
			"raw_label": schema{"type": "string", "minLength": 1, "maxLength": 200}, "person_id": uuidSchema(),
			"if_match_revision": integerSchema(1, 0),
		}, packageIDField, "raw_label", "if_match_revision"), rootObjectSchema(withPrivateCache(custodianAssignmentProperties()),
			cacheRequired("assignment_id", "scope_kind", "raw_label", "rank", "basis", "source_ref", "revision", "recorded_at")...)
}

func listPackagesSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"direction": enumSchema("received", "produced"),
			"page_size": integerSchema(1, 250),
			"cursor":    uuidSchema(),
		}), rootObjectSchema(withPrivateCache(schema{
			"items":     arraySchema(schema{"type": "object"}, 250),
			"direction": enumSchema("received", "produced"),
			"after":     stringSchema(36), "next_after": stringSchema(36),
			"limit": integerSchema(1, 250),
		}), cacheRequired("items", "limit")...)
}

func getPackageSchemas() (schema, schema) {
	return rootObjectSchema(schema{packageIDField: uuidSchema()}, packageIDField), rootObjectSchema(withPrivateCache(schema{
		packageIDField: uuidSchema(), "snapshot_id": uuidSchema(),
		"direction": enumSchema("received", "produced"), "package_name": stringSchema(128),
		"party_label": stringSchema(64), "profile_sha256": sha256Schema(), "mapping_sha256": sha256Schema(),
		"manifest_sha256": sha256Schema(), "manifest_blob_sha256": sha256Schema(),
		"predecessor_package_id": uuidSchema(), "relation": stringSchema(64), "ingest_id": uuidSchema(),
		"export_plan_id": uuidSchema(), "produced_on": dateTimeSchema(), stateField: stringSchema(32),
		"created_at": dateTimeSchema(), "completed_at": dateTimeSchema(),
		"member_count": integerSchema(0, 100_000), "page_count": integerSchema(0, 1_000_000),
		"profile_json": stringSchema(1 << 20), "mapping_json": stringSchema(1 << 20),
		"volumes": arraySchema(schema{"type": "object"}, 64),
	}), cacheRequired(packageIDField, "direction", "package_name", "party_label", "profile_sha256",
		"mapping_sha256", "manifest_sha256", "manifest_blob_sha256", stateField, "created_at",
		"member_count", "page_count", "profile_json", "mapping_json", "volumes")...)
}

func listPackageMembersSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			packageIDField: uuidSchema(), "after_ordinal": integerSchema(0, 100_000),
			"page_size": integerSchema(1, 250),
		}, packageIDField), rootObjectSchema(withPrivateCache(schema{
			"items":         arraySchema(schema{"type": "object"}, 250),
			"after_ordinal": integerSchema(0, 100_000), "next_after_ordinal": integerSchema(0, 100_000),
			"limit": integerSchema(1, 250),
		}), cacheRequired("items", "limit")...)
}

func getPackageRecordSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			packageIDField: uuidSchema(), "row_id": sha256Schema(),
		}, packageIDField, "row_id"), rootObjectSchema(withPrivateCache(schema{
			packageIDField: uuidSchema(), "row_id": sha256Schema(), "load_file": stringSchema(4096),
			"row_ordinal": integerSchema(1, 100_000), "occurrence_id": stringSchema(32),
			"raw_json": stringSchema(1 << 20), "raw_sha256": sha256Schema(), "sensitive": booleanSchema(),
		}), cacheRequired(packageIDField, "row_id", "load_file", "row_ordinal", "occurrence_id", "raw_sha256", "sensitive")...)
}

func lookupBatesLabelSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"label": stringSchema(256), packageIDField: uuidSchema(), "label_set": stringSchema(256),
			"provenance": enumSchema("received", "assigned"), "cursor": stringSchema(maxCursorCharacters),
			"page_size": integerSchema(1, 250),
		}, "label"), rootObjectSchema(withPrivateCache(schema{
			"items": arraySchema(schema{"type": "object"}, 250), "next_cursor": stringSchema(maxCursorCharacters),
			"limit": integerSchema(1, 250),
		}), cacheRequired("items", "limit")...)
}

func privateCacheProperties() schema {
	return schema{
		"ttlMs":      schema{"type": "integer", "const": 0},
		"cacheScope": schema{"type": "string", "const": "private"},
	}
}

func withPrivateCache(properties schema) schema {
	maps.Copy(properties, privateCacheProperties())
	return properties
}

func cacheRequired(required ...string) []string {
	return append(required, "ttlMs", "cacheScope")
}

func renditionIdentitySchema() schema {
	return objectSchema(schema{
		"profile_fingerprint": sha256Schema(),
		"attachment_id":       sha256Schema(),
		"build_id":            sha256Schema(),
	}, "profile_fingerprint", "attachment_id", "build_id")
}

func documentSummarySchema() schema {
	return objectSchema(schema{
		"node_id":                 integerSchema(1, 0), //nolint:goconst // Stable wire field is repeated across tools.
		"content_version_id":      uuidSchema(),        //nolint:goconst // Stable wire field is repeated across tools.
		"path":                    schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters, "pattern": "^/"},
		"name":                    schema{"type": "string", "minLength": 1, "maxLength": store.MaxDocumentCatalogNameCharacters},
		"media_type":              stringSchema(255),
		"size":                    integerSchema(0, 0),
		"modified_at":             dateTimeSchema(),
		"latest_processing_state": stringSchema(64),
		"active_renditions":       arraySchema(renditionIdentitySchema(), 64),
	}, "node_id", "content_version_id", "path", "name", "media_type", "size", "modified_at", "active_renditions")
}

func fenceInputProperties() schema {
	return schema{
		"content_version_ids": schema{
			"type": "array", "items": uuidSchema(), "minItems": 1, "maxItems": 4096, "uniqueItems": true,
		},
		"filters": objectSchema(schema{
			"tag_id":          uuidSchema(),
			"mime_type":       stringSchema(255),
			"under_node_id":   integerSchema(1, 0),
			"modified_since":  dateTimeSchema(),
			"modified_before": dateTimeSchema(),
		}),
	}
}

func fenceAuthoritySchema() schema {
	return objectSchema(schema{
		"vault_id":            uuidSchema(),
		"content_version_ids": schema{"type": "array", "items": uuidSchema(), "maxItems": 4096, "uniqueItems": true},
	}, "vault_id", "content_version_ids")
}

func getVaultInfoSchemas() (schema, schema) {
	input := rootObjectSchema(schema{})
	output := rootObjectSchema(withPrivateCache(schema{
		"vault_id":              uuidSchema(),
		"live_files":            integerSchema(0, 0),
		"live_directories":      integerSchema(0, 0),
		"trashed_nodes":         integerSchema(0, 0),
		"content_versions":      integerSchema(0, 0),
		"logical_version_bytes": integerSchema(0, 0),
		"tracked_blobs":         integerSchema(0, 0),
		"tracked_blob_bytes":    integerSchema(0, 0),
	}), cacheRequired("vault_id", "live_files", "live_directories", "trashed_nodes", "content_versions",
		"logical_version_bytes", "tracked_blobs", "tracked_blob_bytes")...)
	return input, output
}

func listDocumentsSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"path_prefix": schema{"type": "string", "maxLength": maxPathCharacters, "pattern": "^/"},
		"sort":        enumSchema("path", "name", "modified_at", "size", "media_type"),
		"direction":   enumSchema("asc", "desc"),
		"page_size":   integerSchema(1, 250),
		"cursor":      cursorSchema(),
	})
	output := rootObjectSchema(withPrivateCache(schema{
		"path_prefix":     schema{"type": "string", "maxLength": maxPathCharacters, "pattern": "^/"},
		"sort":            enumSchema("path", "name", "modified_at", "size", "media_type"),
		"direction":       enumSchema("asc", "desc"),
		"page_size":       integerSchema(1, 250),
		"items":           arraySchema(documentSummarySchema(), 250),
		"next_cursor":     cursorSchema(),
		"previous_cursor": cursorSchema(),
	}), cacheRequired("path_prefix", "sort", "direction", "page_size", "items")...)
	return input, output
}

func searchDocumentsSchemas() (schema, schema) {
	properties := fenceInputProperties()
	properties["query"] = schema{"type": "string", "minLength": 1, "maxLength": 8192}
	properties["mode"] = enumSchema("auto", "lexical", "semantic", "hybrid")
	properties["limit"] = integerSchema(1, 100)
	properties["profile"] = schema{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[a-z][a-z0-9_-]*$"}
	properties["binding_id"] = stringSchema(128)
	properties["explain"] = booleanSchema()
	input := rootObjectSchema(properties, "query", "profile")
	// Keep the exactly-one-scope rule without a root oneOf, which Anthropic rejects.
	input["if"] = schema{"required": []string{"content_version_ids"}}
	input["then"] = schema{"not": schema{"required": []string{"filters"}}}
	input["else"] = schema{"required": []string{"filters"}}
	result := objectSchema(schema{
		"node_id":            integerSchema(1, 0),
		"content_version_id": uuidSchema(),
		"rank":               integerSchema(1, 100),
		"score":              schema{"type": "number"},
		"path":               schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters, "pattern": "^/"},
		"excerpt":            stringSchema(512),
		"evidence_ids":       arraySchema(stringSchema(1024), 64),
	}, "node_id", "content_version_id", "rank", "score", "path", "evidence_ids")
	output := rootObjectSchema(withPrivateCache(schema{
		"vault_id":             uuidSchema(),
		"fence":                fenceAuthoritySchema(),
		"fence_fingerprint":    schema{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$", "maxLength": 71},
		"observed_scope_count": integerSchema(0, 0),
		"requested_mode":       enumSchema("auto", "lexical", "semantic", "hybrid"),
		"actual_mode":          enumSchema("lexical", "semantic", "hybrid"),
		"coverage": objectSchema(schema{
			"binding_required":   booleanSchema(),
			"scoped_documents":   integerSchema(0, 4096),
			"complete_documents": integerSchema(0, 4096),
			stateField:           stringSchema(64),
		}, "binding_required", "scoped_documents", "complete_documents", stateField),
		"skipped_reasons": arraySchema(stringSchema(64), 64),
		"results":         arraySchema(result, 100),
		"truncated":       booleanSchema(),
	}), cacheRequired("vault_id", "fence", "fence_fingerprint", "observed_scope_count", "requested_mode",
		"actual_mode", "coverage", "skipped_reasons", "results", "truncated")...)
	return input, output
}

func getDocumentSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"node_id":            integerSchema(1, 0),
		"content_version_id": uuidSchema(),
	}, "node_id", "content_version_id")
	output := rootObjectSchema(withPrivateCache(schema{
		"node_id":            integerSchema(1, 0),
		"content_version_id": uuidSchema(),
		"path":               schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters, "pattern": "^/"},
		"name":               schema{"type": "string", "minLength": 1, "maxLength": store.MaxDocumentCatalogNameCharacters},
		"media_type":         stringSchema(255),
		"size":               integerSchema(0, 0),
		"modified_at":        dateTimeSchema(),
		"active_renditions":  arraySchema(renditionIdentitySchema(), 64),
	}), cacheRequired("node_id", "content_version_id", "path", "name", "media_type", "size", "modified_at", "active_renditions")...)
	return input, output
}

func listDocumentVersionsSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"node_id": integerSchema(1, 0),
		"limit":   integerSchema(1, 250),
		"offset":  integerSchema(0, 1_000_000),
	}, "node_id")
	item := objectSchema(schema{
		"node_id":            integerSchema(1, 0),
		"content_version_id": uuidSchema(),
		"size":               integerSchema(0, 0),
		"media_type":         stringSchema(255),
		"recorded_at":        dateTimeSchema(),
		"is_current":         booleanSchema(),
	}, "node_id", "content_version_id", "size", "media_type", "recorded_at", "is_current")
	output := rootObjectSchema(withPrivateCache(schema{
		"node_id": integerSchema(1, 0), "items": arraySchema(item, 250),
		"total": integerSchema(0, 0), "limit": integerSchema(1, 250), "offset": integerSchema(0, 1_000_000),
	}), cacheRequired("node_id", "items", "total", "limit", "offset")...)
	return input, output
}

func readRenditionTextSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"vault_id":           uuidSchema(),
		"node_id":            integerSchema(1, 0),
		"content_version_id": uuidSchema(),
		"attachment_id":      sha256Schema(),
		"offset":             integerSchema(0, 1<<31-1),
		"max_chars":          schema{"type": "integer", "minimum": 1, "maximum": maxRenditionChars, "default": defaultRenditionChars},
	}, "vault_id", "node_id", "content_version_id", "attachment_id")
	output := rootObjectSchema(withPrivateCache(schema{
		"vault_id": uuidSchema(), "node_id": integerSchema(1, 0), "content_version_id": uuidSchema(),
		"attachment_id": sha256Schema(), "build_id": sha256Schema(), "profile_fingerprint": sha256Schema(),
		"text": stringSchema(maxRenditionChars), "media_type": schema{"type": "string", "const": "text/markdown"},
		"checksum": sha256Schema(), "requested_offset": integerSchema(0, 1<<31-1),
		"actual_start": integerSchema(0, 1<<31-1), "actual_end": integerSchema(0, 1<<31-1),
		"next_offset": integerSchema(0, 1<<31-1), "eof": booleanSchema(),
		"response_bytes": integerSchema(0, maxToolResponseBytes),
	}), cacheRequired("vault_id", "node_id", "content_version_id", "attachment_id", "build_id", "profile_fingerprint",
		"text", "media_type", "checksum", "requested_offset", "actual_start", "actual_end", "next_offset", "eof", "response_bytes")...)
	return input, output
}

func processingSelectorProperties() schema {
	return schema{
		"node_id": integerSchema(1, 0), "content_version_id": uuidSchema(),
		"profile": schema{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[a-z][a-z0-9_-]*$"},
	}
}

func getProcessingPlanSchemas() (schema, schema) {
	input := rootObjectSchema(processingSelectorProperties(), "node_id", "content_version_id", "profile")
	runtimeDisclosure := objectSchema(schema{
		"immediate_processor":     schema{"type": "string", "minLength": 1, "maxLength": 1024},
		"ultimate_processor":      schema{"type": "string", "minLength": 1, "maxLength": 1024},
		"endpoint":                schema{"type": "string", "minLength": 1, "maxLength": 1024},
		"deployment":              schema{"type": "string", "minLength": 1, "maxLength": 1024},
		"model":                   stringSchema(1024),
		"model_revision":          stringSchema(1024),
		"vector_space":            stringSchema(1024),
		"metadata_classes":        schema{"type": "array", "items": stringSchema(128), "maxItems": 64, "uniqueItems": true},
		"retained_artifact_roles": schema{"type": "array", "items": stringSchema(128), "maxItems": 64, "uniqueItems": true},
	}, "immediate_processor", "ultimate_processor", "endpoint", "deployment", "metadata_classes", "retained_artifact_roles")
	flowHop := objectSchema(schema{
		"capability":         enumSchema("rendition", "embedding", "query_embedding"),
		"provider_id":        stringSchema(128),
		"trust_boundary":     enumSchema("local_process", "operator_network", "hosted_provider"),
		"input_classes":      schema{"type": "array", "items": enumSchema("original_file", "rendition_chunk", "query_text"), "maxItems": 3, "uniqueItems": true},
		"runtime_disclosure": runtimeDisclosure,
		"disclose_filename":  booleanSchema(),
		"filename":           stringSchema(255),
	}, "capability", "provider_id", "trust_boundary", "input_classes", "runtime_disclosure", "disclose_filename")
	output := rootObjectSchema(withPrivateCache(schema{
		"fingerprint":         sha256Schema(),
		"vault_uid":           uuidSchema(),
		"selector":            objectSchema(processingSelectorProperties(), "node_id", "content_version_id", "profile"),
		"profile_fingerprint": sha256Schema(),
		"flow":                arraySchema(flowHop, 129),
		"disclosed_classes": schema{
			"type": "array", "items": stringSchema(128), "maxItems": 129, "uniqueItems": true,
		},
		"retained_classes": schema{
			"type": "array", "items": stringSchema(128), "maxItems": 129, "uniqueItems": true,
		},
		"estimate": objectSchema(schema{
			"source_bytes": integerSchema(0, 0), "provider_calls": integerSchema(0, 0), "vector_spaces": integerSchema(0, 0),
		}, "source_bytes", "provider_calls", "vector_spaces"),
		"consent_required":   booleanSchema(),
		"consent_state":      enumSchema("active", "required", "expired", "revoked"),
		"backup_consequence": stringSchema(4096),
	}), cacheRequired("fingerprint", "vault_uid", "selector", "profile_fingerprint", "flow", "disclosed_classes",
		"retained_classes", "estimate", "consent_required", "consent_state", "backup_consequence")...)
	return input, output
}

func getProcessingStatusSchemas() (schema, schema) {
	input := rootObjectSchema(schema{mcpJobIDField: sha256Schema()}, mcpJobIDField)
	output := rootObjectSchema(withPrivateCache(schema{
		mcpJobIDField: sha256Schema(), "content_version_id": uuidSchema(), stateField: stringSchema(64), "phase": stringSchema(64),
		"failure_code": stringSchema(64), "embedding_job_ids": arraySchema(sha256Schema(), 64),
		"completed_bindings": integerSchema(0, 64),
	}), cacheRequired(mcpJobIDField, stateField, "phase", "embedding_job_ids", "completed_bindings")...)
	return input, output
}

func getProcessingCoverageSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"profile":             schema{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[a-z][a-z0-9_-]*$"},
		"vault_id":            uuidSchema(),
		"content_version_ids": schema{"type": "array", "items": uuidSchema(), "minItems": 1, "maxItems": 4096, "uniqueItems": true},
	}, "profile", "vault_id", "content_version_ids")
	class := objectSchema(schema{
		"name": stringSchema(128), "required": booleanSchema(), stateField: stringSchema(64),
		"complete": integerSchema(0, 4096), "unavailable": integerSchema(0, 4096), "stale": integerSchema(0, 4096),
		"ineligible": integerSchema(0, 4096), "rebuilding": integerSchema(0, 4096), "total": integerSchema(0, 4096),
		"previous_generation_serving": integerSchema(0, 4096),
	}, "name", "required", stateField, "complete", "unavailable", "stale", "ineligible", "rebuilding",
		"previous_generation_serving", "total")
	output := rootObjectSchema(withPrivateCache(schema{
		"vault_id": uuidSchema(), "content_version_ids": arraySchema(uuidSchema(), 4096),
		"profile_fingerprint": sha256Schema(), stateField: stringSchema(64), "coverage": arraySchema(class, 65),
	}), cacheRequired("vault_id", "content_version_ids", "profile_fingerprint", stateField, "coverage")...)
	return input, output
}

func getFormatCoverageSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"family": stringSchema(64), "format": stringSchema(64), "extension": stringSchema(16),
	})
	input["not"] = schema{"required": []string{"format", "extension"}}
	state := objectSchema(schema{
		"evidence": stringSchema(512), "note": stringSchema(512),
		"provider": stringSchema(512), "provider_fingerprint": stringSchema(64),
		"state": enumSchema("qualified", "unqualified", "unsupported", "not_applicable"),
	}, "evidence", "note", "provider", "provider_fingerprint", "state")
	capabilityKeys := []string{"detect", "retain", "metadata", "expand", "text", "pages", "transcript"}
	capabilities := schema{}
	for _, key := range capabilityKeys {
		capabilities[key] = state
	}
	capabilityMap := objectSchema(capabilities, capabilityKeys...)
	format := objectSchema(schema{
		"capabilities": capabilityMap,
		"extensions":   arraySchema(stringSchema(512), 512),
		"id":           stringSchema(512),
		"media_type":   stringSchema(512),
		"query_family": stringSchema(512),
		"unit_kind":    stringSchema(512),
		"variants": arraySchema(objectSchema(schema{
			"capabilities": capabilityMap,
		}, "capabilities"), 256),
	}, "capabilities", "extensions", "id", "media_type", "query_family", "unit_kind", "variants")
	pending := objectSchema(schema{
		"extensions": arraySchema(stringSchema(512), 512), "label": stringSchema(512),
		"note": stringSchema(512), "owner_slice": stringSchema(512),
	}, "extensions", "label", "note", "owner_slice")
	lookup := objectSchema(schema{
		"format": format, "match": enumSchema("format", "pending", "unknown_format"),
		"pending": pending, "query": stringSchema(512),
	}, "match", "query")
	output := rootObjectSchema(withPrivateCache(schema{
		"contract_version": schema{"type": "string", "const": "format-coverage/v1"},
		"formats":          arraySchema(format, 512),
		"generated_by": objectSchema(schema{
			"bound_providers": arraySchema(stringSchema(512), 512),
			"catalog_rows":    integerSchema(0, 512),
			"extractor_id":    stringSchema(512),
		}, "bound_providers", "catalog_rows", "extractor_id"),
		"pending": arraySchema(pending, 512), "lookup": lookup,
	}), cacheRequired("contract_version", "formats", "generated_by", "pending")...)
	return input, output
}

func startProcessingSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"content_version_id": uuidSchema(), "plan_fingerprint": sha256Schema(),
	}, "content_version_id", "plan_fingerprint")
	output := rootObjectSchema(withPrivateCache(schema{
		mcpJobIDField: sha256Schema(), "rendition_job_id": sha256Schema(), "attachment_id": sha256Schema(),
		"embedding_job_ids": arraySchema(sha256Schema(), 64), "profile_fingerprint": sha256Schema(),
		"content_version_id": uuidSchema(), stateField: schema{"type": "string", "const": "queued"},
	}), cacheRequired(mcpJobIDField, "embedding_job_ids", "profile_fingerprint", "content_version_id", stateField)...)
	return input, output
}
