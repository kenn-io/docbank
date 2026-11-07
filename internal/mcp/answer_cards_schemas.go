package mcp

import "go.kenn.io/docbank/internal/answercard"

func matchAnswerCardsSchemas() (schema, schema) {
	const nodeField, versionField, typeField = "node_id", "content_version_id", "type"
	question := stringSchema(300)
	question["minLength"] = 1
	root := stringSchema(maxPathCharacters)
	root["pattern"] = "^/"
	citation, required := textCitationProperties()
	citation["quote"] = stringSchema(answercard.MaxBytes)
	required = append(required, "quote")
	match := objectSchema(schema{
		"card":     objectSchema(schema{nodeField: integerSchema(1, 0), versionField: uuidSchema(), "path": stringSchema(maxPathCharacters)}, nodeField, versionField, "path"),
		"question": question, "matched_phrasing": question, "score": schema{typeField: "number", "minimum": answercard.MatchThreshold, "maximum": 1},
		"answer": stringSchema(4000), "citations": arraySchema(objectSchema(citation, required...), 16),
	}, "card", "question", "matched_phrasing", "score", "answer", "citations")
	excluded := objectSchema(schema{"stale": integerSchema(0, answercard.MaxFiles), "draft": integerSchema(0, answercard.MaxFiles), "invalid": integerSchema(0, answercard.MaxFiles), "not_a_card": integerSchema(0, answercard.MaxFiles)}, "stale", "draft", "invalid", "not_a_card")
	return rootObjectSchema(schema{"question": question, "root": root, schemaLimitField: integerSchema(1, 10)}, "question"), rootObjectSchema(withPrivateCache(schema{
		"status": enumSchema("matched", "no_match"), "matches": arraySchema(match, 10), "excluded": excluded,
		"stale_cards": arraySchema(objectSchema(schema{"path": stringSchema(maxPathCharacters), "reason": stringSchema(answercard.MaxStaleReasonChars)}, "path", "reason"), 10),
	}), cacheRequired("status", "matches", "excluded", "stale_cards")...)
}
