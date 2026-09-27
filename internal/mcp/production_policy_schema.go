package mcp

import (
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/store"
)

const productionOperationIDField = "operation_id"

func productionPolicySchema(withDigest bool) schema {
	predicate := objectSchema(schema{
		"field": stringSchema(256), "operator": enumSchema("equals", "one_of", "present", "date_between"),
		"values": arraySchema(stringSchema(1024), documentproduction.MaxPolicyValues),
		"from":   stringSchema(10), "through": stringSchema(10),
	}, "field", "operator")
	rule := objectSchema(schema{
		"id": stringSchema(128), "kind": enumSchema("scope", "date", "family", "disposition", "label"),
		"predicate": predicate, "disposition": enumSchema("produce", "withhold"),
		"family_mode": enumSchema("member", "complete_family"), "required_label": stringSchema(256),
	}, "id", "kind", "predicate")
	properties := schema{
		"contract": schema{"type": "string", jsonSchemaConst: documentproduction.PolicyContractV1}, //nolint:goconst // JSON Schema type vocabulary is intentionally repeated.
		"id":       uuidSchema(), "version": integerSchema(1, 0), schemaNameField: stringSchema(256),
		productionCreatedAtField: dateTimeSchema(), "rules": schema{"type": "array", "items": rule, //nolint:goconst // JSON Schema type vocabulary is intentionally repeated.
			"minItems": 1, "maxItems": documentproduction.MaxPolicyRules},
		"output": objectSchema(schema{
			"confidentiality_label": stringSchema(256), "endorsement_kind": stringSchema(64),
			"endorsement_text": stringSchema(documentproduction.MaxPolicyTextBytes),
		}),
		"approval": objectSchema(schema{
			"required": booleanSchema(), "evidence_required": booleanSchema(),
			"max_age_seconds": integerSchema(0, 365*24*60*60),
		}),
		"privilege_log": objectSchema(schema{
			"required": booleanSchema(), "require_frozen_receipt": booleanSchema(),
			"required_fields": arraySchema(stringSchema(128), documentproduction.MaxPrivilegeFields),
			"allowed_bases":   arraySchema(stringSchema(128), documentproduction.MaxPrivilegeFields),
		}),
		"conflict_mode": schema{"type": "string", jsonSchemaConst: documentproduction.PolicyConflictReject},
	}
	required := []string{"contract", "id", "version", schemaNameField, productionCreatedAtField, "rules", "conflict_mode"}
	if withDigest {
		properties["sha256"] = sha256Schema()
		required = append(required, "output", "approval", "privilege_log", "sha256")
	}
	return objectSchema(properties, required...)
}

func productionPolicyResultSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{
		"policy": productionPolicySchema(true),
	}), cacheRequired("policy")...)
}

func createProductionPolicySchemas() (schema, schema) {
	return rootObjectSchema(schema{
		productionOperationIDField: uuidSchema(), "policy": productionPolicySchema(false),
	}, productionOperationIDField, "policy"), productionPolicyResultSchema()
}

func getProductionPolicySchemas() (schema, schema) {
	return rootObjectSchema(schema{
		"policy_id": uuidSchema(), "version": integerSchema(1, 0),
	}, "policy_id", "version"), productionPolicyResultSchema()
}

func listProductionPoliciesSchemas() (schema, schema) {
	summary := objectSchema(schema{
		"id": uuidSchema(), "version": integerSchema(1, 0), schemaNameField: stringSchema(256),
		"sha256": sha256Schema(),
	}, "id", "version", schemaNameField, "sha256")
	return rootObjectSchema(schema{
			schemaCursorField: stringSchema(60), schemaLimitField: integerSchema(1, store.MaxProductionPolicyPage),
		}), rootObjectSchema(withPrivateCache(schema{
			"items": arraySchema(summary, store.MaxProductionPolicyPage), "next_cursor": stringSchema(60),
		}), cacheRequired("items", "next_cursor")...)
}
