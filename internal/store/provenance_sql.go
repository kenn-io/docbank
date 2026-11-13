package store

// provenanceSQLDialect keeps accepted provenance strings lossless. SQLite
// stores TEXT; PostgreSQL uses BYTEA because TEXT cannot contain NUL.
type provenanceSQLDialect struct{ postgres bool }

// storedString binds an accepted UTF-8 string to a PostgreSQL BYTEA column.
// SQLite keeps its released TEXT storage; readers scan both back into strings.
func storedString(postgres bool, value string) any {
	if postgres {
		return []byte(value)
	}
	return value
}

// Only the ASCII prefix determines whether a kind is caller-supplied. Encoding
// those nine bytes avoids decoding arbitrary NUL-containing evidence as TEXT.
// Expressions are fixed SQL from the caller, never user input.
func (d provenanceSQLDialect) embedded(expression string) string {
	if d.postgres {
		return "LOWER(encode(substring(" + expression + ",1,9),'escape'))='embedded:'"
	}
	return "LOWER(" + expression + ") LIKE 'embedded:%'"
}

func storedOptionalString(postgres bool, value *string) any {
	if value == nil {
		return nil
	}
	return storedString(postgres, *value)
}

// Nullable evidence stays absent, while present strings retain every byte.
func storedNullableString(postgres bool, value string) any {
	if value == "" {
		return nil
	}
	return storedString(postgres, value)
}

// storedStringSQL joins a format-constrained text key to an opaque byte key.
// Expressions are fixed SQL; caller values use storedString instead.
func storedStringSQL(postgres bool, expression string) string {
	if postgres {
		return "convert_to(" + expression + ",'UTF8')"
	}
	return expression
}
