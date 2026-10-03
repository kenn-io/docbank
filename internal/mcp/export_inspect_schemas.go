package mcp

import (
	"math"

	"go.kenn.io/docbank/document/bundle"
)

const schemaTotalField = "total"

func exportRoleSchema() schema {
	return enumSchema(
		"original", "text", "pages", "email_pdf", "attachment_original", "attachment_pdf",
	)
}

func exportSourceSchema() schema {
	return objectSchema(schema{
		"id": uuidSchema(), "request_sha256": sha256Schema(),
		"kind": enumSchema(
			"explicit", "nodes", "query", "saved_query", "snapshot", "upload", "mailbox_collection",
		),
		schemaStateField: enumSchema("sealed"), "member_hash": sha256Schema(),
		schemaTotalField: integerSchema(1, bundle.MaxMembers),
		"source_bytes":   integerSchema(0, bundle.MaxRoleBytes),
		"created_at":     dateTimeSchema(), "expires_at": dateTimeSchema(),
		"saved_query_id": uuidSchema(), "saved_query_revision": integerSchema(1, math.MaxInt64),
		"query_fingerprint": sha256Schema(), "collection_id": uuidSchema(),
	}, "id", "request_sha256", "kind", schemaStateField, "member_hash", schemaTotalField, "source_bytes",
		"created_at", "expires_at")
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
	return objectSchema(schema{
		"format": enumSchema(bundle.Format), "id": uuidSchema(), "vault_id": uuidSchema(),
		"toolchain": stringSchema(bundle.MaxMemberBytes), "source": exportSourceSchema(),
		"roles": roles, "fingerprint": sha256Schema(), schemaTotalField: integerSchema(1, bundle.MaxMembers),
		"document_rows": integerSchema(1, bundle.MaxDocumentRows),
		"volume_limits": objectSchema(schema{
			"role_bytes": integerSchema(1, bundle.MaxVolumeRoleBytes),
			"roles":      integerSchema(1, bundle.MaxVolumeRoles),
		}, "role_bytes", "roles"),
		"volumes":          integerSchema(1, bundle.MaxRoles),
		"duplicate_policy": enumSchema("preserve", "collapse_exact_content"),
		"counts":           exportOutputCountsSchema(), "role_entries": integerSchema(0, bundle.MaxRoles),
		"role_bytes":     integerSchema(0, bundle.MaxRoleBytes),
		"metadata_bytes": integerSchema(0, bundle.MaxMetadataBytes),
		"created_at":     dateTimeSchema(), "expires_at": dateTimeSchema(),
	}, "format", "id", "vault_id", "toolchain", "source", "roles", "fingerprint", schemaTotalField,
		"role_entries", "role_bytes", "metadata_bytes", "created_at", "expires_at")
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
