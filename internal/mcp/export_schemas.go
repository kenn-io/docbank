package mcp

import (
	"maps"
	"math"

	"go.kenn.io/docbank/document/bundle"
)

//nolint:goconst // Bundle wire field names remain beside their schemas.
func previewExportSchemas() (schema, schema) {
	member := objectSchema(schema{
		"node_id": integerSchema(1, math.MaxInt64), "version_id": uuidSchema(),
		"sha256": sha256Schema(), "size": integerSchema(0, math.MaxInt64),
		"revision": integerSchema(0, math.MaxInt64),
	}, "node_id", "version_id", "sha256", "size")
	members := arraySchema(member, bundle.ChunkMembers)
	members["minItems"] = 1
	input := rootObjectSchema(schema{
		"source_operation_id": uuidSchema(), "plan_operation_id": uuidSchema(), "members": members,
	}, "source_operation_id", "plan_operation_id", "members")
	source := objectSchema(schema{
		"id": uuidSchema(), "request_sha256": sha256Schema(), "kind": enumSchema("explicit"),
		"state": enumSchema("sealed"), "member_hash": sha256Schema(),
		"total":        integerSchema(1, bundle.ChunkMembers),
		"source_bytes": integerSchema(0, bundle.MaxRoleBytes),
		"created_at":   dateTimeSchema(), "expires_at": dateTimeSchema(),
	}, "id", "request_sha256", "kind", "state", "member_hash", "total", "source_bytes",
		"created_at", "expires_at")
	plan := exportPlanSchema(schema{
		"source": source, "total": integerSchema(1, bundle.ChunkMembers),
		"roles": arraySchema(objectSchema(schema{"role": enumSchema("original")}, "role"), 1),
	})
	return input, rootObjectSchema(withPrivateCache(schema{"plan": plan}), cacheRequired("plan")...)
}

func exportPlanSchema(properties schema) schema {
	common := schema{
		"format": enumSchema(bundle.Format), "id": uuidSchema(), "vault_id": uuidSchema(),
		"toolchain": stringSchema(256), "fingerprint": sha256Schema(),
		"role_entries":   integerSchema(0, bundle.MaxRoles),
		"role_bytes":     integerSchema(0, bundle.MaxRoleBytes),
		"metadata_bytes": integerSchema(0, bundle.MaxMetadataBytes),
		"counts":         exportOutputCountsSchema(),
		"created_at":     dateTimeSchema(), "expires_at": dateTimeSchema(),
	}
	maps.Copy(common, properties)
	return objectSchema(common,
		"format", "id", "vault_id", "toolchain", "source", "roles", "fingerprint", "total",
		"role_entries", "role_bytes", "metadata_bytes", "created_at", "expires_at")
}

func exportReceiptSchema() schema {
	return objectSchema(schema{
		"format": enumSchema(bundle.Format), "plan_fingerprint": sha256Schema(),
		"sha256": sha256Schema(), "size": integerSchema(1, bundle.MaxArchiveBytes),
		"entries": integerSchema(0, 0),
	}, "format", "plan_fingerprint", "sha256", "size", "entries")
}

func exportJobSchema() schema {
	return objectSchema(schema{
		"id": uuidSchema(), "plan_id": uuidSchema(), "fingerprint": sha256Schema(),
		"state":    enumSchema("queued", "running", "completed", "failed", "canceled"),
		"sequence": integerSchema(0, 0), "completed_roles": integerSchema(0, bundle.MaxRoles),
		"completed_bytes": integerSchema(0, bundle.MaxRoleBytes), "attempt": integerSchema(0, 0),
		"failure": stringSchema(256), "created_at": dateTimeSchema(), "deadline": dateTimeSchema(),
		"expires_at": dateTimeSchema(), "receipt": exportReceiptSchema(),
	}, "id", "plan_id", "fingerprint", "state", "sequence", "completed_roles", "completed_bytes",
		"attempt", "created_at", "deadline", "expires_at")
}

func startExportSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"operation_id": uuidSchema(), "plan_id": uuidSchema(), "fingerprint": sha256Schema(),
		}, "operation_id", "plan_id", "fingerprint"),
		rootObjectSchema(withPrivateCache(schema{"job": exportJobSchema()}), cacheRequired("job")...)
}

func getExportStatusSchemas() (schema, schema) {
	return rootObjectSchema(schema{schemaJobIDField: uuidSchema()}, schemaJobIDField),
		rootObjectSchema(withPrivateCache(schema{"job": exportJobSchema()}), cacheRequired("job")...)
}

//nolint:goconst // JSON Schema vocabulary is intentionally repeated.
func cancelExportSchemas() (schema, schema) {
	return rootObjectSchema(schema{schemaJobIDField: uuidSchema()}, schemaJobIDField),
		rootObjectSchema(withPrivateCache(schema{
			schemaJobIDField: uuidSchema(), "accepted": schema{"type": "boolean", "const": true},
		}), cacheRequired(schemaJobIDField, "accepted")...)
}

func releaseExportSchemas() (schema, schema) {
	return rootObjectSchema(schema{schemaJobIDField: uuidSchema()}, schemaJobIDField),
		rootObjectSchema(withPrivateCache(schema{
			schemaJobIDField: uuidSchema(), "released": schema{"type": "boolean", "const": true},
		}), cacheRequired(schemaJobIDField, "released")...)
}

func downloadExportSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			schemaJobIDField: uuidSchema(), "destination_path": path, "overwrite": booleanSchema(),
		}, schemaJobIDField, "destination_path"), rootObjectSchema(withPrivateCache(schema{
			schemaJobIDField: uuidSchema(), "destination_path": path, "receipt": exportReceiptSchema(),
			"state":          enumSchema("published", "published_durability_unknown"),
			"cleanup_failed": booleanSchema(),
		}), cacheRequired(schemaJobIDField, "destination_path", "receipt", "state", "cleanup_failed")...)
}
