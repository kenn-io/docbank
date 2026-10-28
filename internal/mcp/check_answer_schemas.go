package mcp

func checkAnswerSchemas() (schema, schema) {
	citationProperties, citationRequired := textCitationProperties()
	citations := arraySchema(objectSchema(citationProperties, citationRequired...), 8)
	citations["minItems"] = 1
	answer := stringSchema(maxRenditionChars)
	answer["minLength"] = 1
	input := rootObjectSchema(schema{"answer": answer, "citations": citations}, "answer", "citations")
	figure := objectSchema(schema{
		"kind": enumSchema("number", "identifier"), "start": integerSchema(0, maxRenditionChars), "end": integerSchema(1, maxRenditionChars),
		"text": stringSchema(maxRenditionChars), "value": stringSchema(maxRenditionChars + 1),
		"status": enumSchema("found", "missing", "unverifiable"), "found_in": arraySchema(integerSchema(1, 8), 8), "citations": arraySchema(integerSchema(1, 8), 8),
	}, "kind", "start", "end", "text", "value", "status", "found_in", "citations")
	marker := objectSchema(schema{"index": integerSchema(0, 99), "start": integerSchema(0, maxRenditionChars), "end": integerSchema(1, maxRenditionChars)}, "index", "start", "end")
	citation := objectSchema(schema{"index": integerSchema(1, 8), "status": enumSchema("resolved", "citation_unavailable", "invalid_citation_range", "citation_limit", "citation_integrity"), "text_sha256": sha256Schema()}, "index", "status")
	output := rootObjectSchema(withPrivateCache(schema{"status": enumSchema("grounded", "ungrounded", "incomplete", "no_figures"), "citations": arraySchema(citation, 8), "figures": arraySchema(figure, 512), "unknown_markers": arraySchema(marker, 512)}), cacheRequired("status", "citations", "figures", "unknown_markers")...)
	return input, output
}
