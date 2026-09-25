package mcp

import "go.kenn.io/docbank/document/redaction"

func createProductionSetSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		"operation_id": uuidSchema(), "name": stringSchema(256),
		"instructions": stringSchema(redaction.MaxInstructionsBytes),
		"recipe_id":    stringSchema(128), "profile_id": stringSchema(128),
		"disclosure_profile_id": stringSchema(128), "numbering_recipe_id": stringSchema(128),
		"policy_id": stringSchema(36), "policy_version": integerSchema(1, 0),
	}, "operation_id", "name")
	output := rootObjectSchema(withPrivateCache(schema{
		"set": productionSetSchema(), "draft": productionDraftSchema(),
	}), cacheRequired("set", "draft")...)
	return input, output
}

func forkProductionDraftSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0),
			"operation_id": uuidSchema(),
		}, schemaSetIDField, schemaRevisionField, "operation_id"),
		rootObjectSchema(withPrivateCache(schema{"draft": productionDraftSchema()}), cacheRequired("draft")...)
}
