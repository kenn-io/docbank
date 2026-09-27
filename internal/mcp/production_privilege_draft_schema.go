package mcp

func createProductionPrivilegeDraftSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			"log_id": uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"operation_id": uuidSchema(), "set_id": uuidSchema(), "set_revision": integerSchema(1, 0),
			"players_sha256": sha256Schema(), "rows_file": path,
			"predecessor_log_id": uuidSchema(), "predecessor_receipt_sha256": sha256Schema(),
		}, "log_id", productionPrivilegeRevisionField, "operation_id", "set_id", "set_revision", "players_sha256", "rows_file"),
		rootObjectSchema(withPrivateCache(schema{
			"log_id": uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"generation": integerSchema(1, 0),
		}), cacheRequired("log_id", productionPrivilegeRevisionField, "generation")...)
}
