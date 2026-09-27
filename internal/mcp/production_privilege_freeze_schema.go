package mcp

func freezeProductionPrivilegeLogSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			productionOperationIDField: uuidSchema(), "expected_generation": integerSchema(1, 0),
			"expected_inputs_sha256":              sha256Schema(),
			"expected_approval_evaluation_sha256": sha256Schema(),
			"frozen_at":                           productionPrivilegeValidatedAtSchema(),
		}, productionPrivilegeLogIDField, productionPrivilegeRevisionField, productionOperationIDField, "expected_generation",
			"expected_inputs_sha256", "frozen_at"),
		rootObjectSchema(withPrivateCache(schema{
			productionPrivilegeLogIDField: uuidSchema(), productionPrivilegeRevisionField: integerSchema(1, 0),
			"row_count": integerSchema(1, 0), "receipt_sha256": sha256Schema(),
			"inputs_sha256": sha256Schema(), "frozen_at": productionPrivilegeValidatedAtSchema(),
		}), cacheRequired(productionPrivilegeLogIDField, productionPrivilegeRevisionField, "row_count",
			"receipt_sha256", "inputs_sha256", "frozen_at")...)
}
