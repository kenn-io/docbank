package mcp

import "go.kenn.io/docbank/document/redaction"

func productionMemberSummarySchema() schema {
	return objectSchema(schema{
		"id": uuidSchema(), "ordinal": integerSchema(1, redaction.MaxProductionMembers),
		"source_version_id": uuidSchema(), "mode": enumSchema("redact_selected", "keep_selected"),
		"map_sha256": sha256Schema(), "reviewed": booleanSchema(), "review_binding": stringSchema(64),
	}, "id", "ordinal", "source_version_id", "mode", "map_sha256", "reviewed", "review_binding")
}

func productionDecisionSummarySchema() schema {
	return objectSchema(schema{
		"id": uuidSchema(), "member_id": uuidSchema(), "action": enumSchema("keep", "redact"),
		"reason": stringSchema(redaction.MaxDecisionReasonBytes), "label": stringSchema(redaction.MaxDecisionLabelBytes),
		"uncertain": booleanSchema(), "selector_kind": enumSchema("text", "rectangle", "paragraph", "email_message", "transcript_turn", "page"),
		"map_sha256": sha256Schema(),
	}, "id", "member_id", "action", "reason", "label", "uncertain", "selector_kind", "map_sha256")
}

func listProductionMembersSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0),
			schemaCursorField: stringSchema(4096), "limit": integerSchema(1, redaction.MaxProductionPage),
		}, "set_id", "revision"), rootObjectSchema(withPrivateCache(schema{
			"items":               arraySchema(productionMemberSummarySchema(), redaction.MaxProductionPage), //nolint:goconst // JSON Schema vocabulary is repeated across tools.
			schemaNextCursorField: stringSchema(4096),
		}), cacheRequired("items", schemaNextCursorField)...)
}

func listProductionDecisionsSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0),
			schemaCursorField: stringSchema(2048), "limit": integerSchema(1, 100),
			"uncertain": enumSchema("all", "true", "false"),
		}, "set_id", "revision"), rootObjectSchema(withPrivateCache(schema{
			"items":               arraySchema(productionDecisionSummarySchema(), 100),
			schemaNextCursorField: stringSchema(2048),
		}), cacheRequired("items", schemaNextCursorField)...)
}

func getProductionJobSchemas() (schema, schema) {
	job := objectSchema(schema{
		schemaJobIDField: uuidSchema(), schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0),
		"state":           enumSchema("queued", "running", "failed", "canceled", "succeeded"),
		"revision_sha256": sha256Schema(), "receipt_sha256": sha256Schema(),
	}, "job_id", "set_id", "revision", "state", "revision_sha256")
	return rootObjectSchema(schema{schemaSetIDField: uuidSchema(), schemaJobIDField: uuidSchema()}, "set_id", "job_id"),
		rootObjectSchema(withPrivateCache(schema{"job": job}), cacheRequired("job")...)
}
