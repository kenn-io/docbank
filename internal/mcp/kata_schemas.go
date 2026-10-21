//nolint:goconst // Keep Kata wire field names beside their closed schemas.
package mcp

import (
	"math"

	"go.kenn.io/docbank/internal/kataevidence"
)

const kataRequiredField = "required"

func kataIssueRefSchema() schema {
	result := stringSchema(256)
	result["minLength"] = 1
	return result
}

func kataIssueReceiptSchema() schema {
	uid := stringSchema(128)
	uid["minLength"] = 1
	ref := stringSchema(257)
	ref["minLength"] = 1
	return objectSchema(schema{"uid": uid, "qualified_ref": ref, "title": stringSchema(512)}, "uid", "qualified_ref", "title")
}

func kataReferenceSchema() schema {
	document := objectSchema(schema{
		"vault_uid": uuidSchema(), "node_id": integerSchema(1, math.MaxInt64),
		"content_version_id": uuidSchema(), "content_sha256": sha256Schema(),
		"start_rune": integerSchema(0, math.MaxInt32), "end_rune": integerSchema(1, math.MaxInt32),
	}, "vault_uid", "node_id", "content_version_id", "content_sha256", "start_rune", "end_rune")
	citation, required := textCitationProperties()
	// Future versions may retain familiar fields; incomplete references are omitted.
	reference := objectSchema(schema{
		"version": integerSchema(1, math.MaxInt32), "kind": enumSchema("docbank"),
		"docbank": document, "rendition": objectSchema(citation, required...),
	}, "version", "kind")
	reference["oneOf"] = []schema{
		{kataRequiredField: []string{"docbank"}, "not": schema{kataRequiredField: []string{"rendition"}}},
		{kataRequiredField: []string{"rendition"}, "not": schema{kataRequiredField: []string{"docbank"}}},
	}
	return reference
}

func getKataContextSchemas() (schema, schema) {
	evidence := objectSchema(schema{
		"id": sha256Schema(), "reference": kataReferenceSchema(),
		"excerpt": stringSchema(kataevidence.MaxChars), "label": stringSchema(512), "content_trust": enumSchema("untrusted"),
	}, "id", "reference", "excerpt", "label", "content_trust")
	passage := objectSchema(schema{
		schemaStateField: enumSchema("available", "changed", "unavailable", "unreachable", "unsupported", "failed"),
		"evidence":       evidence, "saved_quote": stringSchema(kataevidence.MaxChars),
	}, schemaStateField)
	passage["if"] = schema{"properties": schema{schemaStateField: enumSchema("unsupported")}}
	passage["then"] = schema{}
	passage["else"] = schema{kataRequiredField: []string{"evidence"}}
	return rootObjectSchema(schema{"issue": kataIssueRefSchema(), "offset": integerSchema(0, kataevidence.MaxContextOffset)}, "issue"),
		rootObjectSchema(withPrivateCache(schema{
			"issue": kataIssueReceiptSchema(), "passages": arraySchema(passage, kataevidence.ContextPageSize),
			"next_offset": integerSchema(1, kataevidence.MaxContextOffset),
		}), cacheRequired("issue", "passages")...)
}

func citeKataEvidenceSchemas() (schema, schema) {
	citation, required := textCitationProperties()
	selector := objectSchema(schema{
		"node_id": integerSchema(1, math.MaxInt64), "start_rune": integerSchema(0, math.MaxInt32), "end_rune": integerSchema(1, math.MaxInt32),
		"expected_version": uuidSchema(), "quote": stringSchema(kataevidence.MaxChars), "citation": objectSchema(citation, required...),
	}, "node_id", "start_rune", "end_rune")
	title := stringSchema(512)
	title["minLength"] = 1
	key := stringSchema(128)
	key["minLength"] = 1
	input := rootObjectSchema(schema{
		"selector": selector, "title": title, "brief": stringSchema(2000), "idempotency_key": key, "issue": kataIssueRefSchema(),
	}, "selector")
	input["oneOf"] = []schema{
		{kataRequiredField: []string{"title", "idempotency_key"}, "not": schema{kataRequiredField: []string{"issue"}}},
		{kataRequiredField: []string{"issue"}, "not": schema{"anyOf": []schema{{kataRequiredField: []string{"title"}}, {kataRequiredField: []string{"brief"}}, {kataRequiredField: []string{"idempotency_key"}}}}},
	}
	return input, rootObjectSchema(withPrivateCache(schema{"issue": kataIssueReceiptSchema()}), cacheRequired("issue")...)
}
