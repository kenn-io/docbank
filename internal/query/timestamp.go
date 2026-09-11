package query

import (
	"errors"
	"time"
)

// TimestampKey returns an exact, fixed-width UTC key for SQL time comparisons.
// QueryV1 itself retains its existing canonical timestamp representation.
func TimestampKey(value string) (string, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", errors.New("invalid query timestamp")
	}
	parsed = parsed.UTC()
	if parsed.Year() < 0 || parsed.Year() > 9999 {
		return "", errors.New("query timestamp is outside the four-digit year range")
	}
	return parsed.Format("2006-01-02T15:04:05.000000000Z"), nil
}
