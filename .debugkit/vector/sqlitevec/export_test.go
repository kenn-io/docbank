package sqlitevec

// QueryGenerationSQLForTest builds the exact statement QueryGeneration
// executes plus its bind arguments, so external tests can EXPLAIN its query
// plan against a real database.
func (s *Store[K, G]) QueryGenerationSQLForTest(
	ordinal int64, query []float32, limit int,
) (string, []any, error) {
	expr, value, err := vectorValue(query)
	if err != nil {
		return "", nil, err
	}
	return s.queryGenerationSQL(ordinal, expr), []any{value, limit, ordinal}, nil
}
