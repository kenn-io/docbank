package mcp

import (
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/store"
)

func getProductionPrivilegeLogSchemas() (schema, schema) {
	receipt := objectSchema(schema{
		"contract": schema{"type": "string", jsonSchemaConst: documentproduction.PrivilegeLogReceiptContractV1}, //nolint:goconst // JSON Schema type vocabulary is intentionally repeated.
		"log_id":   uuidSchema(), "revision": integerSchema(1, 0),
		"state":                     enumSchema(documentproduction.PrivilegeLogStateFrozen),
		"withheld_selection_sha256": sha256Schema(), "policy_sha256": sha256Schema(),
		"players_sha256": sha256Schema(), "approval_evaluation_sha256": sha256Schema(),
		"rows_sha256": sha256Schema(), "inputs_sha256": sha256Schema(),
		"row_count":    integerSchema(1, documentproduction.MaxPrivilegeRows),
		"validated_at": dateTimeSchema(), "frozen_at": dateTimeSchema(), "sha256": sha256Schema(),
	}, "contract", "log_id", "revision", "state", "withheld_selection_sha256", "policy_sha256",
		"players_sha256", "rows_sha256", "inputs_sha256", "row_count", "validated_at", "frozen_at", "sha256")
	row := objectSchema(schema{
		"id": uuidSchema(), "withheld_member_id": uuidSchema(),
		"family_order":      integerSchema(1, documentproduction.MaxPrivilegeRows),
		"source_version_id": uuidSchema(), "basis": stringSchema(128),
		"public_description": stringSchema(documentproduction.MaxPolicyTextBytes),
	}, "id", "withheld_member_id", "family_order", "source_version_id", "basis", "public_description")
	return rootObjectSchema(schema{
			"log_id": uuidSchema(), "revision": integerSchema(1, 0),
			schemaCursorField: stringSchema(6), schemaLimitField: integerSchema(1, store.MaxProductionPrivilegePage),
		}, "log_id", "revision"),
		rootObjectSchema(withPrivateCache(schema{
			"receipt": receipt, "rows": arraySchema(row, store.MaxProductionPrivilegePage),
			"next_cursor": stringSchema(6),
		}), cacheRequired("receipt", "rows", "next_cursor")...)
}
