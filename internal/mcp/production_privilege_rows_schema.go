package mcp

const (
	productionPrivilegeLogIDField    = "log_id"
	productionPrivilegeRevisionField = "revision"
)

func replaceProductionPrivilegeRowsSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			productionOperationIDField: uuidSchema(), "expected_generation": integerSchema(1, 0),
			"rows_file": path,
		}, productionPrivilegeLogIDField, productionPrivilegeRevisionField, productionOperationIDField, "expected_generation", "rows_file"),
		rootObjectSchema(withPrivateCache(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"generation": integerSchema(1, 0),
		}), cacheRequired(productionPrivilegeLogIDField, productionPrivilegeRevisionField, "generation")...)
}
