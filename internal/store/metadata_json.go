package store

import "strings"

// metadataJSONDialect renders the JSON expressions in retained-receipt checks.
// Expressions and paths are fixed SQL from those checks, never caller input.
type metadataJSONDialect struct{ postgres bool }

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
	const expression = "r.after_json"
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
