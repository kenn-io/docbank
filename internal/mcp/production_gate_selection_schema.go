package mcp

func selectProductionGateAuthoritySchemas() (schema, schema) {
	fields := schema{
		"set_id": uuidSchema(), "revision": integerSchema(1, 0),
		"approval_id": uuidSchema(), "privilege_log_id": uuidSchema(),
		"privilege_log_revision": integerSchema(1, 0),
	}
	return rootObjectSchema(fields, "set_id", "revision"),
		rootObjectSchema(withPrivateCache(fields), cacheRequired("set_id", "revision")...)
}
