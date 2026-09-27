package mcp

func createProductionWithheldSelectionSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			"set_id": uuidSchema(), "revision": integerSchema(1, 0), "selection_file": path,
		}, "set_id", "revision", "selection_file"),
		rootObjectSchema(withPrivateCache(schema{
			"selection_id": uuidSchema(), "set_id": uuidSchema(), "revision": integerSchema(1, 0),
			"policy_sha256": sha256Schema(), "sha256": sha256Schema(), "member_count": integerSchema(1, 0),
		}), cacheRequired("selection_id", "set_id", "revision", "policy_sha256", "sha256", "member_count")...)
}
