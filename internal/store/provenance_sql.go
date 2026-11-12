package store

// provenanceSQLDialect keeps accepted provenance strings lossless. SQLite
// stores TEXT; PostgreSQL uses BYTEA because TEXT cannot contain NUL.
type provenanceSQLDialect struct{ postgres bool }

func (d provenanceSQLDialect) value(value string) any {
	if d.postgres {
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
