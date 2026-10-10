package mcp

import (
	"math"

	"go.kenn.io/docbank/document/bundle"
)

const schemaTotalField = "total"

func exportRoleSchema() schema {
	return enumSchema(
		"original", "text", "pages", "email_pdf", "attachment_original", "attachment_pdf", "photo_rendered",
	)
}

func exportSourceSchema() schema {
	queryFingerprint := stringSchema(71)
	queryFingerprint["pattern"] = "^sha256:[0-9a-f]{64}$"
	return objectSchema(schema{
		"id": uuidSchema(), "request_sha256": sha256Schema(),
		"kind": enumSchema(
			"explicit", "nodes", "query", "saved_query", "snapshot", "upload", "mailbox_collection", "photos",
		),
		schemaStateField: enumSchema("sealed"), "member_hash": sha256Schema(),
		schemaTotalField: integerSchema(1, bundle.MaxMembers),
		"source_bytes":   integerSchema(0, bundle.MaxRoleBytes),
		"created_at":     dateTimeSchema(), "expires_at": dateTimeSchema(),
		"saved_query_id": uuidSchema(), "saved_query_revision": integerSchema(1, math.MaxInt64),
		"query_fingerprint": queryFingerprint, "collection_id": uuidSchema(),
	}, "id", "request_sha256", "kind", schemaStateField, "member_hash", schemaTotalField,
		"source_bytes", "created_at", "expires_at")
}

func exportOutputCountsSchema() schema {
	return objectSchema(schema{
		"messages": integerSchema(0, 0), "attachments": integerSchema(0, 0),
		"email_pdfs": integerSchema(0, 0), "attachment_pdfs": integerSchema(0, 0),
		"pages": integerSchema(0, 0), "collapsed": integerSchema(0, 0),
		"unavailable": integerSchema(0, 0), "unavailable_inventories": integerSchema(0, 0),
	}, "messages", "attachments", "email_pdfs", "attachment_pdfs", "pages", "collapsed",
		"unavailable", "unavailable_inventories")
}

func retainedExportPlanSchema() schema {
	roles := arraySchema(objectSchema(schema{
		"role": exportRoleSchema(), "allow_unavailable": booleanSchema(),
		"profile_fingerprint": sha256Schema(), "recipe_sha256": sha256Schema(),
	}, "role"), 6)
	roles["minItems"] = 1
	return exportPlanSchema(schema{
		"toolchain": stringSchema(bundle.MaxMemberBytes), "source": exportSourceSchema(),
		"roles": roles, schemaTotalField: integerSchema(1, bundle.MaxMembers),
		"document_rows": integerSchema(1, bundle.MaxDocumentRows),
		"photo_render": objectSchema(schema{
			"format": enumSchema("jpeg", "png"), "quality": integerSchema(0, 100),
			"long_edge": integerSchema(0, 100000), "include_metadata": booleanSchema(),
			"remove_gps": booleanSchema(),
		}, "format", "quality", "long_edge", "include_metadata", "remove_gps"),
		"embedded_previews": integerSchema(0, bundle.MaxPhotoExportMembers),
		"volume_limits": objectSchema(schema{
			"role_bytes": integerSchema(1, bundle.MaxVolumeRoleBytes),
			"roles":      integerSchema(1, bundle.MaxVolumeRoles),
		}, "role_bytes", "roles"),
		"volumes":          integerSchema(1, bundle.MaxRoles),
		"duplicate_policy": enumSchema("preserve", "collapse_exact_content"),
	})
}

func getExportPlanSchemas() (schema, schema) {
	return rootObjectSchema(schema{"plan_id": uuidSchema()}, "plan_id"),
		rootObjectSchema(withPrivateCache(schema{"plan": retainedExportPlanSchema()}),
			cacheRequired("plan")...)
}

//nolint:goconst // Bundle wire field names remain beside their schemas.
func getExportProblemsSchemas() (schema, schema) {
	problem := objectSchema(schema{
		"node_id": integerSchema(1, math.MaxInt64), "version_id": uuidSchema(),
		"role": exportRoleSchema(), "reason": stringSchema(bundle.MaxMemberBytes),
		"part_path": stringSchema(bundle.MaxMemberBytes),
	}, "node_id", "version_id", "role", "reason")
	page := objectSchema(schema{
		"plan_id": uuidSchema(), "fingerprint": sha256Schema(),
		"after":          integerSchema(0, bundle.MaxOutputProblems),
		"next":           integerSchema(0, bundle.MaxOutputProblems),
		schemaTotalField: integerSchema(0, bundle.MaxOutputProblems),
		"items":          arraySchema(problem, bundle.InspectionPageSize),
	}, "plan_id", "fingerprint", "after", "next", schemaTotalField, "items")
	return rootObjectSchema(schema{
			"plan_id": uuidSchema(), "after": integerSchema(0, bundle.MaxOutputProblems),
		}, "plan_id"),
		rootObjectSchema(withPrivateCache(schema{"problems": page}), cacheRequired("problems")...)
}
