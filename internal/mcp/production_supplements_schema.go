package mcp

import "go.kenn.io/docbank/internal/production"

const productionSupplementCreatedAtField = "created_at"

func productionSupplementRecordSchema() schema {
	return objectSchema(schema{
		"contract":                 schema{"type": "string", jsonSchemaConst: production.SupplementRecordContractV1}, //nolint:goconst // JSON Schema vocabulary.
		productionOperationIDField: uuidSchema(),
		"parent_job_id":            uuidSchema(), "job_id": uuidSchema(),
		"parent_receipt_sha256": sha256Schema(), "prepared_sha256": sha256Schema(),
		"prepared_input_sha256": sha256Schema(), "request_sha256": sha256Schema(),
		"set_id": uuidSchema(), "revision": integerSchema(1, 0),
		"namespace_id": uuidSchema(), "parent_allocation_id": uuidSchema(),
		"allocation_id": uuidSchema(), "number_reservation_sha256": sha256Schema(),
		"parent_end_sequence": integerSchema(1, 0),
		"start_sequence":      integerSchema(1, 0), "end_sequence": integerSchema(1, 0),
		productionSupplementCreatedAtField: dateTimeSchema(), "sha256": sha256Schema(),
	}, "contract", productionOperationIDField, "parent_job_id", "job_id",
		"parent_receipt_sha256", "prepared_sha256", "prepared_input_sha256", "request_sha256",
		"set_id", "revision", "namespace_id", "parent_allocation_id", "allocation_id",
		"number_reservation_sha256", "parent_end_sequence", "start_sequence", "end_sequence",
		productionSupplementCreatedAtField, "sha256")
}

func productionSupplementResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{"record": productionSupplementRecordSchema()}),
		cacheRequired("record")...)
}

func createProductionSupplementSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		productionOperationIDField: uuidSchema(),
		"parent_job_id":            uuidSchema(), "job_id": uuidSchema(),
		"parent_receipt_sha256": sha256Schema(), "prepared_sha256": sha256Schema(),
		"prepared_input_sha256": sha256Schema(),
	}, productionOperationIDField, "parent_job_id", "job_id", "parent_receipt_sha256",
		"prepared_sha256", "prepared_input_sha256"), productionSupplementResultSchema()
}

func getProductionSupplementSchemas() (schema, schema) {
	return rootObjectSchema(schema{productionOperationIDField: uuidSchema()}, productionOperationIDField),
		productionSupplementResultSchema()
}
