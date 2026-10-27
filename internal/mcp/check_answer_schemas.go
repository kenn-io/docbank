package mcp

import "math"

func checkAnswerSchemas() (schema, schema) {
	citationProperties, citationRequired := textCitationProperties()
	citations := arraySchema(objectSchema(citationProperties, citationRequired...), 8)
	citations["minItems"] = 1
	answer := stringSchema(maxRenditionChars)
	answer["minLength"] = 1
	input := rootObjectSchema(schema{"answer": answer, "citations": citations}, "answer", "citations")
	indexes := arraySchema(integerSchema(1, math.MaxInt64), 512)
	figure := objectSchema(schema{
		"kind": enumSchema("number", "identifier"), "start": integerSchema(0, maxRenditionChars), "end": integerSchema(1, maxRenditionChars),
		"text": stringSchema(maxRenditionChars), "values": arraySchema(stringSchema(maxRenditionChars+1), 512),
		"status": enumSchema("found", "missing", "unverifiable"), "found_in": indexes,
	}, "kind", "start", "end", "text", "values", "status", "found_in")
	claim := objectSchema(schema{
		"start": integerSchema(0, maxRenditionChars), "end": integerSchema(1, maxRenditionChars),
		"citations": indexes, "unknown_markers": arraySchema(integerSchema(0, math.MaxInt64), 512), "verdict": enumSchema("grounded", "ungrounded", "unverifiable", "no_figures"),
		"figures": arraySchema(figure, 512),
	}, "start", "end", "citations", "unknown_markers", "verdict", "figures")
	citation := objectSchema(schema{"index": integerSchema(1, 8), "status": enumSchema("resolved", "citation_unavailable", "invalid_citation_range", "citation_limit", "citation_integrity", "citation_timeout", "citation_failed"), "text_sha256": sha256Schema()}, "index", "status")
	output := rootObjectSchema(withPrivateCache(schema{"status": enumSchema("grounded", "ungrounded", "incomplete"), "citations": arraySchema(citation, 8), "claims": arraySchema(claim, 256)}), cacheRequired("status", "citations", "claims")...)
	return input, output
}
