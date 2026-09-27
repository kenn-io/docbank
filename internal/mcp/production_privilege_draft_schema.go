package mcp

func createProductionPrivilegeDraftSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			productionOperationIDField: uuidSchema(), "set_id": uuidSchema(), "set_revision": integerSchema(1, 0),
			"players_sha256": sha256Schema(), "rows_file": path,
			"predecessor_log_id": uuidSchema(), "predecessor_receipt_sha256": sha256Schema(),
		}, productionPrivilegeLogIDField, productionPrivilegeRevisionField, productionOperationIDField, "set_id", "set_revision", "players_sha256", "rows_file"),
		rootObjectSchema(withPrivateCache(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"generation": integerSchema(1, 0),
		}), cacheRequired(productionPrivilegeLogIDField, productionPrivilegeRevisionField, "generation")...)
}
