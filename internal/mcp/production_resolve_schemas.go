package mcp

import "go.kenn.io/docbank/document/redaction"

func listProductionRecipesSchemas() (schema, schema) {
	dpi := integerSchema(300, 600)
	dpi["enum"] = []int{300, 600}
	recipe := objectSchema(schema{"id": stringSchema(128), "sha256": sha256Schema(),
		"dpi": dpi}, "id", "sha256", "dpi")
	return rootObjectSchema(schema{}), rootObjectSchema(withPrivateCache(schema{
		"default_id": stringSchema(128), "items": arraySchema(recipe, 2), //nolint:goconst // JSON Schema vocabulary is repeated across tools.
	}), cacheRequired("default_id", "items")...)
}

func resolveProductionSelectionSchemas() (schema, schema) {
	input := rootObjectSchema(schema{
		schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0), "etag": integerSchema(1, 0),
		"member_id": uuidSchema(), "page": integerSchema(1, 0),
		schemaCursorField: stringSchema(1024), schemaLimitField: integerSchema(1, redaction.MaxProductionPage),
	}, "set_id", "revision", "etag", "member_id", "page")
	page := objectSchema(schema{
		"number": integerSchema(1, 0), "frame_sha256": sha256Schema(),
		"width": integerSchema(1, 0), "height": integerSchema(1, 0),
	}, "number", "frame_sha256", "width", "height")
	box := objectSchema(schema{
		"page": integerSchema(1, 0), "frame_sha256": sha256Schema(),
		"x0": integerSchema(0, 0), "y0": integerSchema(0, 0),
		"x1": integerSchema(1, 0), "y1": integerSchema(1, 0),
	}, "page", "frame_sha256", "x0", "y0", "x1", "y1")
	output := rootObjectSchema(withPrivateCache(schema{
		schemaSetIDField: uuidSchema(), schemaRevisionField: integerSchema(1, 0), "etag": integerSchema(1, 0),
		"member_id": uuidSchema(), "page": page, "map_sha256": sha256Schema(),
		"recipe_sha256": sha256Schema(), "resolved_sha256": sha256Schema(),
		"review_binding": sha256Schema(), "total_boxes": integerSchema(0, 0),
		"items":               arraySchema(box, redaction.MaxProductionPage),
		schemaNextCursorField: stringSchema(1024),
	}), cacheRequired(schemaSetIDField, schemaRevisionField, "etag", "member_id", "page", "map_sha256",
		"recipe_sha256", "resolved_sha256", "review_binding", "total_boxes", "items", schemaNextCursorField)...)
	return input, output
}
