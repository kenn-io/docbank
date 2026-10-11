package mcp

import "math"

//nolint:goconst // Citation wire fields remain beside their closed schemas.
func textCitationSchemas() (schema, schema) {
	properties := schema{
		"version": integerSchema(1, 1), "vault_uid": uuidSchema(),
		"node_id":            integerSchema(1, math.MaxInt64),
		"content_version_id": uuidSchema(), "content_sha256": sha256Schema(),
		"rendition_attachment_id": sha256Schema(), "build_id": sha256Schema(),
		"rendition_sha256": sha256Schema(), "start": integerSchema(0, math.MaxInt32),
		"end": integerSchema(1, math.MaxInt32),
	}
	required := []string{"version", "vault_uid", "node_id", "content_version_id", "content_sha256",
		"rendition_attachment_id", "build_id", "rendition_sha256", "start", "end"}
	text := stringSchema(maxRenditionChars)
	text["minLength"] = 1
	return rootObjectSchema(properties, required...), rootObjectSchema(withPrivateCache(schema{
		"citation": objectSchema(properties, required...), "text": text,
		"text_sha256": sha256Schema(), "text_bytes": integerSchema(1, 4*maxRenditionChars),
	}), cacheRequired("citation", "text", "text_sha256", "text_bytes")...)
}
