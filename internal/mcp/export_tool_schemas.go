package mcp

import "go.kenn.io/docbank/document/bundle"

const (
	maxMCPExportMembers = 100
	exportNodeIDField   = "node_id"
	exportTotalField    = "total"
	exportItemsField    = "items"
	mcpJobIDField       = "job_id"
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
		exportTotalField: integerSchema(1, maxMCPExportMembers), stateField: stringSchema(32),
	}), cacheRequired("source_id", "member_hash", exportTotalField, stateField)...)
}

func createExportSourceSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"operation_id": uuidSchema(), "members": arraySchema(exportMemberSchema(), maxMCPExportMembers),
	}, "operation_id", "members"), exportSourceResultSchema()
}

func exportPlanResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"plan_id": uuidSchema(), "source_id": uuidSchema(),
		"fingerprint": sha256Schema(), exportTotalField: integerSchema(1, maxMCPExportMembers),
	}), cacheRequired("plan_id", "source_id", "fingerprint", exportTotalField)...)
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
			"member_hash": sha256Schema(), exportTotalField: integerSchema(1, maxMCPExportMembers),
			"roles": arraySchema(exportRoleSummarySchema(), 8),
		}), cacheRequired("plan_id", "fingerprint", "member_hash", exportTotalField, "roles")...)
}

func getExportPlanSchemas() (schema, schema) {
	return rootObjectSchema(schema{"plan_id": uuidSchema()}, "plan_id"),
		rootObjectSchema(withPrivateCache(schema{
			"plan_id": uuidSchema(), "source_id": uuidSchema(), "member_hash": sha256Schema(),
			"fingerprint": sha256Schema(), exportTotalField: integerSchema(1, bundle.MaxMembers),
			"roles": arraySchema(exportRolePolicySchema(), 8), "expires_at": dateTimeSchema(),
		}), cacheRequired("plan_id", "source_id", "member_hash", "fingerprint", exportTotalField, "roles", "expires_at")...)
}

func exportOutputProblemSchema() schema {
	return objectSchema(schema{
		exportNodeIDField: integerSchema(1, 0), "version_id": uuidSchema(),
		"part_path": stringSchema(4096), "role": stringSchema(64),
		"reason": stringSchema(bundle.MaxMemberBytes),
	}, exportNodeIDField, "version_id", "role", "reason")
}

func listExportOutputProblemsSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"plan_id": uuidSchema(), "after": integerSchema(0, bundle.MaxOutputProblems),
		}, "plan_id"), rootObjectSchema(withPrivateCache(schema{
			"plan_id": uuidSchema(), "fingerprint": sha256Schema(),
			"after":          integerSchema(0, bundle.MaxOutputProblems),
			"next":           integerSchema(0, bundle.MaxOutputProblems),
			exportTotalField: integerSchema(0, bundle.MaxOutputProblems),
			exportItemsField: arraySchema(exportOutputProblemSchema(), bundle.InspectionPageSize),
		}), cacheRequired("plan_id", "fingerprint", "after", "next", exportTotalField, exportItemsField)...)
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
		mcpJobIDField: uuidSchema(), "plan_id": uuidSchema(), "fingerprint": sha256Schema(),
		stateField: stringSchema(32), "sequence": integerSchema(1, 0),
		"receipt": exportReceiptSchema(),
	}), cacheRequired(mcpJobIDField, "plan_id", "fingerprint", stateField, "sequence")...)
}

func startExportJobSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"operation_id": uuidSchema(), "plan_id": uuidSchema(), "fingerprint": sha256Schema(),
	}, "operation_id", "plan_id", "fingerprint"), exportJobResultSchema()
}

func getExportJobSchemas() (schema, schema) {
	return rootObjectSchema(schema{mcpJobIDField: uuidSchema()}, mcpJobIDField), exportJobResultSchema()
}

func cancelExportJobSchemas() (schema, schema) {
	return rootObjectSchema(schema{mcpJobIDField: uuidSchema()}, mcpJobIDField),
		rootObjectSchema(withPrivateCache(schema{
			mcpJobIDField: uuidSchema(), "accepted": booleanSchema(),
		}), cacheRequired(mcpJobIDField, "accepted")...)
}

func openExportArchiveSchemas() (schema, schema) {
	return rootObjectSchema(schema{mcpJobIDField: uuidSchema()}, mcpJobIDField),
		rootObjectSchema(withPrivateCache(schema{
			"handle": stringSchema(64), mcpJobIDField: uuidSchema(),
			"size":   integerSchema(1, maxMCPExportArchiveBytes),
			"sha256": sha256Schema(), "plan_fingerprint": sha256Schema(),
			"expires_at": dateTimeSchema(),
		}), cacheRequired("handle", mcpJobIDField, "size", "sha256", "plan_fingerprint", "expires_at")...)
}

func downloadExportArchiveSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"handle": stringSchema(64), "offset": integerSchema(0, maxMCPExportArchiveBytes),
			"max_bytes": integerSchema(1, maxMCPExportChunkBytes), "close": booleanSchema(),
		}, "handle", "offset", "max_bytes"),
		rootObjectSchema(withPrivateCache(schema{
			"data_base64": stringSchema(350000), "offset": integerSchema(0, maxMCPExportArchiveBytes),
			"next_offset": integerSchema(0, maxMCPExportArchiveBytes),
			"total_bytes": integerSchema(1, maxMCPExportArchiveBytes),
			"sha256":      sha256Schema(), "eof": booleanSchema(), "closed": booleanSchema(),
		}), cacheRequired("data_base64", "offset", "next_offset", "total_bytes", "sha256", "eof", "closed")...)
}
