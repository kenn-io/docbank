package mcp

import "go.kenn.io/docbank/internal/processing"

func recipientPackageSchemas() (schema, schema) {
	path := stringSchema(maxPathCharacters)
	path["minLength"] = 1
	file := objectSchema(schema{"path": stringSchema(256), schemaSizeField: integerSchema(0, processing.MaxRecipientPackageBytes), "sha256": sha256Schema()}, "path", schemaSizeField, "sha256")
	profile := objectSchema(schema{
		"id": stringSchema(64), "field": integerSchema(0, 0x10ffff), "qualifier": integerSchema(0, 0x10ffff), "newline_in_field": integerSchema(0, 0x10ffff),
		"header_row": booleanSchema(), "columns": arraySchema(stringSchema(128), 64), "encoding": stringSchema(64), "date_format": stringSchema(128), "declared_timezone": stringSchema(128),
	}, "id", "field", "qualifier", "newline_in_field", "header_row", "columns", "encoding", "date_format", "declared_timezone")
	document := objectSchema(schema{
		"ordinal": integerSchema(1, 16), "document_id": stringSchema(256), "allocation_id": uuidSchema(), "namespace_id": uuidSchema(),
		"artifact_manifest_sha256": sha256Schema(), "recipe_sha256": sha256Schema(), "review_sha256": sha256Schema(),
		"page_count": integerSchema(1, 16), "labels": arraySchema(stringSchema(256), 16), "pdf": file, "text": file,
	}, "ordinal", "document_id", "allocation_id", "namespace_id", "artifact_manifest_sha256", "recipe_sha256", "review_sha256", "page_count", "labels", "pdf", "text")
	receipt := objectSchema(schema{
		"contract": enumSchema("native-pdf-recipient-package/v1"), "document_count": integerSchema(1, 16), "page_count": integerSchema(1, 64),
		"dat_profile": profile, "opt_profile": profile, "load_file": file, "page_map": file, "mapping": file, "documents": arraySchema(document, 16),
	}, "contract", "document_count", "page_count", "dat_profile", "opt_profile", "load_file", "page_map", "mapping", "documents")
	return rootObjectSchema(schema{"selection_path": path, destinationPathField: path}, "selection_path", destinationPathField), rootObjectSchema(withPrivateCache(schema{
		schemaStateField: enumSchema("published", "published_durability_unknown"), destinationPathField: stringSchema(maxPathCharacters), schemaSizeField: integerSchema(1, processing.MaxRecipientPackageBytes),
		"sha256": sha256Schema(), "receipt_sha256": sha256Schema(), "verification": enumSchema("masks_text_and_numbers"), "receipt": receipt, "cleanup_failed": booleanSchema(),
	}), cacheRequired(schemaStateField, destinationPathField, schemaSizeField, "sha256", "receipt_sha256", "verification", "receipt", "cleanup_failed")...)
}
