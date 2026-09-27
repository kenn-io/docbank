package mcp

func createProductionPlayersSnapshotSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	return rootObjectSchema(schema{
			schemaSnapshotIDField: uuidSchema(), "revision": integerSchema(1, 0),
			productionOperationIDField: uuidSchema(), "players_file": path,
		}, schemaSnapshotIDField, "revision", productionOperationIDField, "players_file"),
		rootObjectSchema(withPrivateCache(schema{
			schemaSnapshotIDField: uuidSchema(), "revision": integerSchema(1, 0),
			"sha256": sha256Schema(), "player_count": integerSchema(1, 0),
		}), cacheRequired(schemaSnapshotIDField, "revision", "sha256", "player_count")...)
}
