package mcp

func publishProductionPackageSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		schemaJobIDField: uuidSchema(), schemaOperationIDField: uuidSchema(),
		"profile_id":       enumSchema("export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1"),
		"max_volume_bytes": integerSchema(1, 50<<30), "max_volume_documents": integerSchema(1, 100_000),
	}, "job_id", schemaOperationIDField, "profile_id", "max_volume_bytes", "max_volume_documents")
	output := rootObjectSchema(withPrivateCache(schema{
		schemaJobIDField: uuidSchema(), schemaOperationIDField: uuidSchema(),
		"profile_id": enumSchema("export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1"),
		"version_id": uuidSchema(), "archive_sha256": sha256Schema(),
		"evidence_sha256": sha256Schema(), "size": integerSchema(1, 0),
	}), cacheRequired("job_id", schemaOperationIDField, "profile_id", "version_id",
		"archive_sha256", "evidence_sha256", "size")...)
	return input, output
}
