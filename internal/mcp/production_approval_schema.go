package mcp

import documentproduction "go.kenn.io/docbank/document/production"

func getProductionApprovalSchemas() (schema, schema) {
	grant := objectSchema(schema{
		"id": uuidSchema(), "subject_sha256": sha256Schema(), "actor": stringSchema(256),
		"granted_at": dateTimeSchema(), "expires_at": dateTimeSchema(), "sha256": sha256Schema(),
	}, "id", "subject_sha256", "actor", "granted_at", "sha256")
	event := objectSchema(schema{
		"id": uuidSchema(), "approval_id": uuidSchema(),
		"kind":         enumSchema(documentproduction.ApprovalEventRevoke, documentproduction.ApprovalEventSupersede),
		"effective_at": dateTimeSchema(), "replacement_approval_id": uuidSchema(),
	}, "id", "approval_id", "kind", "effective_at")
	return rootObjectSchema(schema{"approval_id": uuidSchema()}, "approval_id"),
		rootObjectSchema(withPrivateCache(schema{
			"grant": grant, "events": arraySchema(event, 10000),
		}), cacheRequired("grant", "events")...)
}
