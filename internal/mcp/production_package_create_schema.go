package mcp

import "go.kenn.io/docbank/internal/production"

func productionPackageEvidenceSchema() schema {
	return objectSchema(schema{
		"contract": schema{"type": "string", jsonSchemaConst: production.PackageEvidenceContractV1}, //nolint:goconst // JSON Schema vocabulary.
		"id":       uuidSchema(), productionJobIDField: uuidSchema(),
		"production_receipt_sha256": sha256Schema(), "artifact_manifest_sha256": sha256Schema(),
		"recipient_manifest_sha256": sha256Schema(), "archive_sha256": sha256Schema(),
		"qc_sha256": sha256Schema(), "profile_id": stringSchema(128), "sha256": sha256Schema(),
	}, "contract", "id", productionJobIDField, "production_receipt_sha256",
		"artifact_manifest_sha256", "recipient_manifest_sha256", "archive_sha256",
		"qc_sha256", "profile_id", "sha256")
}

func productionPackageEvidenceResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{"evidence": productionPackageEvidenceSchema()}),
		cacheRequired("evidence")...)
}

func getProductionPackageSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		productionJobIDField: uuidSchema(), productionOperationIDField: uuidSchema(),
	}, productionJobIDField, productionOperationIDField), productionPackageEvidenceResultSchema()
}

func createProductionPackageSchemas() (schema, schema) {
	return rootObjectSchema(schema{
		productionJobIDField: uuidSchema(), productionOperationIDField: uuidSchema(),
		"profile_id": stringSchema(128), "max_volume_bytes": integerSchema(1, 50<<30),
		"max_volume_documents": integerSchema(1, 100_000),
	}, productionJobIDField, productionOperationIDField, "profile_id", "max_volume_bytes",
		"max_volume_documents"), productionPackageEvidenceResultSchema()
}
