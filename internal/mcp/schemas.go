package mcp

import "maps"

const (
	jsonSchemaDraft       = "https://json-schema.org/draft/2020-12/schema"
	maxToolResponseBytes  = 1 << 20
	maxToolErrorBytes     = 1024
	maxPathBytes          = 16 << 10
	maxCursorBytes        = 2048
	maxPathCharacters     = 16 << 10
	maxCursorCharacters   = 2048
	maxRenditionChars     = 16_000
	defaultRenditionChars = 8_000
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
		"name":                    schema{"type": "string", "minLength": 1, "maxLength": 255},
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
	properties["explain"] = schema{"type": "boolean"}
	input := rootObjectSchema(properties, "query", "profile")
	input["oneOf"] = []any{
		schema{"required": []string{"content_version_ids"}},
		schema{"required": []string{"filters"}},
	}
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
			"binding_required":   schema{"type": "boolean"},
			"scoped_documents":   integerSchema(0, 4096),
			"complete_documents": integerSchema(0, 4096),
			"state":              stringSchema(64),
		}, "binding_required", "scoped_documents", "complete_documents", "state"),
		"skipped_reasons": arraySchema(stringSchema(64), 64),
		"results":         arraySchema(result, 100),
		"truncated":       schema{"type": "boolean"},
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
		"name":               schema{"type": "string", "minLength": 1, "maxLength": 255},
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
		"is_current":         schema{"type": "boolean"},
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
		"next_offset": integerSchema(0, 1<<31-1), "eof": schema{"type": "boolean"},
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
	}, "capability", "provider_id", "trust_boundary", "input_classes", "runtime_disclosure")
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
		"consent_required":   schema{"type": "boolean"},
		"consent_state":      enumSchema("active", "required", "expired", "revoked"),
		"backup_consequence": stringSchema(4096),
	}), cacheRequired("fingerprint", "vault_uid", "selector", "profile_fingerprint", "flow", "disclosed_classes",
		"retained_classes", "estimate", "consent_required", "consent_state", "backup_consequence")...)
	return input, output
}

func getProcessingStatusSchemas() (schema, schema) {
	input := rootObjectSchema(schema{"job_id": sha256Schema()}, "job_id")
	output := rootObjectSchema(withPrivateCache(schema{
		"job_id": sha256Schema(), "content_version_id": uuidSchema(), "state": stringSchema(64), "phase": stringSchema(64),
		"failure_code": stringSchema(64), "embedding_job_ids": arraySchema(sha256Schema(), 64),
		"completed_bindings": integerSchema(0, 64),
	}), cacheRequired("job_id", "state", "phase", "embedding_job_ids", "completed_bindings")...)
	return input, output
}

func getProcessingCoverageSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"profile":             schema{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[a-z][a-z0-9_-]*$"},
		"vault_id":            uuidSchema(),
		"content_version_ids": schema{"type": "array", "items": uuidSchema(), "minItems": 1, "maxItems": 4096, "uniqueItems": true},
	}, "profile", "vault_id", "content_version_ids")
	class := objectSchema(schema{
		"name": stringSchema(128), "required": schema{"type": "boolean"}, "state": stringSchema(64),
		"complete": integerSchema(0, 4096), "unavailable": integerSchema(0, 4096), "stale": integerSchema(0, 4096),
		"ineligible": integerSchema(0, 4096), "rebuilding": integerSchema(0, 4096), "total": integerSchema(0, 4096),
		"previous_generation_serving": integerSchema(0, 4096),
	}, "name", "required", "state", "complete", "unavailable", "stale", "ineligible", "rebuilding",
		"previous_generation_serving", "total")
	output := rootObjectSchema(withPrivateCache(schema{
		"vault_id": uuidSchema(), "content_version_ids": arraySchema(uuidSchema(), 4096),
		"profile_fingerprint": sha256Schema(), "state": stringSchema(64), "coverage": arraySchema(class, 65),
	}), cacheRequired("vault_id", "content_version_ids", "profile_fingerprint", "state", "coverage")...)
	return input, output
}

func startProcessingSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"content_version_id": uuidSchema(), "plan_fingerprint": sha256Schema(),
	}, "content_version_id", "plan_fingerprint")
	output := rootObjectSchema(withPrivateCache(schema{
		"job_id": sha256Schema(), "rendition_job_id": sha256Schema(), "attachment_id": sha256Schema(),
		"embedding_job_ids": arraySchema(sha256Schema(), 64), "profile_fingerprint": sha256Schema(),
		"content_version_id": uuidSchema(), "state": schema{"type": "string", "const": "queued"},
	}), cacheRequired("job_id", "embedding_job_ids", "profile_fingerprint", "content_version_id", "state")...)
	return input, output
}
