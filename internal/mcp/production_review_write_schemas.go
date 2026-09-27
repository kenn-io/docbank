package mcp

import "maps"

import "go.kenn.io/docbank/document/redaction"

func productionMutationInputSchema(extra schema, extraRequired ...string) schema {
	properties := schema{
		schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0),
		schemaETagField: integerSchema(1, 0), schemaOperationIDField: uuidSchema(),
	}
	maps.Copy(properties, extra)
	required := append([]string{schemaSetIDField, schemaRevisionField, schemaETagField, schemaOperationIDField}, extraRequired...)
	return rootObjectSchema(properties, required...)
}

func productionReceiptSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		schemaOperationIDField: uuidSchema(), schemaSetIDField: uuidSchema(),
		schemaRevisionField: integerSchema(1, 0), schemaETagField: integerSchema(1, 0),
		"request_sha256": sha256Schema(),
	}), cacheRequired(schemaOperationIDField, schemaSetIDField,
		schemaRevisionField, schemaETagField, "request_sha256")...)
}

func editProductionInstructionsSchemas() (schema, schema) {
	return productionMutationInputSchema(schema{
		"instructions": stringSchema(redaction.MaxInstructionsBytes),
	}, "instructions"), productionReceiptSchema()
}

func sealProductionMembershipSchemas() (schema, schema) {
	return productionMutationInputSchema(schema{
		"total": integerSchema(1, redaction.MaxProductionMembers), "member_hash": sha256Schema(),
	}, "total", "member_hash"), productionReceiptSchema()
}

func reviewProductionMemberSchemas() (schema, schema) {
	return productionMutationInputSchema(schema{
		"member_id": uuidSchema(), "binding": sha256Schema(),
	}, "member_id", "binding"), productionReceiptSchema()
}
