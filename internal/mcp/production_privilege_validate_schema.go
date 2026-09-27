package mcp

func productionPrivilegeValidatedAtSchema() schema {
	value := dateTimeSchema()
	value["maxLength"] = 30
	value["pattern"] = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`
	return value
}

func validateProductionPrivilegeLogSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"operation_id": uuidSchema(), "expected_generation": integerSchema(1, 0),
			"validated_at": productionPrivilegeValidatedAtSchema(),
		}, productionPrivilegeLogIDField, productionPrivilegeRevisionField, "operation_id", "expected_generation", "validated_at"),
		rootObjectSchema(withPrivateCache(schema{
			"draft_generation": integerSchema(1, 0), "inputs_sha256": sha256Schema(),
			"rows_sha256": sha256Schema(), "validated_at": productionPrivilegeValidatedAtSchema(),
		}), cacheRequired("draft_generation", "inputs_sha256", "rows_sha256", "validated_at")...)
}
