package mcp

func finalizeProductionDraftSchemas() (schema, schema) {
	input := productionMutationInputSchema(schema{
		"namespace_id": uuidSchema(), "snapshot_id": uuidSchema(), "start_at": integerSchema(0, 0),
	}, "namespace_id", "snapshot_id")
	output := rootObjectSchema(withPrivateCache(schema{
		"draft": productionDraftSchema(), schemaOperationIDField: uuidSchema(),
		"namespace_id": uuidSchema(), "snapshot_id": uuidSchema(),
		"prepared_sha256": sha256Schema(), "receipt_sha256": sha256Schema(),
	}), cacheRequired("draft", schemaOperationIDField, "namespace_id", "snapshot_id",
		"prepared_sha256", "receipt_sha256")...)
	return input, output
}

func admitProductionJobSchemas() (schema, schema) {
	input := productionMutationInputSchema(schema{"job_id": uuidSchema()}, "job_id")
	_, output := getProductionJobSchemas()
	return input, output
}

func cancelProductionJobSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		schemaSetIDField: uuidSchema(), "job_id": uuidSchema(),
		schemaETagField: integerSchema(1, 0), schemaOperationIDField: uuidSchema(),
	}, schemaSetIDField, "job_id", schemaETagField, schemaOperationIDField)
	return input, productionReceiptSchema()
}
