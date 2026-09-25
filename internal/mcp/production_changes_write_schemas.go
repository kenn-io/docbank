package mcp

import "go.kenn.io/docbank/document/redaction"

func appendProductionMembersSchemas() (schema, schema) {
	return productionMutationInputSchema(schema{
		"members_json": stringSchema(redaction.MaxCommandBytes),
	}, "members_json"), productionReceiptSchema()
}

func applyProductionChangesSchemas() (schema, schema) {
	return productionMutationInputSchema(schema{
		"changes_json": stringSchema(redaction.MaxCommandBytes),
	}, "changes_json"), productionReceiptSchema()
}
