package mcp

const productionPrivilegeRevisionField = "revision"

func replaceProductionPrivilegeRowsSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			"log_id": uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"operation_id": uuidSchema(), "expected_generation": integerSchema(1, 0),
			"rows_file": path,
		}, "log_id", productionPrivilegeRevisionField, "operation_id", "expected_generation", "rows_file"),
		rootObjectSchema(withPrivateCache(schema{
			"log_id": uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"generation": integerSchema(1, 0),
		}), cacheRequired("log_id", productionPrivilegeRevisionField, "generation")...)
}
