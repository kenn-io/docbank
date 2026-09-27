package mcp

func downloadProductionPackageSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			"job_id": uuidSchema(), productionOperationIDField: uuidSchema(),
			"destination_path": path, "overwrite": booleanSchema(),
		}, "job_id", productionOperationIDField, "destination_path", "overwrite"),
		rootObjectSchema(withPrivateCache(schema{
			"job_id": uuidSchema(), productionOperationIDField: uuidSchema(),
			"destination_path": stringSchema(maxPathCharacters),
			"archive_sha256":   sha256Schema(), "version_id": uuidSchema(),
			"size":           integerSchema(1, 0),
			schemaStateField: enumSchema("published", "published_durability_unknown"),
		}), cacheRequired("job_id", productionOperationIDField, "destination_path", "archive_sha256",
			"version_id", "size", schemaStateField)...)
}
