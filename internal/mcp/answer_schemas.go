package mcp

import "maps"

const (
	answerVaultIDField = "vault_id"
	jsonSchemaRequired = "required"
)

func answerQuestionSchemas() (schema, schema) {
	input := fenceInputProperties()
	input["question"] = schema{"type": "string", "minLength": 1, "maxLength": 8192}                                //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
	input["profile"] = schema{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[a-z][a-z0-9_-]*$"} //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
	input["binding_id"] = schema{"type": "string", "minLength": 1, "maxLength": 128}
	input["limit"] = integerSchema(1, 50)
	input["rerank"] = booleanSchema()
	input["max_passage_bytes"] = integerSchema(2048, 65536)
	in := rootObjectSchema(input, "question")
	in["if"] = schema{jsonSchemaRequired: []string{"content_version_ids"}}
	in["then"] = schema{"not": schema{jsonSchemaRequired: []string{"filters"}}}
	common := schema{"node_id": integerSchema(1, 0), versionField: uuidSchema(), "path": schema{"type": "string", "minLength": 1, "maxLength": maxPathCharacters}, "rank": integerSchema(1, 50), "evidence_ids": arraySchema(stringSchema(1024), 64), "relevance": schema{"type": "number"}} //nolint:goconst // JSON Schema vocabulary is intentionally repeated.
	passage := schema{}
	maps.Copy(passage, common)
	passage["text"] = stringSchema(512)
	passage["text_bytes"] = integerSchema(0, 2048)
	citation := objectSchema(schema{"node_id": integerSchema(1, 0), versionField: uuidSchema()}, "node_id", versionField)
	coverage := objectSchema(schema{"binding_required": booleanSchema(), "scoped_documents": integerSchema(0, 4096), "complete_documents": integerSchema(0, 4096), schemaStateField: stringSchema(64)}, "binding_required", "scoped_documents", "complete_documents", schemaStateField)
	receipt := objectSchema(schema{
		"question_sha256": sha256Schema(), "executed_query_sha256": sha256Schema(), "search_executed": booleanSchema(), "observed_scope_count": integerSchema(0, 0), answerVaultIDField: uuidSchema(), "fence": fenceAuthoritySchema(), "fence_fingerprint": stringSchema(71),
		"profile": objectSchema(schema{"name": stringSchema(128), "fingerprint": sha256Schema()}, "name", "fingerprint"), "binding_id": stringSchema(128), "vector_space_ids": arraySchema(stringSchema(1024), 3200), "requested_mode": stringSchema(64), "actual_mode": stringSchema(64), "degradations": arraySchema(stringSchema(64), 64), "coverage": coverage,
		"reranking": objectSchema(schema{"requested": booleanSchema(), "outcome": stringSchema(64), "candidate_count": integerSchema(0, 1000)}, "requested", "outcome", "candidate_count"),
		"gates":     arraySchema(objectSchema(schema{"id": stringSchema(128), "version": stringSchema(128), "outcome": stringSchema(128), "dropped": integerSchema(0, 50)}, "id", "version", "outcome", "dropped"), 16), "cited_versions": arraySchema(citation, 50),
	}, "question_sha256", "search_executed", "observed_scope_count", "vector_space_ids", "degradations", "gates", "cited_versions")
	output := rootObjectSchema(withPrivateCache(schema{"status": enumSchema("ANSWERED", "NO_MATCH", "INCOMPLETE_COVERAGE", "OUTDATED_ONLY", "CONFLICTING"), "coverage_complete": booleanSchema(), "coverage_reasons": arraySchema(enumSchema("scope_too_large", "semantic_unavailable", "embeddings_incomplete", "passage_text_unavailable", "retrieval_truncated"), 8), "semantic_failure": stringSchema(64), "passages": arraySchema(objectSchema(passage, "node_id", versionField, "path", "rank", "text", "text_bytes", "evidence_ids"), 50), "passage_bytes": integerSchema(0, 65536), "omitted_passages": integerSchema(0, 50), "leads": arraySchema(objectSchema(common, "node_id", versionField, "path", "rank", "evidence_ids"), 20), "omitted_leads": integerSchema(0, 50), "omitted_links": integerSchema(0, 3200), "receipt": receipt}), cacheRequired("status", "coverage_complete", "coverage_reasons", "passages", "passage_bytes", "omitted_passages", "leads", "omitted_leads", "omitted_links", "receipt")...)
	return in, output
}
