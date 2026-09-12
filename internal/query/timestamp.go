package query

// TimestampKey returns an exact, fixed-width UTC key for SQL time comparisons.
// It uses the same strict timestamp rules as QueryV1 filters.
func TimestampKey(value string) (string, error) {
	parsed, err := parseTimestamp(value, "query timestamp")
	if err != nil {
		return "", err
	}
	return parsed.Format("2006-01-02T15:04:05.000000000Z"), nil
}
