package mcp

import "go.kenn.io/docbank/document/redaction"

func productionSetSchema() schema {
	return objectSchema(schema{
		"id": uuidSchema(), "name": stringSchema(256), "creator": stringSchema(256),
		"created_at": dateTimeSchema(), "head_revision": integerSchema(1, 0),
	}, "id", "name", "creator", "created_at", "head_revision")
}

func productionDraftSchema() schema {
	policy := objectSchema(schema{
		"policy_id": stringSchema(36), "version": integerSchema(0, 0), "policy_sha256": stringSchema(64),
	}, "policy_id", "version", "policy_sha256")
	return objectSchema(schema{
		"set_id": uuidSchema(), "revision": integerSchema(1, 0), "etag": integerSchema(1, 0),
		"instructions_sha256": sha256Schema(), "member_hash": sha256Schema(),
		"decisions_sha256": sha256Schema(), "recipe_id": stringSchema(128), "recipe_sha256": sha256Schema(),
		"profile_id": stringSchema(128), "profile_sha256": sha256Schema(),
		"disclosure_profile_id": stringSchema(128), "disclosure_profile_sha256": sha256Schema(),
		"numbering_recipe_id": stringSchema(128), "numbering_recipe_sha256": sha256Schema(),
		"policy": policy, "state": enumSchema("draft", "finalized"), "membership_sealed": booleanSchema(),
	}, "set_id", "revision", "etag", "instructions_sha256", "member_hash", "decisions_sha256",
		"recipe_id", "recipe_sha256", "profile_id", "profile_sha256", "disclosure_profile_id",
		"disclosure_profile_sha256", "numbering_recipe_id", "numbering_recipe_sha256", "policy",
		"state", "membership_sealed")
}

func listProductionSetsSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"cursor": stringSchema(2048), "limit": integerSchema(1, redaction.MaxProductionPage),
		}), rootObjectSchema(withPrivateCache(schema{
			"items":       arraySchema(productionSetSchema(), redaction.MaxProductionPage), //nolint:goconst // JSON Schema vocabulary is repeated across tool definitions.
			"next_cursor": stringSchema(2048),
		}), cacheRequired("items", "next_cursor")...)
}

func getProductionSetSchemas() (schema, schema) {
	return rootObjectSchema(schema{"set_id": uuidSchema()}, "set_id"),
		rootObjectSchema(withPrivateCache(schema{"set": productionSetSchema()}), cacheRequired("set")...)
}

func getProductionDraftSchemas() (schema, schema) {
	return rootObjectSchema(schema{"set_id": uuidSchema(), "revision": integerSchema(1, 0)}, "set_id", "revision"),
		rootObjectSchema(withPrivateCache(schema{"draft": productionDraftSchema()}), cacheRequired("draft")...)
}
