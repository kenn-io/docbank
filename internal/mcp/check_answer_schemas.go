package mcp

func checkAnswerSchemas() (schema, schema) {
	citations := arraySchema(textCitationObjectSchema(), 8)
	citations["minItems"] = 1
	answer := stringSchema(maxRenditionChars)
	answer["minLength"] = 1
	input := rootObjectSchema(schema{"answer": answer, "citations": citations}, "answer", "citations")
	figure := objectSchema(schema{"text": stringSchema(maxRenditionChars), "status": enumSchema("found", "missing")}, "text", "status")
	citation := objectSchema(schema{"index": integerSchema(1, 8), "status": enumSchema("resolved", "citation_unavailable"), "text_sha256": sha256Schema()}, "index", "status")
	output := rootObjectSchema(withPrivateCache(schema{"status": enumSchema("grounded", "ungrounded", "incomplete", "no_figures"), "citations": arraySchema(citation, 8), "figures": arraySchema(figure, 512), "unknown_markers": arraySchema(integerSchema(0, 999), 512)}), cacheRequired("status", "citations", "figures", "unknown_markers")...)
	return input, output
}
