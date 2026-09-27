package mcp

import documentproduction "go.kenn.io/docbank/document/production"

const productionJobIDField = "job_id"

func productionReproductionReceiptSchema() schema {
	return objectSchema(schema{
		"contract":                           schema{"type": "string", jsonSchemaConst: documentproduction.ReproductionReceiptContractV1}, //nolint:goconst // JSON Schema vocabulary.
		"id":                                 uuidSchema(),
		"original_production_receipt_sha256": sha256Schema(),
		"original_number_reservation_sha256": sha256Schema(),
		"artifact_manifest_sha256":           sha256Schema(),
		"package_qc_sha256":                  sha256Schema(),
		"delivery_policy_sha256":             sha256Schema(),
		"number_allocation_count":            schema{"type": "integer", jsonSchemaConst: 0},
		productionCreatedAtField:             dateTimeSchema(), "sha256": sha256Schema(),
	}, "contract", "id", "original_production_receipt_sha256",
		"original_number_reservation_sha256", "artifact_manifest_sha256", "package_qc_sha256",
		"delivery_policy_sha256", "number_allocation_count", productionCreatedAtField, "sha256")
}

func productionReproductionResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"receipt": productionReproductionReceiptSchema(),
	}), cacheRequired("receipt")...)
}

func getProductionReproductionSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		productionJobIDField: uuidSchema(), "operation_id": uuidSchema(),
	}, productionJobIDField, "operation_id"), productionReproductionResultSchema()
}

func createProductionReproductionSchemas() (schema, schema) {
	artifactIDs := arraySchema(uuidSchema(), documentproduction.MaxArtifacts)
	artifactIDs["minItems"] = 1
	sourceVersionIDs := arraySchema(uuidSchema(), documentproduction.MaxArtifacts)
	sourceVersionIDs["minItems"] = 1
	methods := arraySchema(enumSchema("secure-transfer", "offline-media"), 2)
	methods["minItems"] = 1
	request := objectSchema(schema{
		"contract":                           schema{"type": "string", jsonSchemaConst: documentproduction.ReproductionRequestContractV1},
		"operation_id":                       uuidSchema(),
		"original_production_receipt_sha256": sha256Schema(),
		"artifact_ids":                       artifactIDs, "source_version_ids": sourceVersionIDs,
		"delivery_policy_sha256": sha256Schema(),
	}, "contract", "operation_id", "original_production_receipt_sha256",
		"artifact_ids", "source_version_ids")
	policy := objectSchema(schema{
		"recipient_code": stringSchema(128), "allowed_methods": methods,
	}, "recipient_code", "allowed_methods")
	return rootObjectSchema(schema{
		productionJobIDField: uuidSchema(), "request": request, "delivery_policy": policy,
		"profile_id": stringSchema(128), "max_volume_bytes": integerSchema(1, 50<<30),
		"max_volume_documents": integerSchema(1, 100_000),
	}, productionJobIDField, "request", "delivery_policy", "profile_id", "max_volume_bytes",
		"max_volume_documents"), productionReproductionResultSchema()
}
