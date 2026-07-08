package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseAge parses Go durations plus a day suffix: "30d" = 30*24h. Empty
// means zero (everything). Negative ages are rejected: a future cutoff
// would silently delete the entire trash.
func ParseAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if base, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(base)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q (want e.g. 30d or 12h): %w", s, err)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q (want e.g. 30d or 12h): %w", s, err)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid age %q: must not be negative", s)
	}
	return d, nil
}
