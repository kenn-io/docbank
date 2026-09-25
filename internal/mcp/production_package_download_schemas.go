package mcp

func downloadProductionPackageSchemas() (schema, schema) {
	destination := stringSchema(maxPathCharacters)
	destination["minLength"] = 1
	input := rootObjectSchema(schema{
		schemaJobIDField: uuidSchema(), schemaOperationIDField: uuidSchema(),
		"destination_path": destination,
		"overwrite":        booleanSchema(),
	}, schemaJobIDField, schemaOperationIDField, "destination_path", "overwrite")
	output := rootObjectSchema(withPrivateCache(schema{
		schemaJobIDField: uuidSchema(), schemaOperationIDField: uuidSchema(),
		"destination_path": stringSchema(maxPathCharacters), "version_id": uuidSchema(),
		"archive_sha256": sha256Schema(), "size": integerSchema(1, 0),
		schemaStateField: enumSchema("published", "published_durability_unknown"),
	}), cacheRequired(schemaJobIDField, schemaOperationIDField, "destination_path",
		"version_id", "archive_sha256", "size", schemaStateField)...)
	return input, output
}
