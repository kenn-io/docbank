package store

import "strings"

// metadataJSONDialect renders the JSON expressions in retained-receipt checks.
// Expressions and paths are fixed SQL from those checks, never caller input.
type metadataJSONDialect struct{ postgres bool }

// Receipt lookups explicitly select their backend's derived query document.
func (d metadataJSONDialect) receiptColumn(alias string) string {
	column := "after_json"
	if d.postgres {
		column = "query_json"
	}
	if alias != "" {
		return alias + "." + column
	}
	return column
}

func (d metadataJSONDialect) text(expression, path string) string {
	if d.postgres {
		return "(" + expression + "::jsonb #>> '{" + strings.ReplaceAll(path, ".", ",") + "}')"
	}
	return "json_extract(" + expression + ",'$." + path + "')"
}

// indexedText matches the receipt indexes' casts, JSON operators and collation.
func (d metadataJSONDialect) indexedText(expression, path string) string {
	if !d.postgres {
		return "CAST(" + d.text(expression, path) + " AS TEXT)"
	}
	if !strings.Contains(path, ".") {
		return "(" + expression + "::jsonb ->> '" + path + "') COLLATE \"C\""
	}
	return d.text(expression, path) + ` COLLATE "C"`
}

func (d metadataJSONDialect) number(expression, path string) string {
	return "CAST(" + d.text(expression, path) + " AS BIGINT)"
}

func (d metadataJSONDialect) receiptArray(path string) string {
	expression := d.receiptColumn("r")
	if d.postgres {
		return "jsonb_array_elements(" + expression + "::jsonb #> '{" + strings.ReplaceAll(path, ".", ",") + "}')"
	}
	return "json_each(" + expression + ",'$." + path + "')"
}

func (d metadataJSONDialect) kind(expression, path string) string {
	if d.postgres {
		return "jsonb_typeof(" + expression + "::jsonb #> '{" + strings.ReplaceAll(path, ".", ",") + "}')"
	}
	return "json_type(" + expression + ",'$." + path + "')"
}

func (d metadataJSONDialect) maximum() string {
	if d.postgres {
		return "GREATEST"
	}
	return "MAX"
}

// arrayValues expands a JSON array parameter. Callers use [] for an empty set.
func (d metadataJSONDialect) arrayValues(parameter string) string {
	if d.postgres {
		return "jsonb_array_elements_text(CAST(" + parameter + " AS jsonb)) AS items(value)"
	}
	return "json_each(" + parameter + ")"
}

// originalFileWaiter excludes jobs that disclose a derived photo artifact.
func (d metadataJSONDialect) originalFileWaiter() string {
	if d.postgres {
		return `NOT EXISTS(SELECT 1 FROM json_array_elements(w.input_classes_json::json) AS items(value) WHERE value::text='"derived_upload"')`
	}
	return "NOT EXISTS(SELECT 1 FROM " + d.arrayValues("w.input_classes_json") + " WHERE value='derived_upload')"
}

// byteText reads hex-encoded request identifiers; persisted JSON stays unchanged.
func (d metadataJSONDialect) byteText(path string) string {
	text := d.text("value", path)
	if d.postgres {
		return "decode(" + text + ",'hex')"
	}
	return text
}
