package mcp

import "go.kenn.io/docbank/document/bundle"

const (
	maxMCPExportMembers = 100
	exportNodeIDField   = "node_id"
)

func exportMemberSchema() schema {
	return objectSchema(schema{
		exportNodeIDField: integerSchema(1, 0), "version_id": uuidSchema(),
		"sha256": sha256Schema(), "size": integerSchema(0, bundle.MaxRoleBytes),
		"revision": integerSchema(1, 0),
	}, exportNodeIDField, "version_id", "sha256", "size")
}

func exportRolePolicySchema() schema {
	return objectSchema(schema{
		"role":              enumSchema("original", "text", "pages", "email_pdf", "attachment_original", "attachment_pdf"),
		"allow_unavailable": booleanSchema(), "profile_fingerprint": sha256Schema(),
		"recipe_sha256": sha256Schema(),
	}, "role")
}

func exportSourceResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"source_id": uuidSchema(), "member_hash": sha256Schema(),
		"total": integerSchema(1, maxMCPExportMembers), stateField: stringSchema(32),
	}), cacheRequired("source_id", "member_hash", "total", stateField)...)
}

func createExportSourceSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"operation_id": uuidSchema(), "members": arraySchema(exportMemberSchema(), maxMCPExportMembers),
	}, "operation_id", "members"), exportSourceResultSchema()
}

func exportPlanResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"plan_id": uuidSchema(), "source_id": uuidSchema(),
		"fingerprint": sha256Schema(), "total": integerSchema(1, maxMCPExportMembers),
	}), cacheRequired("plan_id", "source_id", "fingerprint", "total")...)
}

func createExportPlanSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"operation_id": uuidSchema(), "source_id": uuidSchema(),
		"member_hash": sha256Schema(), "roles": arraySchema(exportRolePolicySchema(), 8),
	}, "operation_id", "source_id", "member_hash", "roles"), exportPlanResultSchema()
}

func exportRoleSummarySchema() schema {
	return objectSchema(schema{
		"role": stringSchema(64), "available_members": integerSchema(0, maxMCPExportMembers),
		"unavailable_members": integerSchema(0, maxMCPExportMembers),
		"files":               integerSchema(0, maxMCPExportMembers),
		"bytes":               integerSchema(0, bundle.MaxRoleBytes),
		"collapsed_files":     integerSchema(0, maxMCPExportMembers),
		"unavailable_reason":  stringSchema(128),
	}, "role", "available_members", "unavailable_members", "files", "bytes")
}

func previewExportPlanSchemas() (schema, schema) {
	return rootObjectSchema(schema{"plan_id": uuidSchema()}, "plan_id"),
		rootObjectSchema(withPrivateCache(schema{
			"plan_id": uuidSchema(), "fingerprint": sha256Schema(),
			"member_hash": sha256Schema(), "total": integerSchema(1, maxMCPExportMembers),
			"roles": arraySchema(exportRoleSummarySchema(), 8),
		}), cacheRequired("plan_id", "fingerprint", "member_hash", "total", "roles")...)
}

func exportReceiptSchema() schema {
	return objectSchema(schema{
		"format": stringSchema(64), "plan_fingerprint": sha256Schema(),
		"sha256": sha256Schema(), "size": integerSchema(1, bundle.MaxArchiveBytes),
		"entries": integerSchema(1, 0),
	}, "format", "plan_fingerprint", "sha256", "size", "entries")
}

func exportJobResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"job_id": uuidSchema(), "plan_id": uuidSchema(), "fingerprint": sha256Schema(),
		stateField: stringSchema(32), "sequence": integerSchema(1, 0),
		"receipt": exportReceiptSchema(),
	}), cacheRequired("job_id", "plan_id", "fingerprint", stateField, "sequence")...)
}

func startExportJobSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"operation_id": uuidSchema(), "plan_id": uuidSchema(), "fingerprint": sha256Schema(),
	}, "operation_id", "plan_id", "fingerprint"), exportJobResultSchema()
}

func getExportJobSchemas() (schema, schema) {
	return rootObjectSchema(schema{"job_id": uuidSchema()}, "job_id"), exportJobResultSchema()
}

func cancelExportJobSchemas() (schema, schema) {
	return rootObjectSchema(schema{"job_id": uuidSchema()}, "job_id"),
		rootObjectSchema(withPrivateCache(schema{
			"job_id": uuidSchema(), "accepted": booleanSchema(),
		}), cacheRequired("job_id", "accepted")...)
}
