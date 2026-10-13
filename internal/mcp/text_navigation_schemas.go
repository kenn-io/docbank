package mcp

import (
	"math"

	"go.kenn.io/docbank/document"
)

func textCitationObjectSchema() schema {
	properties, required := textCitationProperties()
	return objectSchema(properties, required...)
}

func textSectionRangeSchema() schema {
	return objectSchema(schema{"start": integerSchema(0, math.MaxInt32),
		"end": integerSchema(1, math.MaxInt32)}, "start", "end")
}

func textOutlineSchemas() (schema, schema) {
	entry := objectSchema(schema{
		"start": integerSchema(0, math.MaxInt32), "end": integerSchema(1, math.MaxInt32),
		"level": integerSchema(0, 6), "title_markdown": stringSchema(document.MaxTextOutlineTitleChars),
		"title_truncated": booleanSchema(),
	}, "start", "end", "level", "title_markdown", "title_truncated")
	return rootObjectSchema(schema{
			"citation": textCitationObjectSchema(),
			"offset":   integerSchema(0, document.MaxTextNavigationHeadings+1),
		}, "citation", "offset"), rootObjectSchema(withPrivateCache(schema{
			"citation": textCitationObjectSchema(), "document_chars": integerSchema(1, math.MaxInt32),
			"enclosing_section": textSectionRangeSchema(),
			"offset":            integerSchema(0, document.MaxTextNavigationHeadings+1),
			schemaTotalField:    integerSchema(1, document.MaxTextNavigationHeadings+1),
			"sections":          arraySchema(entry, document.TextOutlinePageSize),
			"next_offset":       integerSchema(1, document.MaxTextNavigationHeadings),
		}), cacheRequired("citation", "document_chars", "enclosing_section",
			"offset", schemaTotalField, "sections")...)
}

func textSectionSchemas() (schema, schema) {
	return rootObjectSchema(schema{
			"citation": textCitationObjectSchema(), "section": textSectionRangeSchema(),
			"offset":    integerSchema(0, math.MaxInt32),
			"max_chars": integerSchema(1, document.MaxTextSectionChars),
		}, "citation", "section", "offset", "max_chars"), rootObjectSchema(withPrivateCache(schema{
			"citation": textCitationObjectSchema(), "section": textSectionRangeSchema(),
			"offset": integerSchema(0, math.MaxInt32), "actual_start": integerSchema(0, math.MaxInt32),
			"actual_end": integerSchema(0, math.MaxInt32), "next_offset": integerSchema(0, math.MaxInt32),
			"complete": booleanSchema(), "text": stringSchema(document.MaxTextSectionChars),
			"text_sha256": sha256Schema(), "text_bytes": integerSchema(0, 4*document.MaxTextSectionChars),
		}), cacheRequired("citation", "section", "offset", "actual_start", "actual_end",
			"next_offset", "complete", "text", "text_sha256", "text_bytes")...)
}
